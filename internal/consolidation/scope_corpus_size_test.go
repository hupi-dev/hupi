package consolidation

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestUpdateScopeCorpusSize_InsertsThenUpserts is the direct regression
// test for the upsert itself — internal/store/retrieve.go's
// keywordSearchTierForScope reads this table, so a second call for the
// same scope must overwrite the old counts, not add a second row or leave
// the row stale.
func TestUpdateScopeCorpusSize_InsertsThenUpserts(t *testing.T) {
	runner, db := testRunner(t, "{}", "{}")
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-scope-corpus-size"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from scope_corpus_size where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	seedEpisode := func(id string) {
		inputCT, _ := enc.Encrypt("input")
		outputCT, _ := enc.Encrypt("output")
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
				values ($1, $2, 'interaction', $3, $4, 'sha256:test', 0.5, $5, $6)
			`, id, time.Now(), inputCT, outputCT, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			t.Fatalf("seed episode %s: %v", id, err)
		}
	}
	seedSummary := func(id, period string) {
		summaryCT, _ := enc.Encrypt("summary")
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				insert into summaries (id, period, level, status, summary, grounding_checked, scope_kind, scope_owner, key_version)
				values ($1, $2, 'daily', 'draft', $3, true, $4, $5, 1)
			`, id, period, summaryCT, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			t.Fatalf("seed summary %s: %v", id, err)
		}
	}

	readCounts := func() (episodeCount, summaryCount int) {
		t.Helper()
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `
				select episode_count, summary_count from scope_corpus_size
				where scope_kind = $1 and scope_owner = $2
			`, scope.Kind, scope.Owner).Scan(&episodeCount, &summaryCount)
		})
		if err != nil {
			t.Fatalf("read scope_corpus_size: %v", err)
		}
		return episodeCount, summaryCount
	}

	seedEpisode("ep_corpus_size_1")
	seedSummary("sum_corpus_size_1", "2026-01-01")

	if err := runner.updateScopeCorpusSize(ctx, scope); err != nil {
		t.Fatalf("updateScopeCorpusSize (first call): %v", err)
	}
	gotEpisodes, gotSummaries := readCounts()
	if gotEpisodes != 1 || gotSummaries != 1 {
		t.Errorf("after first call: episode_count=%d summary_count=%d, want 1/1", gotEpisodes, gotSummaries)
	}

	seedEpisode("ep_corpus_size_2")
	seedSummary("sum_corpus_size_2", "2026-01-02")

	if err := runner.updateScopeCorpusSize(ctx, scope); err != nil {
		t.Fatalf("updateScopeCorpusSize (second call): %v", err)
	}
	gotEpisodes, gotSummaries = readCounts()
	if gotEpisodes != 2 || gotSummaries != 2 {
		t.Errorf("after second call: episode_count=%d summary_count=%d, want 2/2 (on-conflict update, not a second row)", gotEpisodes, gotSummaries)
	}
}

// TestRunDaily_UpdatesScopeCorpusSize confirms RunDaily actually wires
// updateScopeCorpusSize in (not just that the function works in
// isolation) — a day with real episodes must leave scope_corpus_size
// current by the time RunDaily returns.
func TestRunDaily_UpdatesScopeCorpusSize(t *testing.T) {
	consolidationJSON := `{
		"summary": "Discussed the HUPI project.",
		"key_facts": [],
		"entities_touched": []
	}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-daily-corpus-size"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from scope_corpus_size where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what should we use for the vector index?")
	outputCT, _ := enc.Encrypt("let's use pgvector")
	date := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_run_daily_corpus_size', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var episodeCount, summaryCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select episode_count, summary_count from scope_corpus_size
			where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&episodeCount, &summaryCount)
	})
	if err != nil {
		t.Fatalf("expected RunDaily to have written scope_corpus_size: %v", err)
	}
	if episodeCount != 1 {
		t.Errorf("episode_count = %d, want 1", episodeCount)
	}
	if summaryCount != 1 {
		t.Errorf("summary_count = %d, want 1", summaryCount)
	}
}
