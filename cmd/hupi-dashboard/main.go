// Command hupi-dashboard is a read-only analytics surface over a scope's
// own memory: conversation volume, a theme word cloud, entity
// relationships, memory health, retrieval governance transparency, and a
// security posture panel (key rotation history, recent audit events).
// See docs/DASHBOARD.md for the full feature list and design rationale.
//
// Tier 1/2 (this OSS build): no login at all, every request resolves to
// identity.DefaultScope — the same zero-auth posture cmd/hupi's own
// gateway already has when h.Auth is nil (internal/gateway/handler.go's
// resolveIdentity). Tier 3 (hupi-dashboard/team.go, hupi-t3 only) adds
// real sign-in (password or OIDC) and team-wide views via the
// nil-by-default hook vars below — the same open-core pattern
// cmd/hupi-admin-ui/handlers.go already uses for mountTeamRoutes.
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

	"hupi/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-dashboard exited with error", "error", err)
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

	srv := &server{db: deps.DB, registry: deps.Registry}

	top := http.NewServeMux()
	top.Handle("/api/", requireDashboardSession(srv.routes()))
	// Auth routes (password/OIDC login) are mounted directly on top,
	// outside requireDashboardSession — same reason assets.go's SPA
	// handler sits outside auth in cmd/hupi-admin-ui: a login screen has
	// to be reachable before there's a credential to send. nil in the
	// OSS build, same as mountTeamRoutes.
	if mountDashboardAuthRoutes != nil {
		mountDashboardAuthRoutes(top, srv)
	}
	top.HandleFunc("/", serveSPA)

	addr := listenAddr()
	httpServer := &http.Server{Addr: addr, Handler: top}

	// 127.0.0.1 by default, same posture as cmd/hupi-admin-ui — this
	// dashboard reads real (if mostly plaintext) data about one scope's
	// memory; it should sit behind a trusted user's own machine or a
	// reverse proxy, not be exposed directly to the internet.
	slog.Info("hupi-dashboard listening", "addr", addr)

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
	if a := os.Getenv("HUPI_DASHBOARD_LISTEN_ADDR"); a != "" {
		return a
	}
	return "127.0.0.1:8790"
}
