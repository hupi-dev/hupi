package store

import (
	"context"
	"database/sql"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestEntityRelationships_RejectsValidUntilBeforeValidFrom is the real
// regression test for review finding B22: entity_relationships
// (schema/0015) let valid_from/valid_until be set independently with
// nothing stopping a relationship from being recorded as "ending before
// it starts." schema/0019 adds a check constraint for exactly this.
func TestEntityRelationships_RejectsValidUntilBeforeValidFrom(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rel-date-order"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:alice", "person", "Alice")
	insertEntity(t, s, scope, "project:widget", "project", "Widget")

	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entity_relationships (id, scope_kind, scope_owner, subject_id, predicate, object_id, valid_from, valid_until)
			values ($1, $2, $3, $4, $5, $6, '2026-06-01', '2026-01-01')
		`, "rel:test-backwards", scope.Kind, scope.Owner, "person:alice", "works_on", "project:widget")
		return err
	})
	if err == nil {
		t.Fatal("expected inserting a relationship with valid_until before valid_from to be rejected by the new check constraint")
	}
}

// TestEntityRelationships_AllowsValidDateOrderingsAndUnknownEnds confirms
// the new constraint isn't overly strict: a well-ordered date range, and
// either end left null (unknown start / still current, schema/0015's own
// documented meaning), must still insert cleanly.
func TestEntityRelationships_AllowsValidDateOrderingsAndUnknownEnds(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rel-date-order-ok"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:bob", "person", "Bob")
	insertEntity(t, s, scope, "project:widget2", "project", "Widget 2")

	cases := []struct {
		name              string
		validFrom, validUntil any
	}{
		{"well-ordered range", "2026-01-01", "2026-06-01"},
		{"equal start and end", "2026-01-01", "2026-01-01"},
		{"unknown start", nil, "2026-06-01"},
		{"still current", "2026-01-01", nil},
		{"both unknown", nil, nil},
	}
	for i, c := range cases {
		id := "rel:test-ok-" + string(rune('a'+i))
		err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				insert into entity_relationships (id, scope_kind, scope_owner, subject_id, predicate, object_id, valid_from, valid_until)
				values ($1, $2, $3, $4, $5, $6, $7, $8)
			`, id, scope.Kind, scope.Owner, "person:bob", "works_on", "project:widget2", c.validFrom, c.validUntil)
			return err
		})
		if err != nil {
			t.Errorf("%s: expected insert to succeed, got: %v", c.name, err)
		}
	}
}
