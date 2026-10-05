package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// insertSummaryStub seeds a bare summaries row at the given level/period —
// just enough for exhaustiveKeyFactsForEntity's own join (on s.id,
// filtering s.level and "current"), mirroring
// summary_key_facts_rls_test.go's insertSummaryWithKeyFact but with a
// controllable level/period so these tests can exercise the
// daily-vs-rollup distinction directly.
func insertSummaryStub(t *testing.T, s *Store, scope identity.Scope, summaryID, level, period, supersedes string) {
	t.Helper()
	err := dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		var superseding sql.NullString
		if supersedes != "" {
			superseding = sql.NullString{String: supersedes, Valid: true}
		}
		_, err := tx.Exec(`
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, supersedes)
			values ($1, $2, $3, 'test summary', $4, $5, $6)
		`, summaryID, period, level, scope.Kind, scope.Owner, superseding)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", summaryID, err)
	}
}

// insertKeyFact (retrieve_test.go) already seeds one real,
// correctly-encrypted summary_key_facts row — reused directly rather than
// redefined here.

// TestExhaustiveKeyFactsForEntity_ReturnsEveryGroundedDailyOccurrence is
// the core, real behavior docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md's
// Attempt 2 exists for: every grounded daily key_fact mentioning the
// entity's name, across every day in scope, not just a bounded top-K
// relevance sample — and nothing mentioning a different entity.
func TestExhaustiveKeyFactsForEntity_ReturnsEveryGroundedDailyOccurrence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-counting-a"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryStub(t, s, scope, "summary:counting-a-1", "daily", "2023-01-01", "")
	insertKeyFact(t, s, scope, "summary:counting-a-1", "Nate won a CS:GO tournament on 2023-01-01.", true)

	insertSummaryStub(t, s, scope, "summary:counting-a-2", "daily", "2023-03-15", "")
	insertKeyFact(t, s, scope, "summary:counting-a-2", "Nate won a Street Fighter tournament on 2023-03-15.", true)

	insertSummaryStub(t, s, scope, "summary:counting-a-3", "daily", "2023-07-08", "")
	insertKeyFact(t, s, scope, "summary:counting-a-3", "Nate won a Valorant tournament on 2023-07-08.", true)
	insertKeyFact(t, s, scope, "summary:counting-a-3", "Joanna received a thank-you letter on 2023-07-08.", true) // different entity entirely

	var got []exhaustiveCountingFact
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		got, err = s.exhaustiveKeyFactsForEntity(ctx, tx, scope, "Nate")
		return err
	})
	if err != nil {
		t.Fatalf("exhaustiveKeyFactsForEntity: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("exhaustiveKeyFactsForEntity() returned %d facts, want 3 (Nate's three tournament wins, not Joanna's letter)", len(got))
	}
	wantPeriods := []string{"2023-01-01", "2023-03-15", "2023-07-08"}
	for i, f := range got {
		if f.period != wantPeriods[i] {
			t.Errorf("fact %d period = %q, want %q (chronological order)", i, f.period, wantPeriods[i])
		}
		if !strings.Contains(f.text, "Nate") {
			t.Errorf("fact %d text = %q, want it to mention Nate", i, f.text)
		}
	}
}

// TestExhaustiveKeyFactsForEntity_ExcludesRollupLevels is the regression
// test for this function's own central design decision: a weekly rollup
// re-extracts its own independent key_facts from its daily sources'
// prose (Runner.RunRollup -> generateSummary), so an unrestricted
// scope-wide scan would double-count the same real occurrence once per
// rollup level it survives into. Restricting to level = 'daily' is what
// prevents that.
func TestExhaustiveKeyFactsForEntity_ExcludesRollupLevels(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-counting-b"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryStub(t, s, scope, "summary:counting-b-daily", "daily", "2023-02-01", "")
	insertKeyFact(t, s, scope, "summary:counting-b-daily", "Nate won a tournament on 2023-02-01.", true)

	// Simulates a weekly rollup independently re-extracting the exact
	// same occurrence as its own new key_facts row.
	insertSummaryStub(t, s, scope, "summary:counting-b-weekly", "weekly", "2023-W05", "")
	insertKeyFact(t, s, scope, "summary:counting-b-weekly", "Nate won a tournament during the week of 2023-02-01.", true)

	var got []exhaustiveCountingFact
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		got, err = s.exhaustiveKeyFactsForEntity(ctx, tx, scope, "Nate")
		return err
	})
	if err != nil {
		t.Fatalf("exhaustiveKeyFactsForEntity: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("exhaustiveKeyFactsForEntity() returned %d facts, want exactly 1 (the daily occurrence only, not the weekly rollup's re-extraction of the same event)", len(got))
	}
	if got[0].summaryID != "summary:counting-b-daily" {
		t.Errorf("exhaustiveKeyFactsForEntity() returned summary %q, want the daily-level summary", got[0].summaryID)
	}
}

// TestExhaustiveKeyFactsForEntity_ExcludesSupersededSummaries confirms
// the same "current" definition fusedSearchSummaries uses (no other
// summary's supersedes points at this row) applies here too — a
// corrected daily summary's pre-correction facts shouldn't still be
// counted alongside the correction's own facts.
func TestExhaustiveKeyFactsForEntity_ExcludesSupersededSummaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-counting-c"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryStub(t, s, scope, "summary:counting-c-old", "daily", "2023-04-09", "")
	insertKeyFact(t, s, scope, "summary:counting-c-old", "Nate won a tournament on 2023-04-09.", true)

	// The correction: a brand-new row whose own supersedes points
	// backward at the replaced row (store.go never updates in place).
	insertSummaryStub(t, s, scope, "summary:counting-c-new", "daily", "2023-04-09", "summary:counting-c-old")
	insertKeyFact(t, s, scope, "summary:counting-c-new", "Nate did not win the tournament on 2023-04-09; he placed second.", true)

	var got []exhaustiveCountingFact
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		got, err = s.exhaustiveKeyFactsForEntity(ctx, tx, scope, "Nate")
		return err
	})
	if err != nil {
		t.Fatalf("exhaustiveKeyFactsForEntity: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("exhaustiveKeyFactsForEntity() returned %d facts, want exactly 1 (only the corrected, current summary's fact)", len(got))
	}
	if got[0].summaryID != "summary:counting-c-new" {
		t.Errorf("exhaustiveKeyFactsForEntity() returned summary %q, want the corrected (current) summary, not the superseded one", got[0].summaryID)
	}
}

// TestExhaustiveKeyFactsForEntity_ExcludesUngroundedFacts confirms this
// stays consistent with loadKeyFacts' own long-standing rule (see its
// doc comment): an ungrounded fact never gets surfaced as something the
// model should treat as established memory, counting included.
func TestExhaustiveKeyFactsForEntity_ExcludesUngroundedFacts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-counting-d"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryStub(t, s, scope, "summary:counting-d-1", "daily", "2023-05-01", "")
	insertKeyFact(t, s, scope, "summary:counting-d-1", "Nate claims to have won a tournament on 2023-05-01.", false)

	var got []exhaustiveCountingFact
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		got, err = s.exhaustiveKeyFactsForEntity(ctx, tx, scope, "Nate")
		return err
	})
	if err != nil {
		t.Fatalf("exhaustiveKeyFactsForEntity: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("exhaustiveKeyFactsForEntity() returned %d facts, want 0 (the only fact is ungrounded)", len(got))
	}
}

// TestExhaustiveKeyFactsForEntity_DeduplicatesExactRepeatedText is the
// real, documented case (docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md: facts
// "sometimes re-confirmed across two consolidation passes") — the exact
// same occurrence can genuinely get key-facted twice with identical text;
// this must count as one, not two.
func TestExhaustiveKeyFactsForEntity_DeduplicatesExactRepeatedText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-counting-e"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	const repeated = "Nate won a tournament on 2023-06-06."
	insertSummaryStub(t, s, scope, "summary:counting-e-1", "daily", "2023-06-06", "")
	insertKeyFact(t, s, scope, "summary:counting-e-1", repeated, true)
	insertSummaryStub(t, s, scope, "summary:counting-e-2", "daily", "2023-06-07", "")
	insertKeyFact(t, s, scope, "summary:counting-e-2", repeated, true) // re-confirmed verbatim on a later pass

	var got []exhaustiveCountingFact
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		got, err = s.exhaustiveKeyFactsForEntity(ctx, tx, scope, "Nate")
		return err
	})
	if err != nil {
		t.Fatalf("exhaustiveKeyFactsForEntity: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("exhaustiveKeyFactsForEntity() returned %d facts, want 1 (exact-text duplicate collapsed)", len(got))
	}
}
