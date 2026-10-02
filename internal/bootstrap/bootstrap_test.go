package bootstrap

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/identity"
)

// Integration test, gated behind a real Postgres instance (see
// internal/store/scope_isolation_test.go's doc comment for how to run
// against one) — reproduces the fresh-install gap this fixes: a brand-new
// deployment with no legacy wrapped-DEK file never had identity.DefaultUserID
// registered in the users table, so cmd/hupi-consolidate's loadActiveScopes
// never discovered it and daily consolidation silently never ran for it.
func TestEnsureDefaultUser(t *testing.T) {
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
	t.Cleanup(func() {
		_, _ = db.Exec(`delete from users where id = $1`, identity.DefaultUserID)
	})

	// Simulate a genuinely fresh install: no row for the default user yet.
	if _, err := db.ExecContext(ctx, `delete from users where id = $1`, identity.DefaultUserID); err != nil {
		t.Fatalf("clear pre-existing default user row: %v", err)
	}

	if err := ensureDefaultUser(ctx, db); err != nil {
		t.Fatalf("ensureDefaultUser: %v", err)
	}

	var exists bool
	if err := db.QueryRowContext(ctx,
		`select exists(select 1 from users where id = $1)`, identity.DefaultUserID,
	).Scan(&exists); err != nil {
		t.Fatalf("check default user row: %v", err)
	}
	if !exists {
		t.Fatal("ensureDefaultUser did not create the default user row")
	}

	// Idempotent: calling it again (every subsequent startup) must not error.
	if err := ensureDefaultUser(ctx, db); err != nil {
		t.Fatalf("ensureDefaultUser (second call): %v", err)
	}
}

func TestResolveDatabaseURL(t *testing.T) {
	t.Run("prefers HUPI_APP_DATABASE_URL when set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "postgres://hupi_app@localhost/hupi")
		t.Setenv("HUPI_DATABASE_URL", "postgres://postgres@localhost/hupi")

		got, err := resolveDatabaseURL()
		if err != nil {
			t.Fatalf("resolveDatabaseURL: %v", err)
		}
		if got != "postgres://hupi_app@localhost/hupi" {
			t.Errorf("resolveDatabaseURL() = %q, want the app-role URL", got)
		}
	})

	t.Run("falls back to HUPI_DATABASE_URL when the app role isn't set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "")
		t.Setenv("HUPI_DATABASE_URL", "postgres://postgres@localhost/hupi")

		got, err := resolveDatabaseURL()
		if err != nil {
			t.Fatalf("resolveDatabaseURL: %v", err)
		}
		if got != "postgres://postgres@localhost/hupi" {
			t.Errorf("resolveDatabaseURL() = %q, want the fallback URL", got)
		}
	})

	t.Run("errors when neither is set", func(t *testing.T) {
		t.Setenv("HUPI_APP_DATABASE_URL", "")
		t.Setenv("HUPI_DATABASE_URL", "")

		if _, err := resolveDatabaseURL(); err == nil {
			t.Fatal("resolveDatabaseURL: expected an error when neither env var is set")
		}
	})
}

func TestRequireEnv(t *testing.T) {
	t.Run("returns the value when set", func(t *testing.T) {
		t.Setenv("HUPI_TEST_REQUIRE_ENV_VAR", "a-value")
		got, err := requireEnv("HUPI_TEST_REQUIRE_ENV_VAR")
		if err != nil {
			t.Fatalf("requireEnv: %v", err)
		}
		if got != "a-value" {
			t.Errorf("requireEnv() = %q, want %q", got, "a-value")
		}
	})

	t.Run("errors when unset", func(t *testing.T) {
		t.Setenv("HUPI_TEST_REQUIRE_ENV_VAR", "")
		if _, err := requireEnv("HUPI_TEST_REQUIRE_ENV_VAR"); err == nil {
			t.Fatal("requireEnv: expected an error for an unset variable")
		}
	})
}

func TestConfigPath(t *testing.T) {
	t.Run("defaults to providers.yaml", func(t *testing.T) {
		t.Setenv("HUPI_PROVIDERS_CONFIG", "")
		if got := configPath(); got != "providers.yaml" {
			t.Errorf("configPath() = %q, want %q", got, "providers.yaml")
		}
	})

	t.Run("honors HUPI_PROVIDERS_CONFIG when set", func(t *testing.T) {
		t.Setenv("HUPI_PROVIDERS_CONFIG", "/etc/hupi/providers.yaml")
		if got := configPath(); got != "/etc/hupi/providers.yaml" {
			t.Errorf("configPath() = %q, want %q", got, "/etc/hupi/providers.yaml")
		}
	})
}
