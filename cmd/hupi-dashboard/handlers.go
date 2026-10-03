package main

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"hupi/internal/provider"
)

// server holds the dependencies every handler needs. Deliberately not
// exported, same reasoning as cmd/hupi-admin-ui's server type — never
// leaves package main.
type server struct {
	db       *sql.DB
	registry *provider.Registry // only used by Phase 2's LLM-powered theme narrative, content_analysis.go
}

// routes wires up the JSON API. Every route here is read-only and lives
// under /api/, wrapped by main.go's requireDashboardSession — see that
// function's doc comment for why this has no CSRF mitigation, unlike
// cmd/hupi-admin-ui.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/conversation-volume", s.handleConversationVolume)
	mux.HandleFunc("GET /api/theme-word-cloud", s.handleThemeWordCloud)
	mux.HandleFunc("GET /api/entity-relationships", s.handleEntityRelationships)
	mux.HandleFunc("GET /api/memory-health", s.handleMemoryHealth)
	mux.HandleFunc("GET /api/keyword-search-governance", s.handleKeywordSearchGovernance)
	mux.HandleFunc("GET /api/security-posture", s.handleSecurityPosture)
	mux.HandleFunc("GET /api/whoami", s.handleWhoami)

	return mux
}

func (s *server) handleConversationVolume(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 90)
	points, err := conversationVolume(r.Context(), s.db, scopeFromContext(r.Context()), days)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, points)
}

func (s *server) handleThemeWordCloud(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	entries, err := themeWordCloud(r.Context(), s.db, scopeFromContext(r.Context()), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *server) handleEntityRelationships(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 200)
	rels, err := entityRelationshipGraph(r.Context(), s.db, scopeFromContext(r.Context()), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rels)
}

func (s *server) handleMemoryHealth(w http.ResponseWriter, r *http.Request) {
	health, err := memoryHealth(r.Context(), s.db, scopeFromContext(r.Context()))
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, health)
}

func (s *server) handleKeywordSearchGovernance(w http.ResponseWriter, r *http.Request) {
	g, err := keywordSearchGovernance(r.Context(), s.db, scopeFromContext(r.Context()))
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *server) handleSecurityPosture(w http.ResponseWriter, r *http.Request) {
	limit := queryInt(r, "limit", 50)
	posture, err := securityPosture(r.Context(), s.db, scopeFromContext(r.Context()), limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, posture)
}

// handleWhoami lets the frontend show which scope it's actually looking
// at (DefaultUserID on Tier 1/2, a real user/team on Tier 3) without
// hardcoding tier-specific assumptions client-side.
func (s *server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	scope := scopeFromContext(r.Context())
	writeJSON(w, http.StatusOK, struct {
		ScopeKind  string `json:"scope_kind"`
		ScopeOwner string `json:"scope_owner"`
	}{scope.Kind, scope.Owner})
}

func queryInt(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// --- JSON helpers, identical to cmd/hupi-admin-ui/handlers.go's own —
// see that file's doc comments for why each exists. Duplicated rather
// than shared for the same reason assets.go's spaFallback is: two
// single-purpose main packages, no real reuse benefit from a shared
// internal/ package for a few lines each. ---------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("dashboard: encode response failed", "error", err)
	}
}

func jsonError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func internalError(w http.ResponseWriter, err error) {
	slog.Error("dashboard: request failed", "error", err)
	jsonError(w, http.StatusInternalServerError, "internal error — see server logs")
}
