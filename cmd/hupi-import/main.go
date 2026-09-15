// Command hupi-import loads an HPMF bundle (hupi-export's output) back
// into Postgres — one scope, or (with -all) every scope the bundle
// contains, restoring a whole-deployment export. See
// docs/GAP_CLOSURE_PLAN.md §4.2 for merge semantics.
//
// Usage:
//
//	hupi-import -in alice.age -scope-kind private -scope-owner user:alice -identity alice-key.txt
//	hupi-import -in full-backup.age -all -identity backup-key.txt -merge
//
// Decrypt with an age identity file (-identity) or a passphrase
// (-passphrase, read from $HUPI_IMPORT_PASSPHRASE). By default the
// target scope must be empty; -merge allows importing into one that
// already has data (episodes deduped by hash, entities by id, summaries
// by period — see internal/hpmf's doc comments for the exact rule).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"filippo.io/age"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/hpmf"
	"hupi/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hupi-import:", err)
		os.Exit(1)
	}
}

func run() error {
	in := flag.String("in", "", "input .age file (required)")
	scopeKind := flag.String("scope-kind", "", "private | shared (with -scope-owner, imports into exactly this scope)")
	scopeOwner := flag.String("scope-owner", "", "user id or team id to import into")
	all := flag.Bool("all", false, "restore every scope the bundle contains, into scopes of the same name (provisioning users/teams as needed)")
	merge := flag.Bool("merge", false, "allow importing into a scope that already has data (default: target must be empty)")
	identityPath := flag.String("identity", "", "path to an age identity file (age1... secret key)")
	passphrase := flag.Bool("passphrase", false, "decrypt with a passphrase from $HUPI_IMPORT_PASSPHRASE instead of -identity")
	actor := flag.String("actor", defaultActor(), "who's running this (audit log)")
	flag.Parse()

	if *in == "" {
		return fmt.Errorf("usage: hupi-import -in <file.age> [-scope-kind private|shared -scope-owner <id> | -all] [-identity <file> | -passphrase] [-merge]")
	}
	single := *scopeKind != "" || *scopeOwner != ""
	if single == *all {
		return fmt.Errorf("hupi-import: specify exactly one of -scope-kind/-scope-owner or -all")
	}
	if single && (*scopeKind == "" || *scopeOwner == "") {
		return fmt.Errorf("hupi-import: -scope-kind and -scope-owner must be given together")
	}

	identities, err := resolveIdentities(*identityPath, *passphrase)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "hupi-import-*")
	if err != nil {
		return fmt.Errorf("hupi-import: create temp directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	if err := hpmf.DecryptAndUnpack(*in, tempDir, identities); err != nil {
		return fmt.Errorf("hupi-import: %w", err)
	}
	manifest, err := hpmf.ReadManifest(tempDir)
	if err != nil {
		return fmt.Errorf("hupi-import: %w", err)
	}

	ctx := context.Background()
	deps, err := bootstrap.Load(ctx)
	if err != nil {
		return err
	}
	defer deps.DB.Close()

	if *all {
		return importAll(ctx, deps, tempDir, manifest, *merge, *actor)
	}
	return importOne(ctx, deps, tempDir, manifest, identity.Scope{Kind: *scopeKind, Owner: *scopeOwner}, *merge, *actor)
}

func importOne(ctx context.Context, deps *bootstrap.Deps, tempDir string, manifest hpmf.Manifest, target identity.Scope, merge bool, actor string) error {
	if len(manifest.Scopes) != 1 {
		return fmt.Errorf("hupi-import: bundle contains %d scopes, not 1 — use -all to restore a whole-deployment export", len(manifest.Scopes))
	}
	sm := manifest.Scopes[0]
	stats, err := hpmf.ImportScope(ctx, deps.DB, deps.Keys, filepath.Join(tempDir, sm.Dir), target, merge, actor)
	if err != nil {
		return fmt.Errorf("hupi-import: %w", err)
	}
	printStats(target, stats)
	return nil
}

// importAll restores every scope in the bundle into a scope of the same
// kind+owner, provisioning the user/team first if it doesn't already
// exist — the DR/whole-deployment-restore complement to hupi-export -all.
func importAll(ctx context.Context, deps *bootstrap.Deps, tempDir string, manifest hpmf.Manifest, merge bool, actor string) error {
	store := auth.New(deps.DB, deps.Keys)
	var failed int
	for _, sm := range manifest.Scopes {
		scope := sm.Scope()
		if err := ensureScopeExists(ctx, store, scope); err != nil {
			fmt.Fprintf(os.Stderr, "hupi-import: provision %s:%s: %v\n", scope.Kind, scope.Owner, err)
			failed++
			continue
		}
		stats, err := hpmf.ImportScope(ctx, deps.DB, deps.Keys, filepath.Join(tempDir, sm.Dir), scope, merge, actor)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hupi-import: %s:%s: %v\n", scope.Kind, scope.Owner, err)
			failed++
			continue
		}
		printStats(scope, stats)
	}
	if failed > 0 {
		return fmt.Errorf("hupi-import: failed for %d/%d scopes", failed, len(manifest.Scopes))
	}
	return nil
}

// ensureScopeExists creates the user/team row (and its DEK) if it's not
// already there — restoring a whole-deployment export into a genuinely
// fresh database needs the identity rows hupi-admin would otherwise
// provision by hand, one per scope, first.
func ensureScopeExists(ctx context.Context, store *auth.Store, scope identity.Scope) error {
	if scope.Kind == identity.ScopeKindPrivate {
		return store.CreateUser(ctx, scope.Owner, "")
	}
	return store.CreateTeam(ctx, scope.Owner, scope.Owner)
}

func printStats(scope identity.Scope, stats hpmf.ImportStats) {
	fmt.Printf("%s:%s — episodes: %d imported, %d skipped; summaries: %d imported, %d skipped; entities: %d imported, %d skipped\n",
		scope.Kind, scope.Owner,
		stats.EpisodesImported, stats.EpisodesSkipped,
		stats.SummariesImported, stats.SummariesSkipped,
		stats.EntitiesImported, stats.EntitiesSkipped,
	)
}

func resolveIdentities(identityPath string, usePassphrase bool) ([]age.Identity, error) {
	switch {
	case identityPath != "" && usePassphrase:
		return nil, fmt.Errorf("hupi-import: specify -identity or -passphrase, not both")
	case identityPath != "":
		f, err := os.Open(identityPath)
		if err != nil {
			return nil, fmt.Errorf("hupi-import: open -identity file: %w", err)
		}
		defer f.Close()
		ids, err := age.ParseIdentities(f)
		if err != nil {
			return nil, fmt.Errorf("hupi-import: parse -identity file: %w", err)
		}
		return ids, nil
	case usePassphrase:
		pass := os.Getenv("HUPI_IMPORT_PASSPHRASE")
		if pass == "" {
			return nil, fmt.Errorf("hupi-import: -passphrase given but $HUPI_IMPORT_PASSPHRASE is not set")
		}
		id, err := age.NewScryptIdentity(pass)
		if err != nil {
			return nil, fmt.Errorf("hupi-import: build passphrase identity: %w", err)
		}
		return []age.Identity{id}, nil
	default:
		return nil, fmt.Errorf("hupi-import: one of -identity or -passphrase is required")
	}
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
