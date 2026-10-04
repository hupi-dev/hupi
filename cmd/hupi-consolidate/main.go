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
//
// Re-running this for an already-consolidated date regenerates from that
// day's full episode set and supersedes the existing draft, rather than
// creating a second, unrelated one — safe to fire more than once as a
// day accumulates more episodes. It is NOT a way to fix a wrong fact: the
// regeneration is told to trust the existing draft as already-settled
// (so a real hupi-correct survives being re-consolidated on top of), so
// a wrong draft stays wrong across every future run until you correct it
// yourself with hupi-correct — see MEMORY_FORMAT.md § Grounding &
// correction's operator caveat.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	"golang.org/x/sync/errgroup"

	"hupi/internal/bootstrap"
	"hupi/internal/consolidation"
	"hupi/internal/identity"
	"hupi/internal/metrics"
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
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

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
	failed := runScopesConcurrently(scopes, func(scope identity.Scope) error {
		if err := runSafely(func() error { return runner.RunDaily(ctx, scope, date) }); err != nil {
			slog.Error("daily consolidation failed for scope",
				"scope_kind", scope.Kind, "scope_owner", scope.Owner, "date", period, "error", err)
			return err
		}
		return nil
	})

	slog.Info("daily consolidation complete", "date", period, "scopes", len(scopes), "failed", failed)

	rollupFailed := runDueRollups(ctx, runner, date, scopes)
	totalFailed := failed + rollupFailed

	pushMetrics()

	if totalFailed > 0 {
		return fmt.Errorf("consolidation failed for %d scope-runs on %s", totalFailed, period)
	}
	return nil
}

// runScopesConcurrently runs fn once per scope, bounded by
// consolidateConcurrency, and returns how many calls failed. One scope's
// error is isolated from every other's — the same "one user's or team's
// bad day (a malformed LLM response, a transient network error)
// shouldn't block everyone else's consolidation" intent the sequential
// version of this loop already had, now with real concurrency rather
// than a scope-by-scope wait. fn's own error is intentionally never
// returned to the errgroup itself: errgroup.Group with SetLimit cancels
// remaining work on the first non-nil error from any goroutine, which
// would silently reintroduce "one bad scope blocks everyone else" —
// fn is expected to log its own error (callers already do, with
// scope-specific context an int count alone can't carry) and this just
// tallies how many did.
//
// A real, confirmed production concern motivates this existing at all,
// not just a hypothetical: before this, every scope was processed one at
// a time with zero concurrency, so nightly runtime scaled linearly with
// the number of active users/teams — found while investigating why a
// LongMemEval benchmark run (which exercises this same code path, just
// with many synthetic scopes sharing one database) kept slowing down as
// more scopes accumulated. Every dependency a scope's work touches is
// already safe for concurrent use across different scopes: *sql.DB pools
// its own connections, crypto.KeyStore has its own internal mutex (it
// already had to be concurrency-safe for cmd/hupi's own concurrent HTTP
// handlers), and consolidation.Runner holds no other mutable per-call
// state.
func runScopesConcurrently(scopes []identity.Scope, fn func(identity.Scope) error) int {
	var failed atomic.Int64
	g := new(errgroup.Group)
	g.SetLimit(consolidateConcurrency())
	for _, scope := range scopes {
		g.Go(func() error {
			if err := fn(scope); err != nil {
				failed.Add(1)
			}
			return nil
		})
	}
	g.Wait() // nolint:errcheck — the g.Go func above always returns nil; failures are counted separately
	return int(failed.Load())
}

// consolidateConcurrency bounds runScopesConcurrently's fan-out (see its
// own doc comment for why this exists at all). Defaults conservative
// rather than maximal — raising it is an operator tuning decision
// (balancing nightly-job wall-clock time against LLM provider rate
// limits and Postgres's own max_connections), not something this code
// should guess aggressively on their behalf.
func consolidateConcurrency() int {
	const defaultConsolidateConcurrency = 5
	v := os.Getenv("HUPI_CONSOLIDATE_CONCURRENCY")
	if v == "" {
		return defaultConsolidateConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("HUPI_CONSOLIDATE_CONCURRENCY is not a positive integer, using default", "value", v, "default", defaultConsolidateConcurrency)
		return defaultConsolidateConcurrency
	}
	return n
}

// runSafely isolates one scope's consolidation/rollup call from a panic
// in f — both per-scope loops in this file already log-and-continue on a
// returned error specifically so one user's or team's bad day can't block
// anyone else's, but a panic would still unwind straight past that and
// crash the whole batch, taking every other scope down with it (review
// finding C4: a real gap in that stated design intent, even though no
// concrete panic path exists today — this is a defensive backstop against
// a future one).
func runSafely(f func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f()
}

// pushMetrics sends this run's consolidation/grounding/rollup metrics
// (internal/metrics) to a Prometheus Pushgateway, if configured. Unlike
// cmd/hupi (a long-lived server a scraper can poll), this is a short-lived
// cron invocation — by the time anything could scrape it, the process has
// already exited, so a pull-based /metrics endpoint here would be
// pointless. Pushgateway is Prometheus's own documented answer to exactly
// this "batch job" case. Entirely optional: HUPI_PUSHGATEWAY_URL unset
// means no push attempted, and a push failure only logs a warning — an
// observability side-channel failing shouldn't fail the actual
// consolidation run.
func pushMetrics() {
	url := os.Getenv("HUPI_PUSHGATEWAY_URL")
	if url == "" {
		return
	}
	if err := push.New(url, "hupi_consolidate").Gatherer(prometheus.DefaultGatherer).Push(); err != nil {
		slog.Warn("failed to push metrics to HUPI_PUSHGATEWAY_URL", "url", url, "error", err)
	}
}

// runDueRollups runs every rollup dueRollups finds for date, once per
// scope, with the same per-scope failure isolation as the daily loop
// above — a no-op call (no jobs due) logs nothing, so most days this is
// silent.
func runDueRollups(ctx context.Context, runner *consolidation.Runner, date time.Time, scopes []identity.Scope) int {
	jobs := dueRollups(date)
	var failed int
	for _, job := range jobs {
		failed += runScopesConcurrently(scopes, func(scope identity.Scope) error {
			if err := runSafely(func() error {
				return runner.RunRollup(ctx, scope, job.level, job.sourceLevel, job.period, job.sourcePeriods)
			}); err != nil {
				metrics.RollupRunsTotal.WithLabelValues("error").Inc()
				slog.Error("rollup failed for scope",
					"scope_kind", scope.Kind, "scope_owner", scope.Owner,
					"level", job.level, "period", job.period, "error", err)
				return err
			}
			metrics.RollupRunsTotal.WithLabelValues("ok").Inc()
			return nil
		})
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
