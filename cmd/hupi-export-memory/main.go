// Command hupi-export-memory dumps a scope's entire decrypted memory
// store as JSON — every entity, every current summary with its key
// facts, every relationship. Built for external diagnostic tooling (see
// docs/EVALMEM_INTEGRATION_PLAN.md's export_full_memory requirement),
// not for interactive use the way cmd/hupi-trace is: hupi-trace answers
// "what did this one turn see," this answers "what does this scope know,
// full stop."
//
// Usage:
//
//	hupi-export-memory <user-or-team-id>
//	hupi-export-memory -scope-kind shared -scope-owner team:acme-eng
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"hupi/internal/bootstrap"
	"hupi/internal/identity"
	"hupi/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-export-memory:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", identity.ScopeKindPrivate, "private | shared")
	scopeOwner := flag.String("scope-owner", "", "user id (private) or team id (shared); required unless given positionally")
	actor := flag.String("actor", defaultActor(), "who's exporting — recorded on the audit log entry; defaults to $USER")
	flag.Parse()

	owner := *scopeOwner
	if owner == "" {
		if flag.NArg() != 1 {
			return fmt.Errorf("usage: hupi-export-memory [-scope-kind private|shared] [-scope-owner <id>] [-actor <name>] [<id>]")
		}
		owner = flag.Arg(0)
	}
	scope := identity.Scope{Kind: *scopeKind, Owner: owner}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	st := store.New(deps.DB, deps.Keys, deps.Registry.Embedding())
	export, err := st.ExportMemory(ctx, scope, *actor)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(export)
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
