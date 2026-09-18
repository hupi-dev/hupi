package identity

import "testing"

func TestIdentityPrivateScope(t *testing.T) {
	id := Identity{UserID: "user:alice", TeamIDs: []string{"team:acme"}}
	got := id.PrivateScope()
	want := Scope{Kind: ScopeKindPrivate, Owner: "user:alice"}
	if got != want {
		t.Errorf("PrivateScope() = %+v, want %+v", got, want)
	}
}

func TestIdentityHasTeam(t *testing.T) {
	id := Identity{UserID: "user:alice", TeamIDs: []string{"team:acme", "team:other"}}

	if !id.HasTeam("team:acme") {
		t.Error("HasTeam(\"team:acme\") = false, want true")
	}
	if !id.HasTeam("team:other") {
		t.Error("HasTeam(\"team:other\") = false, want true")
	}
	if id.HasTeam("team:not-a-member") {
		t.Error("HasTeam(\"team:not-a-member\") = true, want false")
	}
}

func TestIdentityHasTeamWithNoTeams(t *testing.T) {
	id := Identity{UserID: "user:alice"}
	if id.HasTeam("team:anything") {
		t.Error("HasTeam on an identity with no teams = true, want false")
	}
}

func TestDefaultScope(t *testing.T) {
	want := Scope{Kind: ScopeKindPrivate, Owner: DefaultUserID}
	if DefaultScope != want {
		t.Errorf("DefaultScope = %+v, want %+v", DefaultScope, want)
	}
}
