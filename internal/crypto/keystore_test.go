package crypto

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/identity"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// against a real Postgres instance; same HUPI_TEST_DATABASE_URL. These
// tests are docs/HARDENING_PLAN.md §6 step 5's specific cross-checks:
// proof that per-tenant DEKs are actually distinct keys, not the same key
// reached through different lookups, and that KEK possession alone isn't
// sufficient without the right wrapped DEK.

func testDB(t *testing.T) *sql.DB {
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
	return db
}

func cleanupScopeKey(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_, _ = db.Exec(`delete from scope_keys where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
}

// TestKeyStore_ScopesGetDistinctKeys is the core per-tenant DEK guarantee:
// two different scopes' encryptors must be actual different keys, not the
// same key reachable two ways — ciphertext from one must not decrypt
// under the other.
func TestKeyStore_ScopesGetDistinctKeys(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	kek := make([]byte, 32) // fixed test KEK, shared deliberately — D5: one KEK, many DEKs
	keys := NewKeyStore(db, kek)

	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dek-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:test-dek-a"}
	t.Cleanup(func() { cleanupScopeKey(t, db, scopeA); cleanupScopeKey(t, db, scopeB) })

	encA, _, err := keys.GetOrCreate(ctx, scopeA)
	if err != nil {
		t.Fatalf("GetOrCreate scopeA: %v", err)
	}
	encB, _, err := keys.GetOrCreate(ctx, scopeB)
	if err != nil {
		t.Fatalf("GetOrCreate scopeB: %v", err)
	}

	ciphertext, err := encA.Encrypt("scope A's secret")
	if err != nil {
		t.Fatalf("encrypt under scopeA's key: %v", err)
	}

	if _, err := encB.Decrypt(ciphertext); err == nil {
		t.Fatal("DEK ISOLATION FAILED: scope B's encryptor successfully decrypted scope A's ciphertext — they're using the same key")
	}

	// Sanity: scopeA's own key must still decrypt its own ciphertext —
	// proves the failure above is real isolation, not a broken Encryptor.
	plaintext, err := encA.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("scopeA failed to decrypt its own ciphertext: %v", err)
	}
	if plaintext != "scope A's secret" {
		t.Errorf("decrypted %q, want the original plaintext", plaintext)
	}
}

// TestKeyStore_SameScopeIsConsistent confirms repeated resolution of the
// same scope — including via a second, independent KeyStore instance
// hitting the same database (simulating two processes) — always yields a
// key that can decrypt what an earlier resolution encrypted.
func TestKeyStore_SameScopeIsConsistent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	kek := make([]byte, 32)
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dek-b"}
	t.Cleanup(func() { cleanupScopeKey(t, db, scope) })

	keys1 := NewKeyStore(db, kek)
	enc1, _, err := keys1.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("GetOrCreate (first KeyStore): %v", err)
	}
	ciphertext, err := enc1.Encrypt("consistent across instances")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// A second KeyStore instance, same KEK, same DB — must resolve to a
	// key that decrypts what the first one encrypted (reads the same
	// persisted wrapped DEK rather than generating a new one).
	keys2 := NewKeyStore(db, kek)
	enc2, _, err := keys2.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("GetOrCreate (second KeyStore): %v", err)
	}
	plaintext, err := enc2.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("second KeyStore instance could not decrypt the first's ciphertext: %v", err)
	}
	if plaintext != "consistent across instances" {
		t.Errorf("decrypted %q, want original plaintext", plaintext)
	}
}

// TestKeyStore_WrongKEKCannotUnwrap confirms KEK possession alone isn't
// sufficient: a KeyStore built with the wrong KEK must fail to unwrap an
// existing scope's DEK, not silently produce a usable-but-wrong key.
func TestKeyStore_WrongKEKCannotUnwrap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dek-c"}
	t.Cleanup(func() { cleanupScopeKey(t, db, scope) })

	correctKEK := make([]byte, 32)
	correctKEK[0] = 1
	keys := NewKeyStore(db, correctKEK)
	if _, _, err := keys.GetOrCreate(ctx, scope); err != nil {
		t.Fatalf("GetOrCreate with correct KEK: %v", err)
	}

	wrongKEK := make([]byte, 32)
	wrongKEK[0] = 2
	wrongKeys := NewKeyStore(db, wrongKEK)
	if _, _, err := wrongKeys.GetOrCreate(ctx, scope); err == nil {
		t.Fatal("KEK ISOLATION FAILED: a KeyStore with the wrong KEK successfully unwrapped an existing scope's DEK")
	}
}
