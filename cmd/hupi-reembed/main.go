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
	batchSize := flag.Int("batch-size", 100, "rows to re-embed per batch (one embedding-provider call per row, except summary_key_facts, which embeds a whole batch in a single call)")
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
	if err := bootstrap.VerifyEmbedding(ctx, deps); err != nil {
		return err
	}

	runner := reembed.New(deps.DB, deps.Keys, deps.Registry.Embedding())

	if *status {
		return printStatus(ctx, runner, scope)
	}

	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("re-embedding %s:%s to %s: %d summar(ies), %d episode(s), %d entit(ies), %d key fact(s) pending\n",
		scope.Kind, scope.Owner, st.Model, st.SummariesPending, st.EpisodesPending, st.EntitiesPending, st.KeyFactsPending)

	total, err := reembedAll(ctx, runner, scope, *actor, *batchSize, func(processed int, table string, runningTotal int) {
		fmt.Printf("  re-embedded %d %s (%d total)\n", processed, table, runningTotal)
	})
	if err != nil {
		return err
	}
	fmt.Printf("re-embedding complete: %s:%s now on %s (%d row(s) re-embedded)\n", scope.Kind, scope.Owner, st.Model, total)
	return nil
}

// reembedAll drives runner.Continue to completion, logging each batch's
// own audit entry immediately after that batch commits, rather than
// accumulating a total and writing one summary entry at the very end —
// a real, confirmed bug this used to have
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A9): if the process was
// killed after real batches had already committed real embedding writes
// but before reaching a final "if total > 0" check, that work was never
// audited — and the *next* invocation's own total starts at 0 and finds
// nothing pending (the work already happened), so LogRun was never
// called for it, permanently. Auditing per batch means there's no
// window where real, committed data changes can exist with zero audit
// trail: every batch that did real work gets its own entry before the
// next batch even begins. onBatch is called after each non-empty batch,
// purely for progress output — nil is fine if the caller doesn't want it.
func reembedAll(ctx context.Context, runner *reembed.Runner, scope identity.Scope, actor string, batchSize int, onBatch func(processed int, table string, runningTotal int)) (int, error) {
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
			if err := runner.LogRun(ctx, scope, actor, processed); err != nil {
				return total, fmt.Errorf("re-embedded %d %s but failed to write audit log entry: %w", processed, table, err)
			}
		}
		if done {
			return total, nil
		}
	}
}

func printStatus(ctx context.Context, runner *reembed.Runner, scope identity.Scope) error {
	st, err := runner.Status(ctx, scope)
	if err != nil {
		return err
	}
	fmt.Printf("%s:%s: current model=%s, pending: %d summar(ies), %d episode(s), %d entit(ies), %d key fact(s) (%d total)\n",
		scope.Kind, scope.Owner, st.Model, st.SummariesPending, st.EpisodesPending, st.EntitiesPending, st.KeyFactsPending, st.Total())
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
