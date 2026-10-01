package hpmf

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hupi/internal/identity"
)

// TestImportScope_NonMergeFailureHintsAtMerge is the real regression test
// for review finding C6: a non-merge import failing partway through
// (every phase, and within importSummaries every period file, commits in
// its own transaction) could leave real, already-committed data in the
// target scope with no indication that a retry would need -merge — a
// user would only discover that one round trip later, after a bare retry
// hit the "already has data" check from scratch.
func TestImportScope_NonMergeFailureHintsAtMerge(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-partial-failure"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	dir := corruptSummariesBundle(t)

	_, err := ImportScope(ctx, db, keys, dir, scope, false, "test")
	if err == nil {
		t.Fatal("expected ImportScope to fail on the corrupt summaries file")
	}
	if !strings.Contains(err.Error(), "retry with -merge") {
		t.Errorf("err = %q, want it to hint at -merge for a retry", err.Error())
	}
}

// TestImportScope_MergeModeFailureHasNoMergeHint confirms the fix is
// scoped to non-merge imports only: a merge import's own failure doesn't
// need the same hint, since a retry of a merge import already uses the
// same flag.
func TestImportScope_MergeModeFailureHasNoMergeHint(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-partial-failure-merge"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	dir := corruptSummariesBundle(t)

	_, err := ImportScope(ctx, db, keys, dir, scope, true, "test")
	if err == nil {
		t.Fatal("expected ImportScope to fail on the corrupt summaries file")
	}
	if strings.Contains(err.Error(), "retry with -merge") {
		t.Errorf("err = %q, a merge-mode import's own failure shouldn't suggest -merge again", err.Error())
	}
}

// corruptSummariesBundle builds a minimal bundle directory with a
// deliberately invalid summaries/daily/<period>.json — enough to make
// importSummaries fail deterministically at the json.Unmarshal step,
// without needing a real database-level error.
func corruptSummariesBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "summaries", "daily"), 0o755); err != nil {
		t.Fatalf("mkdir summaries dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "summaries", "daily", "2026-01-01.json"), []byte("not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt summaries file: %v", err)
	}
	return dir
}
