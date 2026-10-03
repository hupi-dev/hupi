package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"hupi/internal/crypto"
	"hupi/internal/provider"
	"hupi/internal/store"
)

// server holds the dependencies every handler needs. Deliberately not
// exported, same reasoning as cmd/hupi-admin-ui's server type — never
// leaves package main.
type server struct {
	db       *sql.DB
	keys     *crypto.KeyStore   // only used by handleExport's store.New call
	registry *provider.Registry // used by handleExport (store.New needs an Embedder) and Phase 2's LLM-powered theme narrative, content_analysis.go
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
	mux.HandleFunc("GET /api/forgotten-but-important", s.handleForgottenButImportant)
	mux.HandleFunc("GET /api/keyword-search-governance", s.handleKeywordSearchGovernance)
	mux.HandleFunc("GET /api/security-posture", s.handleSecurityPosture)
	mux.HandleFunc("GET /api/export", s.handleExport)
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

func (s *server) handleForgottenButImportant(w http.ResponseWriter, r *http.Request) {
	minImportance := queryFloat(r, "min_importance", defaultForgottenMinImportance)
	staleDays := queryInt(r, "stale_days", defaultForgottenStaleDays)
	limit := queryInt(r, "limit", 20)
	result, err := forgottenButImportant(r.Context(), s.db, scopeFromContext(r.Context()), minImportance, staleDays, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
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

// handleExport is the "Export" button's backend: reuses
// internal/store.Store.ExportMemory exactly as cmd/hupi-export-memory
// does (same store.New(db, keys, embedder) construction) rather than
// reimplementing any decrypt-and-serialize logic here — this handler is
// pure HTTP glue (construct the Store, call ExportMemory, stream the
// result with a download-triggering header). ExportMemory already writes
// its own audit_log entry (see that method's doc comment), so there's
// nothing extra to log here. actor is the scope's own owner — the only
// identity this dashboard actually has for Tier 1/2, and the correct one
// for Tier 3 too once hupi-t3's session resolution sets scope to the real
// signed-in user.
func (s *server) handleExport(w http.ResponseWriter, r *http.Request) {
	scope := scopeFromContext(r.Context())
	st := store.New(s.db, s.keys, s.registry.Embedding())
	export, err := st.ExportMemory(r.Context(), scope, scope.Owner)
	if err != nil {
		internalError(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="hupi-export-%s.json"`, scope.Owner))
	writeJSON(w, http.StatusOK, export)
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

func queryFloat(r *http.Request, key string, def float64) float64 {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
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
