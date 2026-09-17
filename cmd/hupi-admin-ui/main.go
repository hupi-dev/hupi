// Command hupi-admin-ui is a browser-based operator surface over exactly
// the same internal/auth.Store/TeamStore operations cmd/hupi-admin
// already exposes as a CLI, plus the read/revoke operations a UI needs
// that the create-only CLI never did (list users/teams/keys, revoke a
// key).
//
// This exists despite docs/TIER3_PLAN.md's original non-goal ("no
// team-management UI... a thin CLI, not a product surface") — see
// docs/ADMIN_UI.md for why that call was reversed and what this
// deliberately does *not* do (it never reads or displays memory content;
// hupi-trace still owns that).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-admin-ui exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	authStore := auth.New(deps.DB, deps.Keys)
	teamStore := auth.NewTeamStore(deps.DB)
	srv := &server{store: authStore, teamStore: teamStore, db: deps.DB}

	// /api/* is the JSON API (handlers.go), behind both the CSRF mitigation
	// and named-operator auth, exactly as before this rewrite. Everything
	// else falls through to the placeholder SPA handler (assets.go), which
	// is deliberately NOT behind requireOperatorAuth — a future frontend's
	// login screen has to be reachable before there's a credential to send.
	top := http.NewServeMux()
	top.Handle("/api/", requireCSRFSafe(requireOperatorAuth(authStore, srv.routes())))
	top.HandleFunc("/", serveSPA)
	var handler http.Handler = top

	addr := listenAddr()
	httpServer := &http.Server{Addr: addr, Handler: handler}

	// Binds to 127.0.0.1 by default, same posture as cmd/hupi — this is
	// an operator tool with no CSRF token (see requireCSRFSafe's doc
	// comment for why that's an acceptable trade here), so it should sit
	// behind a trusted operator's own machine or a reverse proxy with its
	// own auth, not be exposed directly to the internet. Every view and
	// action is still logged to audit_log by named operator, per
	// docs/GAP_CLOSURE_PLAN.md §4.3 — provision credentials with
	// `hupi-admin create-operator`, not a shared token.
	slog.Info("hupi-admin-ui listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func listenAddr() string {
	if a := os.Getenv("HUPI_ADMIN_UI_LISTEN_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8788"
}

// operatorContextKey is unexported by design — the only way to read the
// authenticated operator's name back out of a request context is
// operatorFromContext, so every handler goes through the same accessor.
type operatorContextKey struct{}

// operatorFromContext returns "unknown" rather than panicking or empty
// string if called on a context requireOperatorAuth never touched (e.g.
// a future handler added without going through routes()) — audit_log.actor
// is `not null`, and a wrong-but-recognizable placeholder is more useful
// for catching that mistake later than a write that silently fails or a
// panic that takes the whole request down.
func operatorFromContext(ctx context.Context) string {
	if name, ok := ctx.Value(operatorContextKey{}).(string); ok {
		return name
	}
	return "unknown"
}

// requireOperatorAuth authenticates against named operator credentials
// (schema/0008_admin_operators.sql) rather than the single shared token
// this used to be — see docs/GAP_CLOSURE_PLAN.md §4.3: the audit level
// this project chose explicitly includes admin access, which is
// meaningless if every operator is indistinguishable. The password field
// of HTTP Basic Auth carries the raw operator token; auth.Store.ResolveOperator
// does the hash lookup, the same pattern api_keys/Resolve already uses. If
// a username is also given, it must match the resolved operator's name —
// not required (some clients don't populate it), but checked when
// present, catching a copy-pasted-someone-else's-token mistake.
func requireOperatorAuth(store *auth.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			unauthorized(w)
			return
		}
		name, err := store.ResolveOperator(r.Context(), pass)
		if err != nil {
			unauthorized(w)
			return
		}
		if user != "" && user != name {
			unauthorized(w)
			return
		}
		ctx := context.WithValue(r.Context(), operatorContextKey{}, name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="hupi-admin-ui"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// requireCSRFSafe rejects state-changing requests that a browser marks as
// cross-site. Basic Auth credentials are cached per-origin and resent
// automatically by the browser on same-origin requests, including ones
// triggered by a form on a different site — the classic Basic-auth CSRF
// gap, since there's no session cookie to scope a token to. Modern
// browsers attach Sec-Fetch-Site to every request; anything other than
// "same-origin" (or absent, e.g. curl/older clients — allowed through
// since this is a defense-in-depth layer, not the only one) on a POST is
// rejected. This does not replace running the UI somewhere only a trusted
// operator can reach it, which is why it binds to 127.0.0.1 by default.
func requireCSRFSafe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
