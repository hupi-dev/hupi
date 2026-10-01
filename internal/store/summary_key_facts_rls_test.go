package store

import (
	"context"
	"database/sql"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// This file tests schema/0018_summary_key_facts_rls.sql (docs/
// CODEBASE_SURVEY_AND_REVIEW.md finding B18) the same way rls_test.go
// tests episodes/entities: proving the policy holds on its own, even
// when none of the six existing call sites' own caller-side scope
// sequencing is there to help it. Before this migration,
// summary_key_facts had no scope columns and no RLS at all — every one
// of these would have returned the real row.

// insertSummaryWithKeyFact seeds one summary and one key fact row, both
// correctly scoped, the way internal/consolidation/store.go's
// storeSummary does post-B18.
func insertSummaryWithKeyFact(t *testing.T, s *Store, scope identity.Scope, summaryID string) {
	t.Helper()
	err := dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ($1, '2026-01-01', 'daily', 'test summary', $2, $3)
		`, summaryID, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, scope_kind, scope_owner)
			values ($1, $2, '{}', true, $3, $4)
		`, summaryID, []byte("encrypted-fact"), scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed summary + key fact: %v", err)
	}
}

// TestRLS_SummaryKeyFacts_DeniesCompletelyUnscopedRead: a direct query
// against summary_key_facts with no RLS session variables set at all —
// exactly the failure mode the review finding described (a future
// caller reaching this table without first verifying the parent
// summary's scope) — must see zero rows, not the row it would otherwise
// match by summary_id alone.
func TestRLS_SummaryKeyFacts_DeniesCompletelyUnscopedRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-skf-a"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	summaryID := "summary:test-rls-skf-a"
	insertSummaryWithKeyFact(t, s, scope, summaryID)

	var count int
	// Deliberately s.db directly, not dbscope.Run — simulating a future
	// caller that queries summary_key_facts without first checking the
	// parent summary's scope.
	if err := s.db.QueryRowContext(ctx, `select count(*) from summary_key_facts where summary_id = $1`, summaryID).Scan(&count); err != nil {
		t.Fatalf("unscoped count query: %v", err)
	}
	if count != 0 {
		t.Errorf("RLS FAILED TO ENFORCE: an unscoped query against summary_key_facts saw %d row(s) it should never have matched", count)
	}
}

// TestRLS_SummaryKeyFacts_DeniesCrossScopeRead confirms the policy isn't
// just "empty session vars means empty results" — a transaction
// correctly scoped to a *different* scope than the one the facts
// actually belong to must also see zero rows, not just an unscoped
// connection.
func TestRLS_SummaryKeyFacts_DeniesCrossScopeRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-skf-b1"}
	scopeB := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-skf-b2"}
	t.Cleanup(func() { cleanupScope(t, s, scopeA); cleanupScope(t, s, scopeB) })

	summaryID := "summary:test-rls-skf-b1"
	insertSummaryWithKeyFact(t, s, scopeA, summaryID)

	var count int
	err := dbscope.Run(ctx, s.db, scopeB, scopeB, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from summary_key_facts where summary_id = $1`, summaryID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("cross-scope count query: %v", err)
	}
	if count != 0 {
		t.Errorf("CROSS-SCOPE LEAK: scopeB's transaction saw %d row(s) belonging to scopeA's summary_key_facts", count)
	}

	// The owning scope must still see it — this isn't just "nobody sees
	// anything," the policy must be scope-exact, not scope-blind.
	err = dbscope.Run(ctx, s.db, scopeA, scopeA, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from summary_key_facts where summary_id = $1`, summaryID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("owning-scope count query: %v", err)
	}
	if count != 1 {
		t.Errorf("expected scopeA's own transaction to see its own key fact, got count = %d", count)
	}
}

// TestRLS_SummaryKeyFacts_RejectsWriteOutsideWorkspace confirms WITH
// CHECK: a transaction scoped to workspace W cannot insert a key fact
// claiming to belong to a different scope.
func TestRLS_SummaryKeyFacts_RejectsWriteOutsideWorkspace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	workspace := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-rls-skf-c"}
	otherScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-skf-c"}
	t.Cleanup(func() { cleanupScope(t, s, workspace); cleanupScope(t, s, otherScope) })

	summaryID := "summary:test-rls-skf-c"
	err := dbscope.Run(ctx, s.db, workspace, workspace, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ($1, '2026-01-01', 'daily', 'test summary', $2, $3)
		`, summaryID, workspace.Kind, workspace.Owner)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`
			insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, scope_kind, scope_owner)
			values ($1, $2, '{}', true, $3, $4)
		`, summaryID, []byte("encrypted-fact"), otherScope.Kind, otherScope.Owner) // claims a DIFFERENT scope than the transaction's workspace
		return err
	})
	if err == nil {
		t.Fatal("RLS FAILED TO ENFORCE: inserting a key fact claiming a scope other than the transaction's workspace should have been rejected by WITH CHECK")
	}
}
