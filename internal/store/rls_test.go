package store

import (
	"context"
	"database/sql"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// This file tests RLS itself (schema/0005_hardening_phase3_rls.sql), not
// the application code on top of it — docs/HARDENING_PLAN.md §7's specific
// ask: "a raw query issued without setting the session variables must
// return zero rows, not real data." internal/store/scope_isolation_test.go
// proves the two layers agree; this file proves the second layer holds on
// its own, even when the application-level scope filtering isn't there to
// help it.

// TestRLS_DeniesCompletelyUnscopedRead: a query issued with no RLS session
// variables set at all — as if a future code path forgot to use dbscope —
// must see zero rows, not the row it would otherwise match on id alone.
func TestRLS_DeniesCompletelyUnscopedRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-a"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "project:rls-test", "project", "RLS Test")

	var count int
	// Deliberately s.db directly, not dbscope.Run — this is the exact
	// mistake RLS exists to catch.
	if err := s.db.QueryRowContext(ctx, `select count(*) from entities where id = 'project:rls-test'`).Scan(&count); err != nil {
		t.Fatalf("unscoped count query: %v", err)
	}
	if count != 0 {
		t.Errorf("RLS FAILED TO ENFORCE: an unscoped query saw %d row(s) it should never have matched", count)
	}
}

// TestRLS_TwoScopePolicyIsExactNotBroad confirms the OR in the policy
// (docs/HARDENING_PLAN.md D2) grants access to exactly the two scopes set
// — acting and workspace — not anything broader. A transaction scoped to
// (userA as both acting and workspace) must not see userB's data, even
// though both are "private" scope_kind.
func TestRLS_TwoScopePolicyIsExactNotBroad(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-b1"}
	scopeB := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-b2"}
	t.Cleanup(func() { cleanupScope(t, s, scopeA); cleanupScope(t, s, scopeB) })

	insertEntity(t, s, scopeA, "project:only-a", "project", "Only A")
	insertEntity(t, s, scopeB, "project:only-b", "project", "Only B")

	var visibleIDs []string
	err := dbscope.Run(ctx, s.db, scopeA, scopeA, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `select id from entities where id in ('project:only-a', 'project:only-b')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			visibleIDs = append(visibleIDs, id)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("scoped query: %v", err)
	}
	if len(visibleIDs) != 1 || visibleIDs[0] != "project:only-a" {
		t.Errorf("scoped to scopeA, expected to see only project:only-a, got %v", visibleIDs)
	}
}

// TestRLS_RejectsWriteOutsideWorkspace confirms WITH CHECK: a transaction
// scoped to workspace W cannot insert a row claiming to belong to a
// different scope — the write must be rejected outright, not silently
// re-scoped or silently dropped.
func TestRLS_RejectsWriteOutsideWorkspace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	workspace := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-rls-c"}
	otherScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rls-c"}
	t.Cleanup(func() { cleanupScope(t, s, workspace); cleanupScope(t, s, otherScope) })

	enc, _, err := s.keys.GetOrCreate(ctx, workspace)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt test attrs: %v", err)
	}

	err = dbscope.Run(ctx, s.db, workspace, workspace, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner)
			values ('project:smuggled', 'project', 'Smuggled', $1, $2, $3)
		`, attrsCT, otherScope.Kind, otherScope.Owner) // claims a DIFFERENT scope than the transaction's workspace
		return err
	})
	if err == nil {
		t.Fatal("RLS FAILED TO ENFORCE: inserting a row claiming a scope other than the transaction's workspace should have been rejected by WITH CHECK")
	}
}
