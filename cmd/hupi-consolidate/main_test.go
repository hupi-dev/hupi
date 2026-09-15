package main

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/identity"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// against a real instance.
func TestLoadActiveScopes(t *testing.T) {
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	userID, teamID := "user:test-consolidate-scopes", "team:test-consolidate-scopes"
	t.Cleanup(func() {
		_, _ = db.Exec(`delete from teams where id = $1`, teamID)
		_, _ = db.Exec(`delete from users where id = $1`, userID)
	})

	if _, err := db.ExecContext(ctx, `insert into users (id) values ($1) on conflict (id) do nothing`, userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(ctx, `insert into teams (id, name) values ($1, 'Test Team') on conflict (id) do nothing`, teamID); err != nil {
		t.Fatalf("seed team: %v", err)
	}

	scopes, err := loadActiveScopes(ctx, db)
	if err != nil {
		t.Fatalf("loadActiveScopes: %v", err)
	}

	wantUser := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: userID}
	wantTeam := identity.Scope{Kind: identity.ScopeKindShared, Owner: teamID}
	var foundUser, foundTeam bool
	for _, s := range scopes {
		if s == wantUser {
			foundUser = true
		}
		if s == wantTeam {
			foundTeam = true
		}
	}
	if !foundUser {
		t.Errorf("expected loadActiveScopes to include the seeded user's private scope %+v, got %+v", wantUser, scopes)
	}
	if !foundTeam {
		t.Errorf("expected loadActiveScopes to include the seeded team's shared scope %+v, got %+v", wantTeam, scopes)
	}
}
