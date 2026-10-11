package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

func TestSearch_FindsPlainKeywordQueryThatRetrieveGateSkips(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-search-gate"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEpisodeWithoutEmbedding(t, s, scope, "ep_test_search_gate_1",
		"postgres migration plan: use pg_dump then pg_restore", "Sounds good.")

	const query = "postgres migration plan"
	retrieved, err := s.Retrieve(ctx, scope, scope, []provider.Message{{Role: provider.RoleUser, Content: query}}, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if retrieved.Gate != gateway.GateSkipped {
		t.Fatalf("test premise broken: Retrieve gate = %q for %q, want skipped", retrieved.Gate, query)
	}

	result, err := s.Search(ctx, scope, scope, query, time.Now(), SearchOptions{ReadAudit: ReadAudit{Via: "mcp", Tool: "search_memory"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if result.Gate == gateway.GateSkipped {
		t.Fatalf("Search gate = skipped; it must always search")
	}
	if result.AnchorCitations > len(result.Citations) {
		t.Fatalf("AnchorCitations = %d exceeds %d citations", result.AnchorCitations, len(result.Citations))
	}
	found := false
	for _, c := range result.Citations[result.AnchorCitations:] {
		if c.Ref.Kind == identity.RefKindEpisode && c.Ref.ID == "ep_test_search_gate_1" {
			found = true
		}
	}
	if !found {
		t.Errorf("Search hits don't include the matching episode; citations = %+v", result.Citations)
	}

	var detail string
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select detail::text from audit_log
			where event_type = 'retrieve' and acting_scope_owner = $1 and detail->>'tool' = 'search_memory'
			order by ts desc limit 1
		`, scope.Owner).Scan(&detail)
	}); err != nil {
		t.Fatalf("read search audit row: %v", err)
	}
	for _, want := range []string{`"forced": true`, `"via": "mcp"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("audit detail %s missing %s", detail, want)
		}
	}
}

func TestSearch_DoesNotSeeAnotherScopesMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-search-iso-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-search-iso-b"}
	t.Cleanup(func() { cleanupScope(t, s, scopeA); cleanupScope(t, s, scopeB) })

	insertEpisodeWithoutEmbedding(t, s, scopeA, "ep_test_search_iso_a", "zanzibar deployment checklist", "Noted.")

	result, err := s.Search(ctx, scopeB, scopeB, "zanzibar deployment checklist", time.Now(), SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, c := range result.Citations {
		if c.Ref.ID == "ep_test_search_iso_a" {
			t.Fatalf("scope B's search returned scope A's episode")
		}
	}
}
