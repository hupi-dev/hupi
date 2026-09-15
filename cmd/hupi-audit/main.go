// Command hupi-audit is the report surface over audit_log
// (schema/0007_audit_log.sql, docs/GAP_CLOSURE_PLAN.md §4.3) — the answer
// to "show me every time team X's data was accessed and by whom"
// (BUSINESS_PROCESS.md §7), covering writes, retrievals, investigation
// (hupi-trace), and admin actions (hupi-admin/hupi-admin-ui) uniformly,
// since all four write to the same table.
//
// Usage:
//
//	hupi-audit tail [-n 20]
//	hupi-audit query [-scope-kind private|shared] [-scope-owner <id>]
//	                 [-actor <name>] [-event-type <type>]
//	                 [-since <RFC3339>] [-until <RFC3339>] [-limit 100]
//
// Like hupi-trace, this is an operator CLI with full database access, not
// a request-scoped tool — docs/GAP_CLOSURE_PLAN.md §2's non-goals.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"hupi/internal/audit"
	"hupi/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-audit:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: hupi-audit <tail|query> [flags]")
	}
	subcommand, args := os.Args[1], os.Args[2:]

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	switch subcommand {
	case "tail":
		return runTail(ctx, deps.DB, args)
	case "query":
		return runQuery(ctx, deps.DB, args)
	default:
		return fmt.Errorf("unknown subcommand %q (want tail or query)", subcommand)
	}
}

func runTail(ctx context.Context, db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("tail", flag.ContinueOnError)
	n := fs.Int("n", 20, "how many of the most recent entries to show")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Descending order (audit.QueryFilter's default) fetches the *most
	// recent* n efficiently via LIMIT; printed oldest-first below, matching
	// what "tail" means for a log.
	entries, err := audit.Query(ctx, db, audit.QueryFilter{Limit: *n})
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		printEntry(entries[i])
	}
	return nil
}

func runQuery(ctx context.Context, db *sql.DB, args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	scopeKind := fs.String("scope-kind", "", "private | shared — filters workspace_scope_kind")
	scopeOwner := fs.String("scope-owner", "", "user id or team id — filters workspace_scope_owner")
	actor := fs.String("actor", "", "exact actor match")
	eventType := fs.String("event-type", "", "capture | correct | retrieve | trace | admin_provision | admin_ui_view")
	since := fs.String("since", "", "RFC3339 timestamp, inclusive lower bound")
	until := fs.String("until", "", "RFC3339 timestamp, exclusive upper bound")
	limit := fs.Int("limit", 100, "maximum rows to return")
	if err := fs.Parse(args); err != nil {
		return err
	}

	f := audit.QueryFilter{
		ScopeKind:  *scopeKind,
		ScopeOwner: *scopeOwner,
		Actor:      *actor,
		EventType:  *eventType,
		Limit:      *limit,
		Ascending:  true,
	}
	if *since != "" {
		ts, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return fmt.Errorf("-since: %w", err)
		}
		f.Since = &ts
	}
	if *until != "" {
		ts, err := time.Parse(time.RFC3339, *until)
		if err != nil {
			return fmt.Errorf("-until: %w", err)
		}
		f.Until = &ts
	}

	entries, err := audit.Query(ctx, db, f)
	if err != nil {
		return err
	}
	for _, e := range entries {
		printEntry(e)
	}
	fmt.Fprintf(os.Stderr, "%d row(s)\n", len(entries))
	return nil
}

func printEntry(e audit.LogEntry) {
	target := e.TargetRef
	if target == "" {
		target = "-"
	}
	fmt.Printf("%s  %-16s actor=%-24s acting=%s:%s workspace=%s:%s target=%s detail=%s\n",
		e.TS.Format(time.RFC3339), e.EventType, e.Actor,
		e.ActingScopeKind, e.ActingScopeOwner,
		e.WorkspaceScopeKind, e.WorkspaceScopeOwner,
		target, e.Detail,
	)
}
