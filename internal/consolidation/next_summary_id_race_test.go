package consolidation

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestNextSummaryID_ConcurrentCallsNeverCollide is the real regression
// test for review finding C5: nextSummaryID reads the current max
// version and computes +1 inside the caller's own transaction, but two
// concurrent transactions for the same scope+level+period under READ
// COMMITTED could both read the same maxVersion before either commits —
// both would then try to insert the identical id. summaries.id's
// primary key already catches that deterministically (a clean
// unique-violation error, not silent corruption), but it was still a
// real, avoidable failure for whichever transaction lost the race.
//
// Many goroutines each open their own real transaction, call
// nextSummaryID, and insert a row with the id it returns, released off
// one barrier channel to maximize real overlap. With the new advisory
// lock, every single one must succeed with a distinct version number —
// not "exactly one wins" (this isn't a correctness-via-rejection case
// like B14's unique index; every write here is legitimate and should
// succeed, just serialized).
func TestNextSummaryID_ConcurrentCallsNeverCollide(t *testing.T) {
	consolidationJSON := `{"summary": "unused", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-next-summary-id-race"}
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
	summaryCT, err := enc.Encrypt("concurrent test summary")
	if err != nil {
		t.Fatalf("encrypt test summary: %v", err)
	}

	const n = 10
	type result struct {
		id  string
		err error
	}
	results := make(chan result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			var id string
			err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
				var err error
				id, err = runner.nextSummaryID(ctx, tx, scope, "daily", "2026-09-10")
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `
					insert into summaries (id, period, level, summary, scope_kind, scope_owner)
					values ($1, '2026-09-10', 'daily', $2, $3, $4)
				`, id, summaryCT, scope.Kind, scope.Owner)
				return err
			})
			results <- result{id: id, err: err}
		}()
	}
	close(start)

	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		res := <-results
		if res.err != nil {
			t.Errorf("concurrent nextSummaryID+insert failed: %v", res.err)
			continue
		}
		if seen[res.id] {
			t.Errorf("duplicate id %q returned by concurrent nextSummaryID calls", res.id)
		}
		seen[res.id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct ids, want %d — every concurrent call should succeed with a unique version", len(seen), n)
	}
	for v := 1; v <= n; v++ {
		want := fmt.Sprintf("sum_%s_2026-09-10_daily_v%d", scope.Owner, v)
		if !seen[want] {
			t.Errorf("missing expected version id %q — versions should be exactly 1..%d with no gaps or duplicates", want, n)
		}
	}
}
