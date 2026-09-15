// Command hupi-export writes a portable, age-encrypted snapshot of one
// scope's memory, or (with -all) every scope in the deployment — the
// HPMF format's write side, docs/GAP_CLOSURE_PLAN.md §4.2 and
// MEMORY_FORMAT.md § Storage & encryption model. Like hupi-admin/
// hupi-trace, this is an operator CLI with full database access, not a
// request-scoped tool.
//
// Usage:
//
//	hupi-export -scope-kind private -scope-owner user:alice -recipient age1... -out alice.age
//	hupi-export -all -recipient age1... -out full-backup.age
//
// Encrypt to a recipient's age public key (-recipient), or a passphrase
// (-passphrase, read from HUPI_EXPORT_PASSPHRASE — never as a flag, so it
// never lands in shell history or a process list). Exactly one is
// required.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"

	"filippo.io/age"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/crypto"
	"hupi/internal/hpmf"
	"hupi/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-export:", err)
		os.Exit(1)
	}
}

func run() error {
	scopeKind := flag.String("scope-kind", "", "private | shared (with -scope-owner, exports exactly this scope)")
	scopeOwner := flag.String("scope-owner", "", "user id or team id to export")
	all := flag.Bool("all", false, "export every scope in the deployment (whole-deployment backup)")
	out := flag.String("out", "", "output .age file path (required)")
	recipient := flag.String("recipient", "", "age public key (age1...) to encrypt to")
	passphrase := flag.Bool("passphrase", false, "encrypt with a passphrase from $HUPI_EXPORT_PASSPHRASE instead of -recipient")
	actor := flag.String("actor", defaultActor(), "who's running this (audit log)")
	flag.Parse()

	if *out == "" {
		return fmt.Errorf("usage: hupi-export [-scope-kind private|shared -scope-owner <id> | -all] -out <file.age> [-recipient age1... | -passphrase]")
	}
	single := *scopeKind != "" || *scopeOwner != ""
	if single == *all {
		return fmt.Errorf("hupi-export: specify exactly one of -scope-kind/-scope-owner or -all")
	}
	if single && (*scopeKind == "" || *scopeOwner == "") {
		return fmt.Errorf("hupi-export: -scope-kind and -scope-owner must be given together")
	}

	recipients, err := resolveRecipients(*recipient, *passphrase)
	if err != nil {
		return err
	}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	var scopes []identity.Scope
	if *all {
		scopes, err = loadActiveScopes(ctx, deps.DB, deps.Keys)
	} else {
		scopes = []identity.Scope{{Kind: *scopeKind, Owner: *scopeOwner}}
	}
	if err != nil {
		return fmt.Errorf("hupi-export: %w", err)
	}

	tempDir, manifest, err := hpmf.ExportBundle(ctx, deps.DB, deps.Keys, scopes, *actor)
	if err != nil {
		return fmt.Errorf("hupi-export: %w", err)
	}
	defer os.RemoveAll(tempDir)

	if err := hpmf.PackAndEncrypt(tempDir, *out, recipients); err != nil {
		return fmt.Errorf("hupi-export: %w", err)
	}

	fmt.Printf("wrote %s: %d scope(s)\n", *out, len(manifest.Scopes))
	for _, sm := range manifest.Scopes {
		fmt.Printf("  %s:%s — %d episodes, %d summaries, %d entities\n",
			sm.ScopeKind, sm.ScopeOwner, sm.EpisodeCount, sm.SummaryCount, sm.EntityCount)
	}
	return nil
}

func resolveRecipients(recipientStr string, usePassphrase bool) ([]age.Recipient, error) {
	switch {
	case recipientStr != "" && usePassphrase:
		return nil, fmt.Errorf("hupi-export: specify -recipient or -passphrase, not both")
	case recipientStr != "":
		r, err := age.ParseX25519Recipient(recipientStr)
		if err != nil {
			return nil, fmt.Errorf("hupi-export: -recipient: %w", err)
		}
		return []age.Recipient{r}, nil
	case usePassphrase:
		pass := os.Getenv("HUPI_EXPORT_PASSPHRASE")
		if pass == "" {
			return nil, fmt.Errorf("hupi-export: -passphrase given but $HUPI_EXPORT_PASSPHRASE is not set")
		}
		r, err := age.NewScryptRecipient(pass)
		if err != nil {
			return nil, fmt.Errorf("hupi-export: build passphrase recipient: %w", err)
		}
		return []age.Recipient{r}, nil
	default:
		return nil, fmt.Errorf("hupi-export: one of -recipient or -passphrase is required — an export is never written unencrypted")
	}
}

// loadActiveScopes enumerates every user's private scope and every team's
// shared scope via internal/auth.Store — the same identity tables
// cmd/hupi-consolidate's own loadActiveScopes reads directly, but through
// auth.Store's existing List methods here instead of a third copy of the
// raw query.
func loadActiveScopes(ctx context.Context, db *sql.DB, keys *crypto.KeyStore) ([]identity.Scope, error) {
	store := auth.New(db, keys)

	users, err := store.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	teams, err := store.ListTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams: %w", err)
	}

	scopes := make([]identity.Scope, 0, len(users)+len(teams))
	for _, u := range users {
		scopes = append(scopes, identity.Scope{Kind: identity.ScopeKindPrivate, Owner: u.ID})
	}
	for _, t := range teams {
		scopes = append(scopes, identity.Scope{Kind: identity.ScopeKindShared, Owner: t.ID})
	}
	return scopes, nil
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
