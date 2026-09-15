// Command hupi-admin provisions the identity Tier 3 needs — users, teams,
// memberships, API keys, and (docs/GAP_CLOSURE_PLAN.md §4.3) admin-UI
// operator credentials — see docs/TIER3_PLAN.md's non-goals ("no
// team-management UI... direct DB writes or a thin CLI"). This is that
// thin CLI: every subcommand is a direct call into internal/auth.Store,
// nothing more, plus an audit_log entry per docs/GAP_CLOSURE_PLAN.md §4.3.
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
		fs := flag.NewFlagSet("add-member", flag.ContinueOnError)
		team := fs.String("team", "", "team id (required)")
		user := fs.String("user", "", "user id (required)")
		role := fs.String("role", "member", "member | admin")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *team == "" || *user == "" {
			return fmt.Errorf("add-member: -team and -user are required")
		}
		if err := store.AddTeamMember(ctx, *team, *user, *role); err != nil {
			return err
		}
		scope := identity.Scope{Kind: identity.ScopeKindShared, Owner: *team}
		logAdminAction(ctx, deps, *actor, scope, "add-member", map[string]any{"team_id": *team, "user_id": *user, "role": *role})
		fmt.Printf("added %s to %s as %s\n", *user, *team, *role)

	case "create-key":
		fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
		user := fs.String("user", "", "user id to issue a key for (required)")
		actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *user == "" {
			return fmt.Errorf("create-key: -user is required")
		}
		rawKey, err := store.CreateAPIKey(ctx, *user)
		if err != nil {
			return err
		}
		scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: *user}
		logAdminAction(ctx, deps, *actor, scope, "create-key", map[string]any{"user_id": *user})
		// The only time this key is ever available in plaintext — it is
		// never stored or logged anywhere else (internal/auth only ever
		// persists its sha256 hash).
		fmt.Printf("API key for %s (save this now, it cannot be shown again):\n%s\n", *user, rawKey)

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
