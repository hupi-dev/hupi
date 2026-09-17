// Command hupi is the OpenAI-compatible gateway server — see
// ARCHITECTURE.md § Component overview. It answers /v1/chat/completions,
// wiring together retrieval, the provider registry, and capture
// (internal/gateway.Handler) backed by internal/store's Postgres
// implementation.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/gateway"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi gateway exited with error", "error", err)
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
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	teamAuth, err := resolveAuth(deps)
	if err != nil {
		return err
	}
	handler := &gateway.Handler{
		Registry:  deps.Registry,
		Retriever: st,
		Capturer:  st,
		Auth:      teamAuth,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler.HandleChatCompletions)
	mux.HandleFunc("POST /v1/feedback", handler.HandleFeedback)
	// Workspace routing (docs/TIER3_PLAN.md D4): a request to these routes
	// is scoped to the given team, authorized against the caller's own
	// membership. Only mounted when the Tier-3 extension is present (see
	// gateway.MountTeamRoutes) — this plain OSS build has no team routes
	// at all, not routes that always 403.
	if gateway.MountTeamRoutes != nil {
		gateway.MountTeamRoutes(mux, handler)
	}
	// docs/GAP_CLOSURE_PLAN.md §5: Kubernetes liveness/readiness probes,
	// not part of the OpenAI-compatible surface above — deliberately kept
	// out of internal/gateway, which owns that API, not deployment
	// plumbing. Split in two on purpose: liveness never touches the
	// database (a slow/degraded DB shouldn't get a healthy pod killed and
	// restarted, compounding the problem), readiness does (so a pod with
	// no DB connectivity stops receiving traffic without being restarted
	// for something a restart can't fix).
	mux.HandleFunc("GET /healthz", handleLiveness)
	mux.HandleFunc("GET /readyz", handleReadiness(deps.DB))

	addr := listenAddr()
	srv := &http.Server{Addr: addr, Handler: mux}

	// Binds to 127.0.0.1 by default — ARCHITECTURE.md § Resilience:
	// never network-visible unless HUPI_LISTEN_ADDR is deliberately set
	// to something else, and that path is expected to add its own auth.
	slog.Info("hupi gateway listening", "addr", addr)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func listenAddr() string {
	if a := os.Getenv("HUPI_LISTEN_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8787"
}

func handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func handleReadiness(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "database unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// resolveAuth returns nil (Tier 1/2 mode: no auth required, every request
// resolves to identity.DefaultScope) unless HUPI_REQUIRE_AUTH=true, in
// which case every request must carry a valid API key —
// docs/TIER3_PLAN.md Phase 3. Opt-in rather than auto-detected from the
// presence of users/api_keys rows, so an existing Tier 1/2 deployment that
// happens to have provisioned a user for testing doesn't suddenly start
// requiring auth on its next restart.
//
// Real auth requires the Tier-3 extension (auth.NewTeamAuthenticator) —
// erroring out here rather than silently returning nil is deliberate: an
// operator who explicitly asked for HUPI_REQUIRE_AUTH=true should see a
// clear "you need the Enterprise build" message, not have their request
// quietly ignored and Tier 1/2's no-auth behavior kick in instead.
func resolveAuth(deps *bootstrap.Deps) (gateway.Authenticator, error) {
	if os.Getenv("HUPI_REQUIRE_AUTH") != "true" {
		return nil, nil
	}
	if auth.NewTeamAuthenticator == nil {
		return nil, fmt.Errorf("HUPI_REQUIRE_AUTH=true requires the HUPI Enterprise build (Tier 3) — see docs/INSTALL.md")
	}
	return auth.NewTeamAuthenticator(deps.DB), nil
}
