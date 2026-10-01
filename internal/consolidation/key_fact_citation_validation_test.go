package consolidation

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// TestRunDaily_DropsKeyFactCitationNotAmongRealSourceEpisodes is the
// real regression test for review finding B16: a key fact's
// source_episode_ids was never validated for referential existence,
// unlike upsertRelationships' own entityExists check for relationship
// endpoints — a hallucinated or stale episode-id citation on an
// otherwise-correctly-grounded fact was stored permanently unchecked,
// since a text[] column can't carry a real foreign key.
//
// Seeds exactly one real episode, then has the consolidation LLM (via
// fakeConsolidationProvider's canned response) cite both that real
// episode and a second id that was never actually one of this day's
// sources — simulating a hallucinated citation. The stored
// source_episode_ids must contain only the real one.
func TestRunDaily_DropsKeyFactCitationNotAmongRealSourceEpisodes(t *testing.T) {
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	keys := crypto.NewKeyStore(db, make([]byte, 32))

	consolidationJSON := `{
		"summary": "Discussed the HUPI project.",
		"key_facts": [{"fact": "Decided to use pgvector", "source_episode_ids": ["ep_test_citation_real", "ep_test_citation_hallucinated"]}],
		"entities_touched": []
	}`
	groundingJSON := `{"grounded": [true]}`
	consolidationProvider := fakeConsolidationProvider{response: consolidationJSON}
	groundingProvider := fakeConsolidationProvider{response: groundingJSON}
	runner := New(db, keys, consolidationProvider, groundingProvider, consolidationProvider)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-key-fact-citation"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what should we use for the vector index?")
	outputCT, _ := enc.Encrypt("let's use pgvector")
	date := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	// Only ep_test_citation_real is seeded — ep_test_citation_hallucinated
	// (cited above) was never a real source episode for this day at all.
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_citation_real', $1, 'interaction', $2, $3, 'sha256:test-citation', 0.5, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var summaryID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-09-11' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryID)
	}); err != nil {
		t.Fatalf("expected a daily summary to be written: %v", err)
	}

	var sourceEpisodeIDsLit string
	if err := db.QueryRowContext(ctx, `select source_episode_ids from summary_key_facts where summary_id = $1`, summaryID).Scan(&sourceEpisodeIDsLit); err != nil {
		t.Fatalf("expected a key_facts row: %v", err)
	}
	got := pgfmt.ParseTextArray(sourceEpisodeIDsLit)

	if len(got) != 1 || got[0] != "ep_test_citation_real" {
		t.Errorf("source_episode_ids = %v, want exactly [\"ep_test_citation_real\"] — the hallucinated citation must be dropped, not stored unchecked", got)
	}
}
