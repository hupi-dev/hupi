package main

import (
	"net/http"

	"hupi/internal/audit"
	"hupi/internal/auth"
	"hupi/internal/identity"
)

// This file holds the admin UI's Tier-3-only (team/API-key) routes —
// isolated here specifically so it can be lifted into a separately-
// licensed package later (see the open-core split plan). listTeams and
// createTeam still call s.store (CreateTeam/ListTeams are shared scope
// bookkeeping every tier's backup/restore path also needs — see
// internal/auth/auth.go), but the routes themselves are pure team-
// management UI, which has no purpose without real multi-user auth.

func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawKey, err := s.teamStore.CreateAPIKey(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: id},
		map[string]any{"action": "create-key", "user_id": id})
	writeJSON(w, http.StatusOK, map[string]string{"raw_key": rawKey})
}

func (s *server) revokeKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KeyHash string `json:"key_hash"`
		UserID  string `json:"user_id"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.KeyHash == "" || req.UserID == "" {
		jsonError(w, http.StatusBadRequest, "key_hash and user_id are required")
		return
	}
	if err := s.teamStore.RevokeAPIKey(r.Context(), req.KeyHash); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: req.UserID},
		map[string]any{"action": "revoke-key", "user_id": req.UserID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *server) listTeams(w http.ResponseWriter, r *http.Request) {
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, teams)
}

func (s *server) createTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" || req.Name == "" {
		jsonError(w, http.StatusBadRequest, "id and name are required")
		return
	}
	if err := s.store.CreateTeam(r.Context(), req.ID, req.Name); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindShared, Owner: req.ID},
		map[string]any{"action": "create-team", "team_id": req.ID, "name": req.Name})
	writeJSON(w, http.StatusCreated, auth.Team{ID: req.ID, Name: req.Name})
}

func (s *server) teamDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	teams, err := s.store.ListTeams(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var team *auth.Team
	for i := range teams {
		if teams[i].ID == id {
			team = &teams[i]
			break
		}
	}
	if team == nil {
		jsonError(w, http.StatusNotFound, "team not found")
		return
	}
	members, err := s.teamStore.ListTeamMembers(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminUIView, identity.Scope{Kind: identity.ScopeKindShared, Owner: id},
		map[string]any{"page": "team_detail", "team_id": id})
	writeJSON(w, http.StatusOK, struct {
		Team    auth.Team         `json:"team"`
		Members []auth.Membership `json:"members"`
	}{*team, members})
}

func (s *server) addMember(w http.ResponseWriter, r *http.Request) {
	teamID := r.PathValue("id")
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UserID == "" {
		jsonError(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if err := s.teamStore.AddTeamMember(r.Context(), teamID, req.UserID, req.Role); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindShared, Owner: teamID},
		map[string]any{"action": "add-member", "team_id": teamID, "user_id": req.UserID, "role": req.Role})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
