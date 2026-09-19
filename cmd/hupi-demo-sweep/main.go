// Command hupi-demo-sweep deletes expired hosted-demo guest sessions
// (internal/demo.Store.Sweep) and everything they wrote — see
// docs/TODO.md #2. Intended to be invoked by cron every 15 minutes or so
// (docs/INSTALL.md § Cron jobs), not run as a long-lived process, same
// shape as cmd/hupi-consolidate.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/demo"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-demo-sweep exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	authStore := auth.New(deps.DB, deps.Keys)
	// Sweep never consolidates, so the ConsolidationRunner passed here is
	// never called — nil is fine for this interface value.
	demoStore := demo.New(deps.DB, authStore, nil, demo.DefaultLimits)

	swept, err := demoStore.Sweep(ctx, 24*time.Hour)
	if err != nil {
		return err
	}
	slog.Info("hupi-demo-sweep complete", "sessions_swept", swept)
	return nil
}
