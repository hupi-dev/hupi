// Command hupi-consolidate runs one daily consolidation pass, once per
// active scope — see ARCHITECTURE.md § Consolidation engine and
// docs/TIER3_PLAN.md §5. Intended to be invoked by cron once a day, not
// run as a long-lived process; it shares config/DB/key wiring with
// cmd/hupi via internal/bootstrap but is otherwise fully independent of
// the gateway server.
//
// Also runs weekly/monthly/yearly rollups (rollup.go) when the date being
// consolidated crosses one of those boundaries — see
// docs/GAP_CLOSURE_PLAN.md §4.1. No separate cron entry: computing which
// periods belong in a given week/month/year is calendar logic that
// belongs in this scheduling layer, not a second binary.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"hupi/internal/bootstrap"
	"hupi/internal/consolidation"
	"hupi/internal/identity"
)

func main() {
	if err := run(); err != nil {
		slog.Error("hupi-consolidate exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	dateFlag := flag.String("date", "", "date to consolidate, YYYY-MM-DD (default: yesterday)")
	flag.Parse()

	date := time.Now().AddDate(0, 0, -1)
	if *dateFlag != "" {
		parsed, err := time.Parse("2006-01-02", *dateFlag)
		if err != nil {
			return fmt.Errorf("invalid -date: %w", err)
		}
		date = parsed
	}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	runner := consolidation.New(
		deps.DB, deps.Keys,
		deps.Registry.Consolidation(),
		deps.Registry.Grounding(), // active_grounding_provider if set, else falls back to Consolidation's profile
		deps.Registry.Embedding(),
	)

	scopes, err := loadActiveScopes(ctx, deps.DB)
	if err != nil {
		return fmt.Errorf("load active scopes: %w", err)
	}

	period := date.Format("2006-01-02")
	var failed int
	for _, scope := range scopes {
		if err := runner.RunDaily(ctx, scope, date); err != nil {
			// One user's or team's bad day (a malformed LLM response, a
			// transient network error) shouldn't block everyone else's
			// consolidation — log and keep going, report the failure
			// count at the end.
			slog.Error("daily consolidation failed for scope",
				"scope_kind", scope.Kind, "scope_owner", scope.Owner, "date", period, "error", err)
			failed++
			continue
		}
	}

	slog.Info("daily consolidation complete", "date", period, "scopes", len(scopes), "failed", failed)

	rollupFailed := runDueRollups(ctx, runner, date, scopes)
	failed += rollupFailed

	if failed > 0 {
		return fmt.Errorf("consolidation failed for %d scope-runs on %s", failed, period)
	}
	return nil
}

// runDueRollups runs every rollup dueRollups finds for date, once per
// scope, with the same per-scope failure isolation as the daily loop
// above — a no-op call (no jobs due) logs nothing, so most days this is
// silent.
func runDueRollups(ctx context.Context, runner *consolidation.Runner, date time.Time, scopes []identity.Scope) int {
	jobs := dueRollups(date)
	var failed int
	for _, job := range jobs {
		for _, scope := range scopes {
			if err := runner.RunRollup(ctx, scope, job.level, job.sourceLevel, job.period, job.sourcePeriods); err != nil {
				slog.Error("rollup failed for scope",
					"scope_kind", scope.Kind, "scope_owner", scope.Owner,
					"level", job.level, "period", job.period, "error", err)
				failed++
				continue
			}
		}
		slog.Info("rollup complete", "level", job.level, "period", job.period, "scopes", len(scopes))
	}
	return failed
}

// loadActiveScopes enumerates every private scope (one per user) and
// shared scope (one per team) that could conceivably have episodes to
// consolidate. RunDaily itself is a no-op for a scope with nothing new
// that day, so this deliberately doesn't try to filter down to "scopes
// with activity" first — simpler, and the cost of a wasted no-op query is
// negligible next to an LLM call.
func loadActiveScopes(ctx context.Context, db *sql.DB) ([]identity.Scope, error) {
	var scopes []identity.Scope

	userRows, err := db.QueryContext(ctx, `select id from users`)
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	defer userRows.Close()
	for userRows.Next() {
		var id string
		if err := userRows.Scan(&id); err != nil {
			return nil, err
		}
		scopes = append(scopes, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: id})
	}
	if err := userRows.Err(); err != nil {
		return nil, err
	}

	teamRows, err := db.QueryContext(ctx, `select id from teams`)
	if err != nil {
		return nil, fmt.Errorf("load teams: %w", err)
	}
	defer teamRows.Close()
	for teamRows.Next() {
		var id string
		if err := teamRows.Scan(&id); err != nil {
			return nil, err
		}
		scopes = append(scopes, identity.Scope{Kind: identity.ScopeKindShared, Owner: id})
	}
	return scopes, teamRows.Err()
}
