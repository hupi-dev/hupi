package consolidation

import (
	"context"
	"database/sql"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestCorrect_ConcurrentCorrectionsOfSameTargetDoNotFork is the real
// regression test for review finding B14: Correct's "not already
// superseded" guard and its actual write (storeSummary) are separate
// transactions with real work in between (reloading sources, a real
// grounding-check call) — two correctors racing the same not-yet-
// superseded target (an operator's manual correction racing the
// automatic contradiction detector, or two different triggering runs)
// could both pass the guard and both successfully insert a row
// superseding it, forking history with no principled way for retrieval
// to pick between the two.
//
// Many goroutines call Correct simultaneously (released off one barrier
// channel to maximize real overlap) against the same freshly-seeded,
// not-yet-superseded summary. Exactly one must succeed; every other call
// must fail with a real error (the new unique index's constraint
// violation), not silently also succeed. The final row count for this
// period must be exactly 2 (the original plus the one winning
// correction) — a fork would show 3 or more.
func TestCorrect_ConcurrentCorrectionsOfSameTargetDoNotFork(t *testing.T) {
	consolidationJSON := `{"summary": "unused", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-correct-concurrent-fork"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	v1CT, _ := enc.Encrypt("v1: the original, not yet corrected")
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ('sum_test_concurrent_fork_v1', '2026-09-08', 'daily', $1, $2, $3)
		`, v1CT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}

	const n = 10
	type result struct{ err error }
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			output := ConsolidationOutput{Summary: "a concurrent correction attempt"}
			err := runner.Correct(ctx, scope, "sum_test_concurrent_fork_v1", output, "concurrent correction race test", "operator-concurrent", "")
			results <- result{err: err}
		}()
	}
	close(start) // release every goroutine at once, maximizing real overlap

	successes := 0
	for i := 0; i < n; i++ {
		res := <-results
		if res.err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 — the rest must fail with a real error (the unique index), not silently also succeed and fork history", successes)
	}

	var count int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where period = '2026-09-08' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	}); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if count != 2 {
		t.Errorf("summaries for this period = %d, want exactly 2 (the original v1 plus the one winning correction) — more than 2 means history forked", count)
	}
}
