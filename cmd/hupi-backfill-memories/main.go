// Command hupi-backfill-memories populates the new `memories` table
// (schema/0025_unified_memories.sql) from one scope's existing
// entities.attributes and summary_key_facts rows — Phase 0 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md. See
// internal/backfillmemories's own doc comment for why this is safe to
// run against live data: read-only against the old tables, idempotent
// and resumable against the new one.
//
// Usage:
//
//	hupi-backfill-memories -scope-kind private -scope-owner user:alice
//	hupi-backfill-memories -scope-kind shared -scope-owner team:acme-eng -status
//
// Safe to kill and re-run, same as hupi-reembed — see
// internal/backfillmemories's own doc comment for why no persisted
// progress cursor is needed.
//
// Like hupi-rotate-key/hupi-reembed/hupi-admin/hupi-trace, this is an
// operator CLI with full database access, not a request-scoped tool —
// docs/GAP_CLOSURE_PLAN.md §2's non-goals.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"hupi/internal/backfillmemories"
	"hupi/internal/bootstrap"
	"hupi/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-backfill-memories:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", "", "private | shared (required)")
	scopeOwner := flag.String("scope-owner", "", "user id or team id (required)")
	status := flag.Bool("status", false, "print how many rows still need backfilling and exit, without doing any work")
	batchSize := flag.Int("batch-size", 200, "source rows to backfill per batch")
	actor := flag.String("actor", defaultActor(), "who's running this (audit log)")
	flag.Parse()

	if *scopeKind == "" || *scopeOwner == "" {
		return fmt.Errorf("usage: hupi-backfill-memories -scope-kind private|shared -scope-owner <id> [-status | -batch-size N]")
	}
	scope := identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	runner := backfillmemories.New(deps.DB, deps.Keys)

	if *status {
		return printStatus(ctx, runner, scope)
	}

	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("backfilling %s:%s: %d attribute entit(ies), %d key fact(s) pending\n",
		scope.Kind, scope.Owner, st.AttributeEntitiesPending, st.KeyFactsPending)

	total, err := backfillAll(ctx, runner, scope, *actor, *batchSize, func(processed int, table string, runningTotal int) {
		fmt.Printf("  backfilled %d from %s (%d total)\n", processed, table, runningTotal)
	})
	if err != nil {
		return err
	}
	fmt.Printf("backfill complete: %s:%s (%d row(s) backfilled)\n", scope.Kind, scope.Owner, total)
	return nil
}

// backfillAll drives runner.Continue to completion, auditing each
// non-empty batch immediately after it commits rather than accumulating
// one summary entry for the very end — same reasoning, and the same
// real bug class it avoids, as cmd/hupi-reembed's own reembedAll (see
// that function's doc comment: a kill between a committed batch and a
// final "if total > 0" check would otherwise leave real, committed data
// changes with no audit trail at all, and the next invocation's own
// total starting fresh at 0 would never retroactively cover for it).
func backfillAll(ctx context.Context, runner *backfillmemories.Runner, scope identity.Scope, actor string, batchSize int, onBatch func(processed int, table string, runningTotal int)) (int, error) {
	total := 0
	for {
		processed, table, done, err := runner.Continue(ctx, scope, batchSize)
		if err != nil {
			return total, err
		}
		total += processed
		if processed > 0 {
			if onBatch != nil {
				onBatch(processed, table, total)
			}
			if err := runner.LogRun(ctx, scope, actor, processed, table); err != nil {
				return total, fmt.Errorf("backfilled %d from %s but failed to write audit log entry: %w", processed, table, err)
			}
		}
		if done {
			return total, nil
		}
	}
}

func printStatus(ctx context.Context, runner *backfillmemories.Runner, scope identity.Scope) error {
	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("%s:%s: pending: %d attribute entit(ies), %d key fact(s) (%d total)\n",
		scope.Kind, scope.Owner, st.AttributeEntitiesPending, st.KeyFactsPending, st.Total())
	return nil
}

func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}
