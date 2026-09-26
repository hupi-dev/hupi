package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// insertRelationship inserts a currently-valid (valid_until null) edge
// directly, bypassing consolidation's own extraction/canonicalization —
// this file tests retrieve.go's graph walk in isolation, not
// internal/consolidation's write path (see relationships_test.go in that
// package for those tests).
func insertRelationship(t *testing.T, s *Store, scope identity.Scope, id, subjectID, predicate, objectID string) {
	t.Helper()
	err := dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entity_relationships (id, scope_kind, scope_owner, subject_id, predicate, object_id)
			values ($1, $2, $3, $4, $5, $6)
		`, id, scope.Kind, scope.Owner, subjectID, predicate, objectID)
		return err
	})
	if err != nil {
		t.Fatalf("insert test relationship %s: %v", id, err)
	}
}

// TestRetrieve_GraphWalkSurfacesConnectedEntityNeverNamedInQuery is the
// real point of docs/ENTITY_RELATIONSHIPS_PLAN.md §6: a query that only
// names one entity (Melanie, found by stage1's plain substring match)
// should still surface a second entity (Zara) connected to it by a
// relationship edge — Zara's own name/attributes share nothing with the
// query, and she has no embedding at all, so neither vector search nor
// keyword search could find her; only the graph walk can.
func TestRetrieve_GraphWalkSurfacesConnectedEntityNeverNamedInQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-graph-walk"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:melanie", "person", "Melanie")
	insertEntity(t, s, scope, "person:zara", "person", "Zara")
	insertRelationship(t, s, scope, "rel:test1", "person:melanie", "friends_with", "person:zara")

	messages := []provider.Message{{Role: provider.RoleUser, Content: "Tell me about Melanie"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "entity person:zara") {
		t.Errorf("expected Zara (connected to Melanie via friends_with, never named in the query, no embedding of her own) to be surfaced by the graph walk, got: %q", result.ContextMessage)
	}
}

// TestRetrieve_GraphWalkDisabledByFlag mirrors
// bm25_retrieve_test.go's TestRetrieve_KeywordSearchDisabledByFlag: the
// exact same scenario as the test above, but with the admin escape
// hatch off, proving the flag genuinely gates this feature rather than
// just existing as an unused knob.
func TestRetrieve_GraphWalkDisabledByFlag(t *testing.T) {
	t.Setenv("HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK", "false")
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-graph-walk-disabled"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:melanie", "person", "Melanie")
	insertEntity(t, s, scope, "person:zara", "person", "Zara")
	insertRelationship(t, s, scope, "rel:test2", "person:melanie", "friends_with", "person:zara")

	messages := []provider.Message{{Role: provider.RoleUser, Content: "Tell me about Melanie"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if strings.Contains(result.ContextMessage, "person:zara") {
		t.Errorf("expected Zara NOT to be surfaced with HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK=false, got: %q", result.ContextMessage)
	}
}
