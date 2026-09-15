// Command hupi-trace prints exactly what a given turn's retrieval engine
// saw and injected — ARCHITECTURE.md § Retrieval observability's "grep,
// not an unanswerable question." Usage:
//
//	hupi-trace <episode_id>
//	hupi-trace -scope-kind shared -scope-owner team:acme-eng <episode_id>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"hupi/internal/bootstrap"
	"hupi/internal/identity"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-trace:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", identity.ScopeKindPrivate, "private | shared")
	scopeOwner := flag.String("scope-owner", identity.DefaultUserID, "user id (private) or team id (shared)")
	actor := flag.String("actor", defaultActor(), "who's investigating — recorded on the audit log entry (docs/GAP_CLOSURE_PLAN.md §4.3); defaults to $USER")
	flag.Parse()

	if flag.NArg() != 1 {
		return fmt.Errorf("usage: hupi-trace [-scope-kind private|shared] [-scope-owner <id>] [-actor <name>] <episode_id>")
	}
	episodeID := flag.Arg(0)
	scope := identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	trace, err := st.Trace(ctx, scope, episodeID, *actor)
	if err != nil {
		return err
	}

	printTrace(trace)
	return nil
}

// defaultActor gives -actor a sensible default without forcing every
// invocation to type it — real accountability needs someone to type
// their own name, but a CLI that hangs on a missing flag isn't better.
func defaultActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("LOGNAME"); u != "" {
		return u
	}
	return "unknown"
}

func printTrace(t store.Trace) {
	e := t.Episode
	fmt.Printf("episode:      %s\n", e.ID)
	fmt.Printf("scope:        %s:%s\n", e.Scope.Kind, e.Scope.Owner)
	fmt.Printf("ts:           %s\n", e.TS.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Printf("memory_gate:  %s\n", displayOr(e.MemoryGate, "(none — feedback row or opted out)"))
	fmt.Printf("\ninput:\n  %s\n", e.InputText)
	fmt.Printf("\noutput:\n  %s\n", e.OutputText)

	fmt.Printf("\nretrieved summaries (%d):\n", len(t.RetrievedSummaries))
	for _, s := range t.RetrievedSummaries {
		fmt.Printf("  - %s [scope=%s:%s, %s %s, grounding_checked=%v]\n    %s\n",
			s.Ref.ID, s.Ref.Scope.Kind, s.Ref.Scope.Owner, s.Level, s.Period, s.GroundingChecked, s.Text)
	}

	fmt.Printf("\nretrieved entities (%d):\n", len(t.RetrievedEntities))
	for _, en := range t.RetrievedEntities {
		fmt.Printf("  - %s (%s, kind=%s, scope=%s:%s)\n    %s\n",
			en.Ref.ID, en.Name, en.Kind, en.Ref.Scope.Kind, en.Ref.Scope.Owner, en.Attributes)
	}

	fmt.Printf("\nretrieved episodes (%d):\n", len(t.RetrievedEpisodes))
	for _, eh := range t.RetrievedEpisodes {
		fmt.Printf("  - %s (scope=%s:%s)\n    USER: %s\n    ASSISTANT: %s\n",
			eh.Ref.ID, eh.Ref.Scope.Kind, eh.Ref.Scope.Owner, eh.InputText, eh.OutputText)
	}
}

func displayOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
