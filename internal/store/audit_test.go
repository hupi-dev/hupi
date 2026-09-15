package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// See scope_isolation_test.go's doc comment for how to run these against
// a real instance. audit_log's SELECT policy is deliberately unrestricted
// (schema/0007_audit_log.sql) — a plain, unscoped query against s.db is
// the correct way to verify it here, not an oversight.

func TestCapture_WritesAuditLogEntry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-audit-capture"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })
	t.Cleanup(func() { s.db.Exec(`delete from audit_log where workspace_scope_owner = $1`, scope.Owner) })

	ep := gateway.Episode{
		ID:          "ep_test_audit_capture",
		TS:          time.Now(),
		Type:        "interaction",
		InputText:   "hello",
		OutputText:  "hi",
		Hash:        hash("audit-capture"),
		ActorUserID: "user:test-audit-capture", // matches scope.Owner here — see the team-scope case below
	}
	if err := s.Capture(ctx, scope, ep); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	var eventType, actor string
	var targetRefJSON []byte
	err := s.db.QueryRowContext(ctx, `
		select event_type, actor, target_ref from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'capture'
	`, scope.Kind, scope.Owner).Scan(&eventType, &actor, &targetRefJSON)
	if err != nil {
		t.Fatalf("expected a capture audit_log row: %v", err)
	}
	if actor != ep.ActorUserID {
		t.Errorf("actor = %q, want %q", actor, ep.ActorUserID)
	}
	var ref identity.Ref
	if err := json.Unmarshal(targetRefJSON, &ref); err != nil {
		t.Fatalf("parse target_ref: %v", err)
	}
	if ref.ID != ep.ID || ref.Kind != identity.RefKindEpisode {
		t.Errorf("target_ref = %+v, want episode ref for %s", ref, ep.ID)
	}
}

// TestCapture_AuditActorIsIndividualNotWorkspace exercises the exact
// scenario docs/GAP_CLOSURE_PLAN.md §4.3 called out: a team-scoped
// capture must record which member actually sent the message in `actor`,
// even though the scope columns themselves are the team's (RLS requires
// that — see capture.go's comment on why ActingScope/WorkspaceScope can't
// be the individual's own private scope here).
func TestCapture_AuditActorIsIndividualNotWorkspace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	teamScope := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-audit-actor"}
	t.Cleanup(func() { cleanupScope(t, s, teamScope) })
	t.Cleanup(func() { s.db.Exec(`delete from audit_log where workspace_scope_owner = $1`, teamScope.Owner) })

	ep := gateway.Episode{
		ID:          "ep_test_audit_actor",
		TS:          time.Now(),
		Type:        "interaction",
		InputText:   "team hello",
		OutputText:  "team hi",
		Hash:        hash("audit-actor"),
		ActorUserID: "user:test-audit-actor-member",
	}
	if err := s.Capture(ctx, teamScope, ep); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	var actor, workspaceOwner string
	err := s.db.QueryRowContext(ctx, `
		select actor, workspace_scope_owner from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'capture'
	`, teamScope.Kind, teamScope.Owner).Scan(&actor, &workspaceOwner)
	if err != nil {
		t.Fatalf("expected a capture audit_log row: %v", err)
	}
	if actor != ep.ActorUserID {
		t.Errorf("actor = %q, want the individual member %q, not the team", actor, ep.ActorUserID)
	}
	if workspaceOwner != teamScope.Owner {
		t.Errorf("workspace_scope_owner = %q, want the team %q", workspaceOwner, teamScope.Owner)
	}
}

func TestRetrieve_WritesAuditLogEntry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-audit-retrieve"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })
	t.Cleanup(func() { s.db.Exec(`delete from audit_log where workspace_scope_owner = $1`, scope.Owner) })

	_, err := s.Retrieve(ctx, scope, scope, []provider.Message{
		{Role: provider.RoleUser, Content: "what did we decide about the vector index?"},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	var eventType, actor string
	err = s.db.QueryRowContext(ctx, `
		select event_type, actor from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'retrieve'
	`, scope.Kind, scope.Owner).Scan(&eventType, &actor)
	if err != nil {
		t.Fatalf("expected a retrieve audit_log row: %v", err)
	}
	if actor != scope.Owner {
		t.Errorf("actor = %q, want %q", actor, scope.Owner)
	}
}

func TestTrace_WritesAuditLogEntry(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-audit-trace"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })
	t.Cleanup(func() { s.db.Exec(`delete from audit_log where workspace_scope_owner = $1`, scope.Owner) })

	ep := gateway.Episode{
		ID: "ep_test_audit_trace", TS: time.Now(), Type: "interaction",
		InputText: "trace me", OutputText: "ok", Hash: hash("audit-trace"),
	}
	if err := s.Capture(ctx, scope, ep); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	const investigator = "test-operator-dana"
	if _, err := s.Trace(ctx, scope, ep.ID, investigator); err != nil {
		t.Fatalf("Trace: %v", err)
	}

	var actor string
	var targetRefJSON []byte
	err := s.db.QueryRowContext(ctx, `
		select actor, target_ref from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'trace'
	`, scope.Kind, scope.Owner).Scan(&actor, &targetRefJSON)
	if err != nil {
		t.Fatalf("expected a trace audit_log row: %v", err)
	}
	if actor != investigator {
		t.Errorf("actor = %q, want %q", actor, investigator)
	}
	var ref identity.Ref
	if err := json.Unmarshal(targetRefJSON, &ref); err != nil {
		t.Fatalf("parse target_ref: %v", err)
	}
	if ref.ID != ep.ID {
		t.Errorf("target_ref.ID = %q, want %q", ref.ID, ep.ID)
	}
}
