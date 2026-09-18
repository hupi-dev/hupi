package dbscope

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/identity"
)

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

func TestRunSetsSessionScopeVariables(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	actingUser := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:dbscope-test-a"}
	workspace := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:dbscope-test-a"}

	var gotActingKind, gotActingOwner, gotWorkspaceKind, gotWorkspaceOwner string
	err := Run(ctx, db, actingUser, workspace, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select
				current_setting('hupi.acting_scope_kind', true),
				current_setting('hupi.acting_scope_owner', true),
				current_setting('hupi.workspace_scope_kind', true),
				current_setting('hupi.workspace_scope_owner', true)
		`).Scan(&gotActingKind, &gotActingOwner, &gotWorkspaceKind, &gotWorkspaceOwner)
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotActingKind != actingUser.Kind || gotActingOwner != actingUser.Owner {
		t.Errorf("acting scope = {%s %s}, want %+v", gotActingKind, gotActingOwner, actingUser)
	}
	if gotWorkspaceKind != workspace.Kind || gotWorkspaceOwner != workspace.Owner {
		t.Errorf("workspace scope = {%s %s}, want %+v", gotWorkspaceKind, gotWorkspaceOwner, workspace)
	}
}

func TestRunRollsBackOnFnError(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:dbscope-test-b"}

	sentinel := errors.New("deliberate failure")
	err := Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `insert into users (id) values ($1)`, "user:dbscope-test-b"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() error = %v, want the sentinel error", err)
	}

	var count int
	if err := db.QueryRowContext(ctx, `select count(*) from users where id = $1`, "user:dbscope-test-b").Scan(&count); err != nil {
		t.Fatalf("query users: %v", err)
	}
	if count != 0 {
		t.Errorf("expected the insert to be rolled back, but found %d row(s)", count)
		db.Exec(`delete from users where id = $1`, "user:dbscope-test-b")
	}
}
