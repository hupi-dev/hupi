package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"hupi/internal/identity"
)

// stubAuthenticator resolves a fixed set of raw keys to identities, for
// testing gateway's authorization logic without a real database.
type stubAuthenticator struct {
	byKey map[string]identity.Identity
}

func (s stubAuthenticator) Resolve(_ context.Context, apiKey string) (identity.Identity, error) {
	id, ok := s.byKey[apiKey]
	if !ok {
		return identity.Identity{}, errors.New("invalid key")
	}
	return id, nil
}

func newTeamRequest(t *testing.T, teamID, bearer string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/team/"+teamID+"/chat/completions", nil)
	r.SetPathValue("team_id", teamID)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// TestResolveTeamScope_NoAuthConfigured is the fail-closed case
// docs/TIER3_PLAN.md's D4 depends on: with h.Auth nil, there is no way to
// prove team membership, so every team-routed request must be refused,
// never silently allowed into a shared scope.
func TestResolveTeamScope_NoAuthConfigured(t *testing.T) {
	h := &Handler{Auth: nil}
	r := newTeamRequest(t, "team:acme-eng", "")

	_, _, status, err := h.resolveTeamScope(r)
	if err == nil {
		t.Fatal("expected an error with no auth configured, got nil")
	}
	if status != http.StatusForbidden {
		t.Errorf("got status %d, want %d (Forbidden)", status, http.StatusForbidden)
	}
}

func TestResolveTeamScope_MemberIsAuthorized(t *testing.T) {
	h := &Handler{Auth: stubAuthenticator{byKey: map[string]identity.Identity{
		"valid-key": {UserID: "user:alice", TeamIDs: []string{"team:acme-eng"}},
	}}}
	r := newTeamRequest(t, "team:acme-eng", "valid-key")

	actingUser, workspace, _, err := h.resolveTeamScope(r)
	if err != nil {
		t.Fatalf("expected a team member to be authorized, got error: %v", err)
	}
	wantActingUser := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:alice"}
	wantWorkspace := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:acme-eng"}
	if actingUser != wantActingUser {
		t.Errorf("actingUser = %+v, want %+v", actingUser, wantActingUser)
	}
	if workspace != wantWorkspace {
		t.Errorf("workspace = %+v, want %+v", workspace, wantWorkspace)
	}
}

// TestResolveTeamScope_NonMemberIsForbidden is the core authorization
// check: an authenticated user who simply isn't on the team must not
// reach that team's shared scope.
func TestResolveTeamScope_NonMemberIsForbidden(t *testing.T) {
	h := &Handler{Auth: stubAuthenticator{byKey: map[string]identity.Identity{
		"valid-key": {UserID: "user:bob", TeamIDs: []string{"team:other-team"}},
	}}}
	r := newTeamRequest(t, "team:acme-eng", "valid-key")

	_, _, status, err := h.resolveTeamScope(r)
	if err == nil {
		t.Fatal("expected a non-member to be refused, got nil error")
	}
	if status != http.StatusForbidden {
		t.Errorf("got status %d, want %d (Forbidden)", status, http.StatusForbidden)
	}
}

func TestResolveTeamScope_InvalidKeyIsUnauthorized(t *testing.T) {
	h := &Handler{Auth: stubAuthenticator{byKey: map[string]identity.Identity{}}}
	r := newTeamRequest(t, "team:acme-eng", "garbage-key")

	_, _, status, err := h.resolveTeamScope(r)
	if err == nil {
		t.Fatal("expected an invalid key to fail, got nil error")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("got status %d, want %d (Unauthorized) — invalid credentials should be distinct from valid-but-unauthorized", status, http.StatusUnauthorized)
	}
}
