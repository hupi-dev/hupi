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
	"hupi/internal/provider"
)

// TestGenerateDailySummary_InsurancePassRunsWhenClusteringEmbedFails is
// the real regression test for review finding B15: generateDailySummary
// only ever called the per-episode insurance pass (extractPerEpisodeFacts)
// after clustering *succeeded* with more than one cluster — a busy day
// (above clusterEpisodeThreshold, so clustering is actually attempted)
// whose clustering embed call fails fell back to a bare single-pass
// summary with no insurance pass at all, reintroducing the exact
// pre-clustering dilution behavior on precisely the days this whole
// mechanism exists to protect.
//
// Seeds clusterEpisodeThreshold+1 episodes for one day (enough to
// attempt clustering) with an embedder that always fails Embed — the
// exact fallback branch this finding is about. Counts real
// ChatCompletion calls via fakeConsolidationProvider's own
// capturedRequests: 1 for the main single-pass summary, plus exactly
// one more per episode if (and only if) the insurance pass actually ran.
func TestGenerateDailySummary_InsurancePassRunsWhenClusteringEmbedFails(t *testing.T) {
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

	consolidationJSON := `{"summary": "A busy day.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	var captured []provider.ChatRequest
	consolidationProvider := fakeConsolidationProvider{response: consolidationJSON, capturedRequests: &captured}
	groundingProvider := fakeConsolidationProvider{response: groundingJSON}
	runner := New(db, keys, consolidationProvider, groundingProvider, failingEmbedder{})

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-insurance-fallback"}
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
	date := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	const numEpisodes = clusterEpisodeThreshold + 1
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for i := 0; i < numEpisodes; i++ {
			inputCT, _ := enc.Encrypt("a busy day exchange")
			outputCT, _ := enc.Encrypt("a busy day reply")
			if _, err := tx.ExecContext(ctx, `
				insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
				values ($1, $2, 'interaction', $3, $4, $5, 0.5, $6, $7)
			`, idFor(i), date, inputCT, outputCT, hashFor(i), scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %d episodes: %v", numEpisodes, err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	wantCalls := 1 + numEpisodes // 1 main single-pass summary + 1 insurance call per episode
	if len(captured) != wantCalls {
		t.Errorf("ChatCompletion was called %d times, want %d (1 single-pass summary + %d insurance-pass calls, one per episode) — the insurance pass must still run when clustering's own embed call fails",
			len(captured), wantCalls, numEpisodes)
	}
}

func idFor(i int) string   { return "ep_test_insurance_fallback_" + string(rune('a'+i)) }
func hashFor(i int) string { return "sha256:test-insurance-" + string(rune('a'+i)) }
