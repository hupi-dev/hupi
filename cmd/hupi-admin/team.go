package main

import (
	"context"
	"flag"
	"fmt"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/identity"
)

// This file holds hupi-admin's Tier-3-only (team/API-key) subcommand
// bodies — isolated here specifically so it can be lifted into a
// separately-licensed package later (see the open-core split plan).
// create-user/create-team/create-operator/revoke-operator stay in
// main.go: every tier's backup/restore path needs CreateUser/CreateTeam,
// and any tier can opt into the admin UI's operator login.

func runAddMember(ctx context.Context, deps *bootstrap.Deps, teamStore *auth.TeamStore, args []string) error {
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
	if err := teamStore.AddTeamMember(ctx, *team, *user, *role); err != nil {
		return err
	}
	scope := identity.Scope{Kind: identity.ScopeKindShared, Owner: *team}
	logAdminAction(ctx, deps, *actor, scope, "add-member", map[string]any{"team_id": *team, "user_id": *user, "role": *role})
	fmt.Printf("added %s to %s as %s\n", *user, *team, *role)
	return nil
}

func runCreateKey(ctx context.Context, deps *bootstrap.Deps, teamStore *auth.TeamStore, args []string) error {
	fs := flag.NewFlagSet("create-key", flag.ContinueOnError)
	user := fs.String("user", "", "user id to issue a key for (required)")
	actor := fs.String("actor", defaultActor(), "who's running this (audit log)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return fmt.Errorf("create-key: -user is required")
	}
	rawKey, err := teamStore.CreateAPIKey(ctx, *user)
	if err != nil {
		return err
	}
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: *user}
	logAdminAction(ctx, deps, *actor, scope, "create-key", map[string]any{"user_id": *user})
	// The only time this key is ever available in plaintext — it is
	// never stored or logged anywhere else (internal/auth only ever
	// persists its sha256 hash).
	fmt.Printf("API key for %s (save this now, it cannot be shown again):\n%s\n", *user, rawKey)
	return nil
}
