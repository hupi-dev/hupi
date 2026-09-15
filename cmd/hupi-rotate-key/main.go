// Command hupi-rotate-key rotates one scope's data-encryption key online
// — no downtime, safe to run while the gateway keeps serving that scope's
// traffic — and resumably: killing this process and re-running the same
// command picks up where it left off rather than restarting or
// corrupting already-migrated rows. See docs/GAP_CLOSURE_PLAN.md §4.4 and
// internal/rotate's doc comment for how.
//
// Usage:
//
//	hupi-rotate-key -scope-kind private -scope-owner user:alice
//	hupi-rotate-key -scope-kind shared -scope-owner team:acme-eng -status
//	hupi-rotate-key -scope-kind shared -scope-owner team:acme-eng -prune-old-versions
//
// Like hupi-admin/hupi-trace, this is an operator CLI with full database
// access, not a request-scoped tool — docs/GAP_CLOSURE_PLAN.md §2's
// non-goals.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"hupi/internal/bootstrap"
	"hupi/internal/identity"
	"hupi/internal/rotate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-rotate-key:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", "", "private | shared (required)")
	scopeOwner := flag.String("scope-owner", "", "user id or team id (required)")
	status := flag.Bool("status", false, "print rotation status and exit, without doing any work")
	prune := flag.Bool("prune-old-versions", false, "delete key material older than the completed rotation's target version, then exit")
	batchSize := flag.Int("batch-size", 500, "rows to migrate per batch/transaction")
	actor := flag.String("actor", defaultActor(), "who's running this (audit log)")
	flag.Parse()

	if *scopeKind == "" || *scopeOwner == "" {
		return fmt.Errorf("usage: hupi-rotate-key -scope-kind private|shared -scope-owner <id> [-status | -prune-old-versions | -batch-size N]")
	}
	scope := identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	runner := rotate.New(deps.DB, deps.Keys)

	if *status {
		return printStatus(ctx, runner, scope)
	}
	if *prune {
		pruned, err := runner.Prune(ctx, scope, *actor)
		if err != nil {
			return err
		}
		fmt.Printf("pruned %d old key version(s) for %s:%s\n", pruned, scope.Kind, scope.Owner)
		fmt.Println("if this rotation was prompted by a suspected compromise, also restart every other process sharing this database (the gateway, hupi-consolidate) — pruning only removes the persisted key, not an already-running process's own memory")
		return nil
	}

	fromVersion, toVersion, err := runner.Start(ctx, scope, *actor)
	if err != nil {
		return err
	}
	fmt.Printf("rotating %s:%s: v%d -> v%d\n", scope.Kind, scope.Owner, fromVersion, toVersion)

	total := 0
	for {
		processed, done, err := runner.Continue(ctx, scope, *batchSize, *actor)
		if err != nil {
			return err
		}
		total += processed
		if processed > 0 {
			fmt.Printf("  migrated %d row(s) (%d total)\n", processed, total)
		}
		if done {
			break
		}
	}
	fmt.Printf("rotation complete: %s:%s is now on v%d (%d row(s) migrated)\n", scope.Kind, scope.Owner, toVersion, total)
	fmt.Println("old key material was kept, not deleted — see -prune-old-versions once you're confident nothing needs it")
	return nil
}

func printStatus(ctx context.Context, runner *rotate.Runner, scope identity.Scope) error {
	st, ok, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Printf("%s:%s has never been rotated\n", scope.Kind, scope.Owner)
		return nil
	}
	fmt.Printf("%s:%s: v%d -> v%d, status=%s, cursor=%s:%s, started=%s",
		scope.Kind, scope.Owner, st.FromVersion, st.ToVersion, st.Status, st.CursorTable, st.CursorID,
		st.StartedAt.Format("2006-01-02T15:04:05Z07:00"))
	if st.CompletedAt != nil {
		fmt.Printf(", completed=%s", st.CompletedAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	fmt.Println()
	return nil
}

// defaultActor gives -actor a sensible default without forcing every
// invocation to type it.
func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}
