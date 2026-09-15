package rotate

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

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

func cleanup(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
	db.Exec(`delete from key_rotations where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
	db.Exec(`delete from scope_keys where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
	db.Exec(`delete from audit_log where workspace_scope_kind = $1 and workspace_scope_owner = $2`, scope.Kind, scope.Owner)
}

// seed writes n episodes, n summaries (each with one key fact), and n
// entities into scope, all under whatever version keys.GetOrCreate
// currently resolves to — enough rows, with a small batch size, to force
// Continue to take several calls per table.
func seed(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, n int, tag string) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	ts := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	for i := 0; i < n; i++ {
		suffix := tag + "-" + string(rune('a'+i))
		inputCT, _ := enc.Encrypt("input " + suffix)
		outputCT, _ := enc.Encrypt("output " + suffix)
		noteCT, _ := enc.Encrypt("note " + suffix)
		err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				insert into episodes (id, ts, type, input_text, output_text, note, hash, importance, scope_kind, scope_owner)
				values ($1, $2, 'feedback', $3, $4, $5, $6, 0.5, $7, $8)
			`, "ep_"+suffix, ts, inputCT, outputCT, noteCT, "sha256:"+suffix, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			t.Fatalf("seed episode %s: %v", suffix, err)
		}

		summaryCT, _ := enc.Encrypt("summary " + suffix)
		factCT, _ := enc.Encrypt("fact " + suffix)
		err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `
				insert into summaries (id, period, level, summary, scope_kind, scope_owner)
				values ($1, $2, 'daily', $3, $4, $5)
			`, "sum_"+suffix, "2026-09-0"+string(rune('1'+i)), summaryCT, scope.Kind, scope.Owner); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				insert into summary_key_facts (summary_id, fact, grounded) values ($1, $2, true)
			`, "sum_"+suffix, factCT)
			return err
		})
		if err != nil {
			t.Fatalf("seed summary %s: %v", suffix, err)
		}

		attrsCT, _ := enc.Encrypt(`{"tag":"` + suffix + `"}`)
		err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, attributes, scope_kind, scope_owner)
				values ($1, 'project', $2, $3, $4, $5)
			`, "project:"+suffix, "Project "+suffix, attrsCT, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			t.Fatalf("seed entity %s: %v", suffix, err)
		}
	}
}

func runToCompletion(t *testing.T, ctx context.Context, r *Runner, scope identity.Scope, batchSize int) int {
	t.Helper()
	total := 0
	for i := 0; i < 1000; i++ { // hard cap so a bug can't hang the test suite
		processed, done, err := r.Continue(ctx, scope, batchSize, "test")
		if err != nil {
			t.Fatalf("Continue: %v", err)
		}
		total += processed
		if done {
			return total
		}
	}
	t.Fatal("rotation did not complete within 1000 Continue calls — likely stuck")
	return total
}

func TestRotate_FullLifecycle(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-full"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 3, "full")

	r := New(db, keys)
	fromVersion, toVersion, err := r.Start(ctx, scope, "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if fromVersion != 1 || toVersion != 2 {
		t.Fatalf("got from=%d to=%d, want 1->2", fromVersion, toVersion)
	}

	total := runToCompletion(t, ctx, r, scope, 1) // batch size 1 forces many small batches
	if total != 9 {                               // 3 episodes + 3 summaries + 3 entities
		t.Errorf("migrated %d rows, want 9", total)
	}

	st, ok, err := r.Status(ctx, scope)
	if err != nil || !ok {
		t.Fatalf("Status: ok=%v err=%v", ok, err)
	}
	if st.Status != StatusCompleted {
		t.Errorf("status = %s, want completed", st.Status)
	}
	if st.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	// Every row must now report key_version = 2, and decrypt correctly
	// under v2's key — not just "some version," the *new* one specifically.
	assertAllOnVersion(t, ctx, db, scope, "episodes", 2)
	assertAllOnVersion(t, ctx, db, scope, "summaries", 2)
	assertAllOnVersion(t, ctx, db, scope, "entities", 2)

	toEnc, err := keys.GetVersion(ctx, scope, toVersion)
	if err != nil {
		t.Fatalf("GetVersion(toVersion): %v", err)
	}
	var inputCT []byte
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select input_text from episodes where id = 'ep_full-a'`).Scan(&inputCT)
	}); err != nil {
		t.Fatalf("load migrated episode: %v", err)
	}
	text, err := toEnc.Decrypt(inputCT)
	if err != nil {
		t.Fatalf("decrypt with new key: %v", err)
	}
	if text != "input full-a" {
		t.Errorf("decrypted %q, want %q — content must survive re-encryption exactly", text, "input full-a")
	}

	// The old key must still exist (kept, not deleted) and still be
	// resolvable — pruning is a separate, explicit step.
	if _, err := keys.GetVersion(ctx, scope, fromVersion); err != nil {
		t.Errorf("old version should still be resolvable until an explicit prune: %v", err)
	}
}

func assertAllOnVersion(t *testing.T, ctx context.Context, db *sql.DB, scope identity.Scope, table string, version int) {
	t.Helper()
	var count int
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			"select count(*) from "+table+" where scope_kind = $1 and scope_owner = $2 and key_version != $3",
			scope.Kind, scope.Owner, version,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count %s not on version %d: %v", table, version, err)
	}
	if count != 0 {
		t.Errorf("%s has %d row(s) not on version %d after rotation completed", table, count, version)
	}
}

// TestRotate_ResumesAfterInterruption simulates a killed process: run
// Continue exactly once (partial progress, since batch size 1 can't
// finish 9 rows in one call), then hand off to a *new* Runner (a fresh
// process would build a fresh one too) and drive it to completion —
// nothing should be double-processed or skipped.
func TestRotate_ResumesAfterInterruption(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-resume"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 3, "resume")

	r1 := New(db, keys)
	if _, _, err := r1.Start(ctx, scope, "test"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	processed, done, err := r1.Continue(ctx, scope, 1, "test")
	if err != nil {
		t.Fatalf("first Continue: %v", err)
	}
	if done || processed != 1 {
		t.Fatalf("first Continue: processed=%d done=%v, want 1 row and not done yet", processed, done)
	}

	// A brand new Runner (and a brand new KeyStore, same DB and KEK —
	// what a restarted process would actually have) resumes correctly.
	r2 := New(db, crypto.NewKeyStore(db, make([]byte, 32)))
	total := processed + runToCompletion(t, ctx, r2, scope, 1)
	if total != 9 {
		t.Errorf("total migrated across both runners = %d, want 9 (no row skipped or double-processed)", total)
	}
	assertAllOnVersion(t, ctx, db, scope, "episodes", 2)
	assertAllOnVersion(t, ctx, db, scope, "summaries", 2)
	assertAllOnVersion(t, ctx, db, scope, "entities", 2)
}

// TestRotate_ConcurrentWriteLandsOnNewVersion is the "no downtime"
// promise itself: a write that happens *during* an in-progress rotation
// (simulating the live gateway still serving traffic) must land on the
// new version immediately, and the rotation batch job must never touch
// it (it's already on to_version, so it never matches the
// still-on-from_version query).
func TestRotate_ConcurrentWriteLandsOnNewVersion(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-concurrent"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 2, "old")

	r := New(db, keys)
	_, toVersion, err := r.Start(ctx, scope, "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Simulate a live write arriving mid-rotation, before any batch has
	// run — internal/store.Capture's real call site is exactly
	// keys.GetOrCreate followed by an insert with that returned version.
	enc, version, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("GetOrCreate during rotation: %v", err)
	}
	if version != toVersion {
		t.Fatalf("a write during rotation resolved version %d, want the new version %d — it would be lost on prune", version, toVersion)
	}
	newCT, _ := enc.Encrypt("written during rotation")
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner, key_version)
			values ('ep_concurrent', now(), 'interaction', $1, $1, 'sha256:concurrent', 0.5, $2, $3, $4)
		`, newCT, scope.Kind, scope.Owner, version)
		return err
	}); err != nil {
		t.Fatalf("seed concurrent write: %v", err)
	}

	total := runToCompletion(t, ctx, r, scope, 2)
	if total != 6 { // 2 episodes + 2 summaries + 2 entities from seed — NOT the concurrent write
		t.Errorf("migrated %d rows, want exactly 6 (the concurrent write should never be touched by the batch job)", total)
	}

	var keyVersion int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select key_version from episodes where id = 'ep_concurrent'`).Scan(&keyVersion)
	}); err != nil {
		t.Fatalf("load concurrent episode: %v", err)
	}
	if keyVersion != toVersion {
		t.Errorf("concurrent episode's key_version = %d after rotation, want it unchanged at %d", keyVersion, toVersion)
	}
}

func TestRotate_PruneRefusesBeforeCompletion(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-prune-early"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 1, "prune-early")
	r := New(db, keys)
	if _, _, err := r.Start(ctx, scope, "test"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := r.Prune(ctx, scope, "test"); err == nil {
		t.Fatal("expected Prune to refuse while rotation is still in_progress")
	}
}

func TestRotate_PruneDeletesOldVersionOnceCompleted(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-prune-done"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 1, "prune-done")
	r := New(db, keys)
	fromVersion, _, err := r.Start(ctx, scope, "test")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runToCompletion(t, ctx, r, scope, 10)

	pruned, err := r.Prune(ctx, scope, "test")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruned != 1 {
		t.Errorf("pruned %d version(s), want 1", pruned)
	}

	if _, err := keys.GetVersion(ctx, scope, fromVersion); err == nil {
		t.Error("expected the pruned old version to no longer resolve")
	}
}

func TestRotate_StartIsIdempotentWhileInProgress(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rotate-idempotent"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seed(t, ctx, db, keys, scope, 1, "idempotent")
	r := New(db, keys)
	from1, to1, err := r.Start(ctx, scope, "test")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	from2, to2, err := r.Start(ctx, scope, "test")
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if from1 != from2 || to1 != to2 {
		t.Errorf("second Start returned %d->%d, want the same %d->%d as the first (no third version created)", from2, to2, from1, to1)
	}
	current, err := keys.CurrentVersion(ctx, scope)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	if current != to1 {
		t.Errorf("current version = %d after calling Start twice, want it to still be %d (not a third version)", current, to1)
	}
}
