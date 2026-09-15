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

// See internal/store/scope_isolation_test.go's doc comment for how to run
// these against a real Postgres instance; same HUPI_TEST_DATABASE_URL.

func testAuthStore(t *testing.T) *Store {
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
	return New(db, keys)
}

func TestResolve_ValidKeyIncludesTeamMemberships(t *testing.T) {
	s := testAuthStore(t)
	ctx := context.Background()

	userID := "user:test-auth-a"
	teamID := "team:test-auth-a"
	t.Cleanup(func() { cleanupAuth(t, s, userID, teamID) })

	if err := s.CreateUser(ctx, userID, "a@example.com"); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.CreateTeam(ctx, teamID, "Test Team A"); err != nil {
		t.Fatalf("create team: %v", err)
	}
	if err := s.AddTeamMember(ctx, teamID, userID, "member"); err != nil {
		t.Fatalf("add team member: %v", err)
	}
	rawKey, err := s.CreateAPIKey(ctx, userID)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}

	id, err := s.Resolve(ctx, rawKey)
	if err != nil {
		t.Fatalf("resolve valid key: %v", err)
	}
	if id.UserID != userID {
		t.Errorf("got UserID %q, want %q", id.UserID, userID)
	}
	if !id.HasTeam(teamID) {
		t.Errorf("expected identity to include team membership %q, got %+v", teamID, id.TeamIDs)
	}
}

func TestResolve_UnknownKeyFails(t *testing.T) {
	s := testAuthStore(t)
	ctx := context.Background()

	_, err := s.Resolve(ctx, "hupi_sk_this_key_was_never_created")
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("got error %v, want ErrInvalidKey", err)
	}
}

func TestResolve_RevokedKeyFails(t *testing.T) {
	s := testAuthStore(t)
	ctx := context.Background()

	userID := "user:test-auth-b"
	t.Cleanup(func() { cleanupAuth(t, s, userID, "") })

	if err := s.CreateUser(ctx, userID, ""); err != nil {
		t.Fatalf("create user: %v", err)
	}
	rawKey, err := s.CreateAPIKey(ctx, userID)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `update api_keys set revoked_at = now() where key_hash = $1`, hashKey(rawKey)); err != nil {
		t.Fatalf("revoke key: %v", err)
	}

	_, err = s.Resolve(ctx, rawKey)
	if !errors.Is(err, ErrInvalidKey) {
		t.Errorf("got error %v, want ErrInvalidKey for a revoked key", err)
	}
}

func cleanupAuth(t *testing.T, s *Store, userID, teamID string) {
	t.Helper()
	if userID != "" {
		_, _ = s.db.Exec(`delete from api_keys where user_id = $1`, userID)
		_, _ = s.db.Exec(`delete from team_members where user_id = $1`, userID)
		_, _ = s.db.Exec(`delete from users where id = $1`, userID)
		_, _ = s.db.Exec(`delete from scope_keys where scope_kind = 'private' and scope_owner = $1`, userID)
	}
	if teamID != "" {
		_, _ = s.db.Exec(`delete from team_members where team_id = $1`, teamID)
		_, _ = s.db.Exec(`delete from teams where id = $1`, teamID)
		_, _ = s.db.Exec(`delete from scope_keys where scope_kind = 'shared' and scope_owner = $1`, teamID)
	}
}
