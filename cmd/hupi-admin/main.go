// Command hupi-admin provisions identity — users, teams, memberships,
// API keys, and (docs/GAP_CLOSURE_PLAN.md §4.3) admin-UI operator
// credentials — see docs/TIER3_PLAN.md's non-goals ("no team-management
// UI... direct DB writes or a thin CLI"). This is that thin CLI: every
// subcommand is a direct call into internal/auth.Store or
// internal/auth.TeamStore, nothing more, plus an audit_log entry per
// docs/GAP_CLOSURE_PLAN.md §4.3. add-member/create-key's bodies live in
// team.go, not here — see that file's doc comment.
//
// Usage:
//
//	hupi-admin create-user     -id user:alice -email alice@example.com
//	hupi-admin create-team     -id team:acme-eng -name "Acme Eng"
//	hupi-admin add-member      -team team:acme-eng -user user:alice [-role admin]
//	hupi-admin create-key      -user user:alice
//	hupi-admin create-operator -name alice
//	hupi-admin revoke-operator -name alice
//
// Every subcommand accepts -actor (default: $USER) to name whoever's
// running it, recorded on the audit_log entry.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"hupi/internal/audit"
	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-admin:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: hupi-admin <create-user|create-team|add-member|create-key|create-operator|revoke-operator> [flags]")
	}
	subcommand, args := os.Args[1], os.Args[2:]

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	store := auth.New(deps.DB, deps.Keys)
	teamStore := auth.NewTeamStore(deps.DB)

	switch subcommand {
	case "create-user":
		fs := flag.NewFlagSet("create-user", flag.ContinueOnError)
		id := fs.String("id", "", "user id, e.g. user:alice (required)")
		email := fs.String("email", "", "email (optional)")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *id == "" {
			return fmt.Errorf("create-user: -id is required")
		}
		if err := store.CreateUser(ctx, *id, *email); err != nil {
			return err
		}
		scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: *id}
		logAdminAction(ctx, deps, *actor, scope, "create-user", map[string]any{"user_id": *id})
		fmt.Printf("created user %s\n", *id)

	case "create-team":
		fs := flag.NewFlagSet("create-team", flag.ContinueOnError)
		id := fs.String("id", "", "team id, e.g. team:acme-eng (required)")
		name := fs.String("name", "", "display name (required)")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *id == "" || *name == "" {
			return fmt.Errorf("create-team: -id and -name are required")
		}
		if err := store.CreateTeam(ctx, *id, *name); err != nil {
			return err
		}
		scope := identity.Scope{Kind: identity.ScopeKindShared, Owner: *id}
		logAdminAction(ctx, deps, *actor, scope, "create-team", map[string]any{"team_id": *id, "name": *name})
		fmt.Printf("created team %s (%s)\n", *id, *name)

	case "add-member":
		return runAddMember(ctx, deps, teamStore, args)

	case "create-key":
		return runCreateKey(ctx, deps, teamStore, args)

	case "create-operator":
		fs := flag.NewFlagSet("create-operator", flag.ContinueOnError)
		name := fs.String("name", "", "operator name — recorded as audit_log.actor for everything they do in the admin UI (required)")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("create-operator: -name is required")
		}
		rawToken, err := store.CreateOperator(ctx, *name)
		if err != nil {
			return err
		}
		// No memory scope is naturally associated with provisioning an
		// operator credential (unlike users/teams/keys above) —
		// identity.DefaultScope is this project's existing convention for
		// "no real scope applies," not a new one introduced here.
		logAdminAction(ctx, deps, *actor, identity.DefaultScope, "create-operator", map[string]any{"operator_name": *name})
		fmt.Printf("operator token for %s (save this now, it cannot be shown again):\n%s\n", *name, rawToken)

	case "revoke-operator":
		fs := flag.NewFlagSet("revoke-operator", flag.ContinueOnError)
		name := fs.String("name", "", "operator name to revoke (required)")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("revoke-operator: -name is required")
		}
		if err := store.RevokeOperator(ctx, *name); err != nil {
			return err
		}
		logAdminAction(ctx, deps, *actor, identity.DefaultScope, "revoke-operator", map[string]any{"operator_name": *name})
		fmt.Printf("revoked operator %s\n", *name)

	default:
		return fmt.Errorf("unknown subcommand %q", subcommand)
	}
	return nil
}

// logAdminAction is best-effort: a failure to write the audit trail
// shouldn't make hupi-admin report a provisioning action as failed when
// it actually succeeded, so this logs to stderr rather than propagating.
func logAdminAction(ctx context.Context, deps *bootstrap.Deps, actor string, scope identity.Scope, action string, detail map[string]any) {
	detail["action"] = action
	err := audit.LogStandalone(ctx, deps.DB, audit.Entry{
		EventType:      audit.EventAdminProvision,
		Actor:          actor,
		ActingScope:    scope,
		WorkspaceScope: scope,
		Detail:         detail,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hupi-admin: warning: audit log write failed: %v\n", err)
	}
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
