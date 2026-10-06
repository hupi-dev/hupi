package schema

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestMigration0004Probe_IsPerDatabaseNotJustRoleExistence is the real
// regression test for a confirmed bug: migrate.sh's/install.sh's
// idempotency probe for 0004_hardening_phase1_app_role.sql originally
// checked only `pg_roles` — role existence, which is cluster-wide. But
// 0004's own GRANT statements are scoped to whichever database the
// script runs against. On a Postgres cluster that already has hupi_app
// (from a different, already-migrated database — the completely normal
// case for a shared dev/CI cluster hosting more than one logical
// database), the old probe reported "already applied" against a brand
// new database and silently skipped 0004 there, leaving hupi_app with
// zero privileges in it. Every real binary then failed at startup with
// "permission denied for table users". Found by an E2E harness pass
// hitting this directly; reproduced and fixed here against a real,
// disposable database on the same cluster.
//
// Skips itself (not a failure) if HUPI_ADMIN_DATABASE_URL isn't set —
// this needs CREATE DATABASE privileges, a stronger requirement than
// the HUPI_TEST_DATABASE_URL (hupi_app-level) gate every other
// real-Postgres test in this repo uses.
func TestMigration0004Probe_IsPerDatabaseNotJustRoleExistence(t *testing.T) {
	adminURL := os.Getenv("HUPI_ADMIN_DATABASE_URL")
	if adminURL == "" {
		t.Skip("HUPI_ADMIN_DATABASE_URL not set; skipping (needs CREATE DATABASE privileges)")
	}
	ctx := context.Background()

	adminDB, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer adminDB.Close()

	const probeDBName = "hupi_migration_probe_test"
	// DSN rewriting is brittle in general, but this repo's own
	// HUPI_ADMIN_DATABASE_URL convention is always
	// "postgres://user:pass@host:port/<dbname>?params" (see
	// docs/INSTALL.md) — swapping the path segment is the same trick
	// internal/hpmf's own tests use for a scoped throwaway resource,
	// just at database granularity instead of a scope string.
	probeDSN, err := rewriteDatabaseName(adminURL, probeDBName)
	if err != nil {
		t.Fatalf("rewrite admin DSN for probe database: %v", err)
	}

	if _, err := adminDB.ExecContext(ctx, "drop database if exists "+probeDBName); err != nil {
		t.Fatalf("drop pre-existing probe database: %v", err)
	}
	if _, err := adminDB.ExecContext(ctx, "create database "+probeDBName); err != nil {
		t.Fatalf("create probe database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), "drop database if exists "+probeDBName)
	})

	probeDB, err := sql.Open("pgx", probeDSN)
	if err != nil {
		t.Fatalf("open probe database connection: %v", err)
	}
	defer probeDB.Close()

	// Minimal stand-in for 0001_init.sql's real `episodes` table — only
	// what the probe itself needs to exist, not a full schema replay.
	if _, err := probeDB.ExecContext(ctx, "create table episodes (id text)"); err != nil {
		t.Fatalf("create minimal episodes table: %v", err)
	}
	// hupi_app is cluster-wide, so it already exists from whatever
	// other test/migration created it first — idempotent create here
	// makes this test self-contained regardless of run order.
	if _, err := adminDB.ExecContext(ctx, `
		do $$
		begin
			if not exists (select from pg_roles where rolname = 'hupi_app') then
				create role hupi_app with login;
			end if;
		end
		$$;
	`); err != nil {
		t.Fatalf("ensure hupi_app role exists: %v", err)
	}

	const probeSQL = "select exists(select 1 from information_schema.table_privileges where table_name='episodes' and grantee='hupi_app' and privilege_type='SELECT')"

	var appliedBefore bool
	if err := probeDB.QueryRowContext(ctx, probeSQL).Scan(&appliedBefore); err != nil {
		t.Fatalf("run probe before grants: %v", err)
	}
	if appliedBefore {
		t.Fatal("probe reports 0004 already applied in a fresh database before any grant was ever run there — the bug this test exists to catch")
	}

	// The real 0004 migration's own grant statements (schema/0004's own
	// file), not a paraphrase — if that file's grants ever change, this
	// test should be updated to match, not silently keep passing
	// against a stale copy.
	if _, err := probeDB.ExecContext(ctx, `
		grant usage on schema public to hupi_app;
		grant select, insert, update, delete on episodes to hupi_app;
	`); err != nil {
		t.Fatalf("apply 0004-equivalent grants: %v", err)
	}

	var appliedAfter bool
	if err := probeDB.QueryRowContext(ctx, probeSQL).Scan(&appliedAfter); err != nil {
		t.Fatalf("run probe after grants: %v", err)
	}
	if !appliedAfter {
		t.Fatal("probe reports 0004 NOT applied immediately after granting the exact privilege it checks for")
	}
}

// rewriteDatabaseName swaps the path component of a Postgres DSN
// (postgres://user:pass@host:port/dbname?params) for a different
// database name on the same server — used to point at a disposable
// probe database without needing a second admin URL env var.
func rewriteDatabaseName(dsn, newDBName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/" + newDBName
	return u.String(), nil
}
