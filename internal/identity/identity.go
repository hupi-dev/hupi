package identity

// Identity is the authenticated caller of a request, resolved from an API
// key by internal/auth.TeamStore.Resolve (Phase 3, docs/TIER3_PLAN.md). A
// user's own private scope is always implicitly accessible; TeamIDs are
// the shared scopes they're also allowed to reach once workspace routing
// (Phase 4) resolves a request to a specific one.
type Identity struct {
	UserID  string
	TeamIDs []string
}

// PrivateScope is the Identity's own private scope.
func (i Identity) PrivateScope() Scope {
	return Scope{Kind: ScopeKindPrivate, Owner: i.UserID}
}

// HasTeam reports whether the identity is a member of the given team —
// used by Phase 4's workspace routing to authorize a request to
// /v1/team/{team_id}/... before resolving it to that team's shared scope.
func (i Identity) HasTeam(teamID string) bool {
	for _, t := range i.TeamIDs {
		if t == teamID {
			return true
		}
	}
	return false
}
