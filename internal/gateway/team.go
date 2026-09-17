package gateway

import (
	"errors"
	"fmt"
	"net/http"

	"hupi/internal/identity"
)

// This file holds every Tier-3-only (team/shared-workspace) request path
// — isolated here specifically so it can be lifted into a separately-
// licensed package later (see the open-core split plan). It has no
// dependencies outside the same package's existing exported/unexported
// seams (Handler, handleChatCompletionsScoped, handleFeedbackScoped,
// resolveIdentity) — Tier 1/2 code paths never call into this file.

// HandleTeamChatCompletions serves POST /v1/team/{team_id}/chat/completions
// — workspace routing is path-based, not a per-message flag
// (docs/TIER3_PLAN.md D4), specifically because a per-message "was this
// shared?" toggle is the kind of thing people forget under normal use.
// Requires h.Auth to be configured: with no auth there's no way to prove
// team membership, so a team request is correctly refused (403) rather
// than silently allowed — see resolveTeamScope.
//
// Mount alongside the other routes with a Go 1.22+ ServeMux pattern:
//
//	mux.HandleFunc("POST /v1/team/{team_id}/chat/completions", handler.HandleTeamChatCompletions)
func (h *Handler) HandleTeamChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actingUser, workspace, status, err := h.resolveTeamScope(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	h.handleChatCompletionsScoped(w, r, actingUser, workspace)
}

// HandleTeamFeedback serves POST /v1/team/{team_id}/feedback — feedback on
// an episode captured in that team's shared workspace. Mount alongside
// HandleTeamChatCompletions:
//
//	mux.HandleFunc("POST /v1/team/{team_id}/feedback", handler.HandleTeamFeedback)
func (h *Handler) HandleTeamFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actingUser, workspace, status, err := h.resolveTeamScope(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	h.handleFeedbackScoped(w, r, workspace, actingUser.Owner)
}

// resolveTeamScope determines actingUser/workspace for a team-route
// request (docs/TIER3_PLAN.md D4): actingUser is always the caller's own
// private scope (so self_model stays personal, per D3); workspace is the
// requested team's shared scope, granted only if the caller is actually a
// member — otherwise this returns 403, distinct from resolveIdentity's 401
// for "not authenticated at all."
func (h *Handler) resolveTeamScope(r *http.Request) (actingUser, workspace identity.Scope, status int, err error) {
	id, err := h.resolveIdentity(r)
	if err != nil {
		return identity.Scope{}, identity.Scope{}, http.StatusUnauthorized, err
	}
	teamID := r.PathValue("team_id")
	if teamID == "" {
		return identity.Scope{}, identity.Scope{}, http.StatusBadRequest, errors.New("missing team_id in path")
	}
	if !id.HasTeam(teamID) {
		return identity.Scope{}, identity.Scope{}, http.StatusForbidden, fmt.Errorf("not a member of %s", teamID)
	}
	return id.PrivateScope(), identity.Scope{Kind: identity.ScopeKindShared, Owner: teamID}, 0, nil
}
