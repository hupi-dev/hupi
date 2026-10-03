package auth

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
)

// testStore opens a connection to a real Postgres instance — skips (not
// fails) if HUPI_TEST_DATABASE_URL isn't set, same convention
// internal/store/scope_isolation_test.go's testStore uses.
func testStore(t *testing.T) (*Store, *sql.DB) {
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
	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
	return New(db, keys), db
}

func TestSetPassword_VerifyPassword_RoundTrip(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()
	userID := "user:test-auth-password"
	t.Cleanup(func() {
		_, _ = db.Exec(`delete from users where id = $1`, userID)
	})

	if err := store.CreateUser(ctx, userID, ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.SetPassword(ctx, userID, "correct horse battery staple"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if err := store.VerifyPassword(ctx, userID, "correct horse battery staple"); err != nil {
		t.Errorf("VerifyPassword with the right password: %v", err)
	}
	if err := store.VerifyPassword(ctx, userID, "wrong password"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("VerifyPassword with the wrong password = %v, want ErrInvalidPassword", err)
	}
}

func TestVerifyPassword_UserWithNoPasswordSetIsInvalid(t *testing.T) {
	store, db := testStore(t)
	ctx := context.Background()
	userID := "user:test-auth-no-password"
	t.Cleanup(func() {
		_, _ = db.Exec(`delete from users where id = $1`, userID)
	})

	if err := store.CreateUser(ctx, userID, ""); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	if err := store.VerifyPassword(ctx, userID, "anything"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("VerifyPassword for a user with no password set = %v, want ErrInvalidPassword", err)
	}
}

func TestVerifyPassword_NonexistentUserIsInvalid(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	if err := store.VerifyPassword(ctx, "user:does-not-exist-at-all", "anything"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("VerifyPassword for a nonexistent user = %v, want ErrInvalidPassword", err)
	}
}

func TestSetPassword_NonexistentUserErrors(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	if err := store.SetPassword(ctx, "user:does-not-exist-at-all", "irrelevant"); err == nil {
		t.Error("SetPassword for a nonexistent user: expected an error, got nil")
	}
}
