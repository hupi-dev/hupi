// Command hupi-reembed re-embeds one scope's summaries, high-importance
// episodes, and entities that either never got a vector or got one from
// an embedding model that isn't the one currently configured
// (providers.yaml's active_embedding_provider) — see internal/reembed's
// doc comment for why a provider switch needs this: cosine similarity
// between vectors from two different models is closer to noise than a
// real signal, so a stale-model vector is worse than no vector at all.
//
// Usage:
//
//	hupi-reembed -scope-kind private -scope-owner user:alice
//	hupi-reembed -scope-kind shared -scope-owner team:acme-eng -status
//
// Safe to kill and re-run: unlike hupi-rotate-key, this has no persisted
// progress row to resume from because it doesn't need one — see
// internal/reembed's doc comment.
//
// Like hupi-rotate-key/hupi-admin/hupi-trace, this is an operator CLI
// with full database access, not a request-scoped tool —
// docs/GAP_CLOSURE_PLAN.md §2's non-goals.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"hupi/internal/bootstrap"
	"hupi/internal/identity"
	"hupi/internal/reembed"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-reembed:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", "", "private | shared (required)")
	scopeOwner := flag.String("scope-owner", "", "user id or team id (required)")
	status := flag.Bool("status", false, "print how many rows still need re-embedding and exit, without doing any work")
	batchSize := flag.Int("batch-size", 100, "rows to re-embed per batch (each row is one embedding-provider call)")
	actor := flag.String("actor", defaultActor(), "who's running this (audit log)")
	flag.Parse()

	if *scopeKind == "" || *scopeOwner == "" {
		return fmt.Errorf("usage: hupi-reembed -scope-kind private|shared -scope-owner <id> [-status | -batch-size N]")
	}
	scope := identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	runner := reembed.New(deps.DB, deps.Keys, deps.Registry.Embedding())

	if *status {
		return printStatus(ctx, runner, scope)
	}

	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("re-embedding %s:%s to %s: %d summar(ies), %d episode(s), %d entit(ies) pending\n",
		scope.Kind, scope.Owner, st.Model, st.SummariesPending, st.EpisodesPending, st.EntitiesPending)

	total := 0
	for {
		processed, table, done, err := runner.Continue(ctx, scope, *batchSize)
		if err != nil {
			return err
		}
		total += processed
		if processed > 0 {
			fmt.Printf("  re-embedded %d %s (%d total)\n", processed, table, total)
		}
		if done {
			break
		}
	}

	if total > 0 {
		if err := runner.LogRun(ctx, scope, *actor, total); err != nil {
			return fmt.Errorf("re-embedding finished (%d rows) but failed to write audit log entry: %w", total, err)
		}
	}
	fmt.Printf("re-embedding complete: %s:%s now on %s (%d row(s) re-embedded)\n", scope.Kind, scope.Owner, st.Model, total)
	return nil
}

func printStatus(ctx context.Context, runner *reembed.Runner, scope identity.Scope) error {
	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("%s:%s: current model=%s, pending: %d summar(ies), %d episode(s), %d entit(ies) (%d total)\n",
		scope.Kind, scope.Owner, st.Model, st.SummariesPending, st.EpisodesPending, st.EntitiesPending, st.Total())
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
