package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"hupi/internal/audit"
	"hupi/internal/auth"
	"hupi/internal/identity"
)

// server holds the dependencies every handler needs. Deliberately not
// named "Handler" (cf. internal/gateway.Handler) — this never leaves
// package main, so there's no reason to export it.
type server struct {
	store *auth.Store
	db    *sql.DB // for audit.LogStandalone/audit.Query — everything else goes through store
}

// routes wires up the JSON API. Every route here lives under /api/ and is
// wrapped by main.go's requireCSRFSafe(requireOperatorAuth(...)) — see
// main.go's run() for how this mux and the placeholder SPA handler
// (assets.go) are combined.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("GET /api/users/{id}", s.userDetail)
	mux.HandleFunc("POST /api/users/{id}/keys", s.createKey)
	mux.HandleFunc("POST /api/keys/revoke", s.revokeKey)

	mux.HandleFunc("GET /api/teams", s.listTeams)
	mux.HandleFunc("POST /api/teams", s.createTeam)
	mux.HandleFunc("GET /api/teams/{id}", s.teamDetail)
	mux.HandleFunc("POST /api/teams/{id}/members", s.addMember)

	mux.HandleFunc("GET /api/operators", s.listOperators)
	mux.HandleFunc("POST /api/operators", s.createOperator)
	mux.HandleFunc("POST /api/operators/revoke", s.revokeOperator)

	mux.HandleFunc("GET /api/whoami", s.whoami)
	mux.HandleFunc("GET /api/audit", s.auditQuery)

	return mux
}

// audit is best-effort — a failed audit write shouldn't turn a successful
// provisioning action or page view into an error response for the
// operator, only a gap in the log (logged to stderr instead, matching
// cmd/hupi-admin's logAdminAction).
func (s *server) audit(r *http.Request, eventType string, scope identity.Scope, detail map[string]any) {
	err := audit.LogStandalone(r.Context(), s.db, audit.Entry{
		EventType:      eventType,
		Actor:          operatorFromContext(r.Context()),
		ActingScope:    scope,
		WorkspaceScope: scope,
		Detail:         detail,
	})
	if err != nil {
		slog.Error("admin UI audit log write failed", "event_type", eventType, "error", err)
	}
}

// --- users ---------------------------------------------------------------

func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		jsonError(w, http.StatusBadRequest, "id is required")
		return
	}
	if err := s.store.CreateUser(r.Context(), req.ID, req.Email); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: req.ID},
		map[string]any{"action": "create-user", "user_id": req.ID})
	writeJSON(w, http.StatusCreated, auth.User{ID: req.ID, Email: req.Email})
}

// userDetail bundles what used to be three separate page loads (the user,
// their teams, their API keys) into one response — see docs/ADMIN_UI.md.
func (s *server) userDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	var user *auth.User
	for i := range users {
		if users[i].ID == id {
			user = &users[i]
			break
		}
	}
	if user == nil {
		jsonError(w, http.StatusNotFound, "user not found")
		return
	}
	teams, err := s.store.ListTeamsForUser(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return
	}
	keys, err := s.store.ListAPIKeys(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminUIView, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: id},
		map[string]any{"page": "user_detail", "user_id": id})
	writeJSON(w, http.StatusOK, struct {
		User  auth.User     `json:"user"`
		Teams []auth.Team   `json:"teams"`
		Keys  []auth.APIKey `json:"keys"`
	}{*user, teams, keys})
}

func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rawKey, err := s.store.CreateAPIKey(r.Context(), id)
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
	if err := s.store.RevokeAPIKey(r.Context(), req.KeyHash); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: req.UserID},
		map[string]any{"action": "revoke-key", "user_id": req.UserID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- teams -----------------------------------------------------------------

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
	members, err := s.store.ListTeamMembers(r.Context(), id)
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
	if err := s.store.AddTeamMember(r.Context(), teamID, req.UserID, req.Role); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.Scope{Kind: identity.ScopeKindShared, Owner: teamID},
		map[string]any{"action": "add-member", "team_id": teamID, "user_id": req.UserID, "role": req.Role})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- operators ---------------------------------------------------------------

// Operators aren't scoped to a user or team (they have no private/shared
// memory scope of their own — see internal/auth.go's Operator doc
// comment), so there's no natural identity.Scope for their audit events.
// cmd/hupi-admin's create-operator/revoke-operator subcommands already
// settled this by logging under identity.DefaultScope; these handlers
// follow the same convention for consistency across both admin surfaces.

func (s *server) listOperators(w http.ResponseWriter, r *http.Request) {
	operators, err := s.store.ListOperators(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, operators)
}

func (s *server) createOperator(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	rawToken, err := s.store.CreateOperator(r.Context(), req.Name)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.DefaultScope,
		map[string]any{"action": "create-operator", "operator_name": req.Name})
	writeJSON(w, http.StatusCreated, map[string]string{"raw_token": rawToken})
}

func (s *server) revokeOperator(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		jsonError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.store.RevokeOperator(r.Context(), req.Name); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, audit.EventAdminProvision, identity.DefaultScope,
		map[string]any{"action": "revoke-operator", "operator_name": req.Name})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// --- misc ---------------------------------------------------------------

// whoami lets a frontend confirm its stored Basic Auth credentials still
// work and show "logged in as X" without needing a dedicated session
// concept — this admin UI has none (see docs/ADMIN_UI.md), every request
// re-authenticates.
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"name": operatorFromContext(r.Context())})
}

const (
	defaultAuditLimit = 100
	maxAuditLimit     = 500
)

// auditQuery exposes internal/audit.Query over HTTP for GET /api/audit.
// Viewing the audit log is itself an admin action worth auditing (see
// docs/GAP_CLOSURE_PLAN.md §4.3's stated scope: "writes + retrievals +
// admin access"), but a cross-tenant query has no single natural scope
// the way a user or team detail page does. If the caller filtered by
// scope_owner, that scope is the most honest description of what was
// looked at; otherwise this falls back to identity.DefaultScope, the same
// placeholder cmd/hupi-admin uses for scope-less admin actions
// (create-operator/revoke-operator).
func (s *server) auditQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := defaultAuditLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			jsonError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}

	f := audit.QueryFilter{
		ScopeKind:  q.Get("scope_kind"),
		ScopeOwner: q.Get("scope_owner"),
		Actor:      q.Get("actor"),
		EventType:  q.Get("event_type"),
		Limit:      limit,
		Ascending:  true,
	}
	if raw := q.Get("since"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		f.Since = &ts
	}
	if raw := q.Get("until"); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			jsonError(w, http.StatusBadRequest, "until must be RFC3339")
			return
		}
		f.Until = &ts
	}

	entries, err := audit.Query(r.Context(), s.db, f)
	if err != nil {
		internalError(w, err)
		return
	}

	scope := identity.DefaultScope
	if f.ScopeOwner != "" {
		kind := f.ScopeKind
		if kind == "" {
			kind = identity.ScopeKindPrivate
		}
		scope = identity.Scope{Kind: kind, Owner: f.ScopeOwner}
	}
	s.audit(r, audit.EventAdminUIView, scope,
		map[string]any{"page": "audit_log", "filter": f})

	writeJSON(w, http.StatusOK, entries)
}

// --- JSON helpers ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("admin UI encode response failed", "error", err)
	}
}

// jsonError writes {"error": "<message>"} with the given status code.
func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// internalError logs the real error server-side but never reflects it
// into the response body — some of these wrap raw Postgres errors (e.g.
// an AddTeamMember foreign-key violation echoing back the attempted user
// id), and none of that belongs in a JSON response an API client renders
// to an operator.
func internalError(w http.ResponseWriter, err error) {
	slog.Error("admin UI request failed", "error", err)
	jsonError(w, http.StatusInternalServerError, "internal error — see server logs")
}

// decodeJSON decodes r.Body into v, writing a 400 JSON error and
// returning false on failure. An empty body decodes to io.EOF, which is
// treated as "no fields set" (the zero value of v) rather than an error —
// some routes (e.g. POST /users/{id}/keys) take no body at all, and
// requiring `{}` from every client for those would be needless ceremony.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		jsonError(w, http.StatusBadRequest, "invalid JSON request body")
		return false
	}
	return true
}
