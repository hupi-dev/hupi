package hpmf

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// these against a real Postgres instance.

func testDB(t *testing.T) (*sql.DB, *crypto.KeyStore) {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
}

func cleanupScope(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
	db.Exec(`delete from audit_log where workspace_scope_kind = $1 and workspace_scope_owner = $2`, scope.Kind, scope.Owner)
}

// seedScope writes one episode, one two-version summary (with a key
// fact), and one entity into scope — enough to exercise every record
// kind ExportScope/ImportScope handle, including the supersedes chain.
func seedScope(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, tag string) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key for %s: %v", tag, err)
	}
	ts := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	inputCT, _ := enc.Encrypt("input " + tag)
	outputCT, _ := enc.Encrypt("output " + tag)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ($1, $2, 'interaction', $3, $4, $5, 0.6, $6, $7)
		`, "ep_"+tag, ts, inputCT, outputCT, "sha256:"+tag, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode for %s: %v", tag, err)
	}

	v1CT, _ := enc.Encrypt("summary v1 " + tag)
	v2CT, _ := enc.Encrypt("summary v2 " + tag)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, source_episode_ids, scope_kind, scope_owner)
			values ($1, '2026-09-07', 'daily', $2, $3, $4, $5)
		`, "sum_"+tag+"_v1", v1CT, "{ep_"+tag+"}", scope.Kind, scope.Owner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded)
			values ($1, $2, $3, true)
		`, "sum_"+tag+"_v1", v1CT, "{ep_"+tag+"}"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, source_episode_ids, supersedes, correction_reason, scope_kind, scope_owner)
			values ($1, '2026-09-07', 'daily', $2, $3, $4, 'fixed a mistake', $5, $6)
		`, "sum_"+tag+"_v2", v2CT, "{ep_"+tag+"}", "sum_"+tag+"_v1", scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed summary for %s: %v", tag, err)
	}

	attrsCT, _ := enc.Encrypt(`{"tag":"` + tag + `"}`)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner)
			values ($1, 'project', $2, $3, $4, $5)
		`, "project:"+tag, "Project "+tag, attrsCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed entity for %s: %v", tag, err)
	}
}

func decryptedSummaryText(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string) string {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}
	var ct []byte
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select summary from summaries where id = $1`, id).Scan(&ct)
	})
	if err != nil {
		t.Fatalf("load summary %s: %v", id, err)
	}
	text, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt summary %s: %v", id, err)
	}
	return text
}

func TestExportImportRoundTrip_SameScope(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()

	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-src"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-dst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "roundtrip")

	dir := t.TempDir()
	sm, err := ExportScope(ctx, db, keys, src, dir, "test")
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	if sm.EpisodeCount != 1 || sm.SummaryCount != 2 || sm.EntityCount != 1 {
		t.Fatalf("manifest = %+v, want 1 episode, 2 summaries (v1+v2), 1 entity", sm)
	}

	// Regression check: attributes must serialize as a nested JSON object
	// in entities.jsonl, not a string holding escaped JSON — the whole
	// point of a "human-readable, diffable" export (MEMORY_FORMAT.md's
	// own design principle #2).
	entitiesRaw, err := os.ReadFile(filepath.Join(dir, "entities.jsonl"))
	if err != nil {
		t.Fatalf("read entities.jsonl: %v", err)
	}
	if want := `"attributes":{"tag":"roundtrip"}`; !strings.Contains(string(entitiesRaw), want) {
		t.Errorf("entities.jsonl = %s, want it to contain %s (attributes as a nested object)", entitiesRaw, want)
	}

	stats, err := ImportScope(ctx, db, keys, dir, dst, false, "test")
	if err != nil {
		t.Fatalf("ImportScope: %v", err)
	}
	if stats.EpisodesImported != 1 || stats.SummariesImported != 2 || stats.EntitiesImported != 1 {
		t.Fatalf("stats = %+v, want 1/2/1 imported", stats)
	}

	// Content round-trips, decrypted, through the target scope's own key —
	// not just "a row exists."
	var episodeCount int
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from episodes where scope_kind=$1 and scope_owner=$2`, dst.Kind, dst.Owner).Scan(&episodeCount)
	}); err != nil {
		t.Fatalf("count episodes: %v", err)
	}
	if episodeCount != 1 {
		t.Errorf("dst has %d episodes, want 1", episodeCount)
	}

	var dstV1ID, dstV2ID, supersedes string
	if err := dbscope.Run(ctx, db, dst, dst, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where scope_kind=$1 and scope_owner=$2 and supersedes is null`, dst.Kind, dst.Owner).Scan(&dstV1ID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id, supersedes from summaries where scope_kind=$1 and scope_owner=$2 and supersedes is not null`, dst.Kind, dst.Owner).Scan(&dstV2ID, &supersedes)
	}); err != nil {
		t.Fatalf("load imported summary ids: %v", err)
	}
	if supersedes != dstV1ID {
		t.Errorf("imported v2's supersedes = %q, want it remapped to the imported v1's own new id %q (not the source scope's id)", supersedes, dstV1ID)
	}
	if got := decryptedSummaryText(t, ctx, db, keys, dst, dstV2ID); got != "summary v2 roundtrip" {
		t.Errorf("decrypted summary text = %q, want %q", got, "summary v2 roundtrip")
	}
}

func TestImportScope_FreshRejectsNonEmpty(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-nonempty"}
	t.Cleanup(func() { cleanupScope(t, db, scope) })

	seedScope(t, ctx, db, keys, scope, "existing")
	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, scope, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	if _, err := ImportScope(ctx, db, keys, dir, scope, false, "test"); err == nil {
		t.Fatal("expected fresh (non-merge) import into a non-empty scope to fail")
	}
}

func TestImportScope_MergeDedupsOnSecondImport(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	src := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-mergesrc"}
	dst := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-mergedst"}
	t.Cleanup(func() { cleanupScope(t, db, src); cleanupScope(t, db, dst) })

	seedScope(t, ctx, db, keys, src, "merge")
	dir := t.TempDir()
	if _, err := ExportScope(ctx, db, keys, src, dir, "test"); err != nil {
		t.Fatalf("ExportScope: %v", err)
	}

	first, err := ImportScope(ctx, db, keys, dir, dst, true, "test")
	if err != nil {
		t.Fatalf("first ImportScope: %v", err)
	}
	if first.EpisodesImported != 1 || first.SummariesImported != 2 || first.EntitiesImported != 1 {
		t.Fatalf("first import stats = %+v, want everything imported", first)
	}

	second, err := ImportScope(ctx, db, keys, dir, dst, true, "test")
	if err != nil {
		t.Fatalf("second ImportScope: %v", err)
	}
	if second.EpisodesImported != 0 || second.EpisodesSkipped != 1 {
		t.Errorf("second import episodes = %+v, want everything skipped", second)
	}
	if second.EntitiesImported != 0 || second.EntitiesSkipped != 1 {
		t.Errorf("second import entities = %+v, want everything skipped", second)
	}
	if second.SummariesImported != 0 || second.SummariesSkipped != 2 {
		t.Errorf("second import summaries = %+v, want everything skipped", second)
	}
}

func TestExportBundle_WholeDeployment_ScopeIsolation(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-bundle-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-hpmf-bundle-b"}
	t.Cleanup(func() { cleanupScope(t, db, scopeA); cleanupScope(t, db, scopeB) })

	seedScope(t, ctx, db, keys, scopeA, "bundlea")
	seedScope(t, ctx, db, keys, scopeB, "bundleb")

	tempDir, manifest, err := ExportBundle(ctx, db, keys, []identity.Scope{scopeA, scopeB}, "test")
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(tempDir) })

	if len(manifest.Scopes) != 2 {
		t.Fatalf("manifest has %d scopes, want 2", len(manifest.Scopes))
	}
	for _, sm := range manifest.Scopes {
		if sm.Dir == "." {
			t.Errorf("whole-deployment bundle scope %s:%s has flat Dir %q, want nested scopes/... path", sm.ScopeKind, sm.ScopeOwner, sm.Dir)
		}
	}

	// Import each into a fresh target and confirm B's data never lands in
	// A's target scope or vice versa.
	targetA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-hpmf-bundle-a-restored"}
	targetB := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-hpmf-bundle-b-restored"}
	t.Cleanup(func() { cleanupScope(t, db, targetA); cleanupScope(t, db, targetB) })

	for _, sm := range manifest.Scopes {
		target := targetA
		if sm.ScopeKind == scopeB.Kind && sm.ScopeOwner == scopeB.Owner {
			target = targetB
		}
		if _, err := ImportScope(ctx, db, keys, filepath.Join(tempDir, sm.Dir), target, false, "test"); err != nil {
			t.Fatalf("ImportScope %s into %s:%s: %v", sm.Dir, target.Kind, target.Owner, err)
		}
	}

	if got := decryptedEntityName(t, ctx, db, keys, targetA, "project:bundlea"); got != "Project bundlea" {
		t.Errorf("targetA entity name = %q, want %q", got, "Project bundlea")
	}
	if got := decryptedEntityName(t, ctx, db, keys, targetB, "project:bundleb"); got != "Project bundleb" {
		t.Errorf("targetB entity name = %q, want %q", got, "Project bundleb")
	}
	if hasEntity(t, ctx, db, targetA, "project:bundleb") {
		t.Error("scope B's entity leaked into scope A's restored target")
	}
	if hasEntity(t, ctx, db, targetB, "project:bundlea") {
		t.Error("scope A's entity leaked into scope B's restored target")
	}
}

func decryptedEntityName(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string) string {
	t.Helper()
	var name string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select name from entities where scope_kind=$1 and scope_owner=$2 and id=$3`, scope.Kind, scope.Owner, id).Scan(&name)
	}); err != nil {
		t.Fatalf("load entity %s: %v", id, err)
	}
	return name
}

func hasEntity(t *testing.T, ctx context.Context, db *sql.DB, scope identity.Scope, id string) bool {
	t.Helper()
	var count int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from entities where scope_kind=$1 and scope_owner=$2 and id=$3`, scope.Kind, scope.Owner, id).Scan(&count)
	}); err != nil {
		t.Fatalf("check entity %s: %v", id, err)
	}
	return count > 0
}

func TestPackAndEncrypt_RoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "episodes", "2026", "09"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "hello from a test fixture\n"
	if err := os.WriteFile(filepath.Join(srcDir, "episodes", "2026", "09", "2026-09-07.jsonl"), []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	ageIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "bundle.age")
	if err := PackAndEncrypt(srcDir, outPath, []age.Recipient{ageIdentity.Recipient()}); err != nil {
		t.Fatalf("PackAndEncrypt: %v", err)
	}

	destDir := t.TempDir()
	if err := DecryptAndUnpack(outPath, destDir, []age.Identity{ageIdentity}); err != nil {
		t.Fatalf("DecryptAndUnpack: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(destDir, "episodes", "2026", "09", "2026-09-07.jsonl"))
	if err != nil {
		t.Fatalf("read round-tripped file: %v", err)
	}
	if string(got) != content {
		t.Errorf("round-tripped content = %q, want %q", got, content)
	}
}

func TestDecryptAndUnpack_WrongIdentityFails(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "manifest.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	correctIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	wrongIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "bundle.age")
	if err := PackAndEncrypt(srcDir, outPath, []age.Recipient{correctIdentity.Recipient()}); err != nil {
		t.Fatalf("PackAndEncrypt: %v", err)
	}
	if err := DecryptAndUnpack(outPath, t.TempDir(), []age.Identity{wrongIdentity}); err == nil {
		t.Error("expected decryption with the wrong identity to fail")
	}
}
