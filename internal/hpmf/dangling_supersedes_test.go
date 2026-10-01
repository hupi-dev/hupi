package hpmf

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// TestImportScope_DanglingSupersedesImportedAsUnlinkedAndCounted is the
// real regression test for review finding B20: a summary version's
// `supersedes` could point at a predecessor not present in the bundle —
// e.g. a partial/filtered export — and importSummaries silently resolved
// that to "" (no supersedes at all), so the imported row ended up
// indistinguishable from an original, non-corrected summary, with no
// warning and nothing in ImportStats reflecting the lost lineage.
//
// Reproduces the real scenario: export a scope with a genuine v1->v2
// correction, then edit the exported bundle on disk to drop v1 — exactly
// what a partial/filtered export would produce — before importing.
func TestImportScope_DanglingSupersedesImportedAsUnlinkedAndCounted(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-dangling-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-dangling-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "dangling")

	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	// Simulate a partial/filtered export: drop the v1 record the real
	// bundle's v2 still refers to via `supersedes`.
	path := filepath.Join(dir, "summaries", "daily", "2026-09-07.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read exported summaries file: %v", err)
	}
	var versions []SummaryRecord
	if err := json.Unmarshal(data, &versions); err != nil {
		t.Fatalf("parse exported summaries file: %v", err)
	}
	if len(versions) != 2 {
		t.Fatalf("expected the real v1+v2 correction pair in the export, got %d records", len(versions))
	}
	var v2Only []SummaryRecord
	for _, v := range versions {
		if v.Supersedes != "" { // keep only the version that supersedes something (v2)
			v2Only = append(v2Only, v)
		}
	}
	if len(v2Only) != 1 {
		t.Fatalf("expected exactly one corrected version, got %d", len(v2Only))
	}
	if v2Only[0].Supersedes == "" {
		t.Fatalf("test setup bug: the kept version has no supersedes reference to go dangling")
	}
	filtered, err := json.Marshal(v2Only)
	if err != nil {
		t.Fatalf("marshal filtered bundle: %v", err)
	}
	if err := os.WriteFile(path, filtered, 0o600); err != nil {
		t.Fatalf("write filtered bundle: %v", err)
	}

	stats, err := ImportScope(ctx, db, keys, dir, dst, false, "test")
	if err != nil {
		t.Fatalf("ImportScope: %v", err)
	}

	if stats.SummariesImported != 1 {
		t.Errorf("SummariesImported = %d, want 1 (the unlinked version still imports)", stats.SummariesImported)
	}
	if stats.SummariesWithDanglingSupersedes != 1 {
		t.Errorf("SummariesWithDanglingSupersedes = %d, want 1", stats.SummariesWithDanglingSupersedes)
	}

	var supersedes sql.NullString
	var count int
	err = dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select count(*) from summaries where scope_kind = $1 and scope_owner = $2`, dst.Kind, dst.Owner).Scan(&count); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			select supersedes from summaries where scope_kind = $1 and scope_owner = $2 and period = '2026-09-07' and level = 'daily'
		`, dst.Kind, dst.Owner).Scan(&supersedes)
	})
	if err != nil {
		t.Fatalf("load imported summary: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 imported summary row, got %d", count)
	}
	if supersedes.Valid {
		t.Errorf("imported row's supersedes = %q, want NULL — the predecessor was never in the bundle", supersedes.String)
	}
}
