package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/auth"
	"hupi/internal/bootstrap"
	"hupi/internal/crypto"
	"hupi/internal/identity"
)

func TestListenAddr(t *testing.T) {
	t.Run("defaults to 127.0.0.1:8787", func(t *testing.T) {
		t.Setenv("HUPI_LISTEN_ADDR", "")
		if got := listenAddr(); got != "127.0.0.1:8787" {
			t.Errorf("listenAddr() = %q, want %q", got, "127.0.0.1:8787")
		}
	})

	t.Run("honors HUPI_LISTEN_ADDR when set", func(t *testing.T) {
		t.Setenv("HUPI_LISTEN_ADDR", "0.0.0.0:9000")
		if got := listenAddr(); got != "0.0.0.0:9000" {
			t.Errorf("listenAddr() = %q, want %q", got, "0.0.0.0:9000")
		}
	})
}

func TestHandleLiveness(t *testing.T) {
	rec := httptest.NewRecorder()
	handleLiveness(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("handleLiveness status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// testDB opens a connection against HUPI_TEST_DATABASE_URL — same
// skip-if-unset convention used throughout this repo's other integration
// tests (see e.g. internal/auth/team_test.go in hupi-t3).
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

func TestHandleReadiness(t *testing.T) {
	t.Run("200 when the database is reachable", func(t *testing.T) {
		db := testDB(t)
		rec := httptest.NewRecorder()
		handleReadiness(db)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("handleReadiness status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	})

	t.Run("503 when the database is unreachable", func(t *testing.T) {
		db, err := sql.Open("pgx", "postgres://nobody:nothing@127.0.0.1:1/doesnotexist?sslmode=disable&connect_timeout=1")
		if err != nil {
			t.Fatalf("open unreachable database: %v", err)
		}
		defer db.Close()

		rec := httptest.NewRecorder()
		handleReadiness(db)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("handleReadiness status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

// stubTeamAuthenticator is the minimal fake needed to prove resolveAuth
// actually returns whatever auth.NewTeamAuthenticator hands back — the
// public repo alone has no concrete TeamAuthenticator implementation at
// all (that type lives in the private hupi-t3 overlay), so this stub is
// this test's only option for simulating "the Tier-3 extension is
// present" without a real Tier-3 build.
type stubTeamAuthenticator struct{}

func (stubTeamAuthenticator) Resolve(ctx context.Context, apiKey string) (identity.Identity, error) {
	return identity.Identity{}, nil
}
func (stubTeamAuthenticator) CreateAPIKey(ctx context.Context, userID string) (string, error) {
	return "", nil
}
func (stubTeamAuthenticator) ListAPIKeys(ctx context.Context, userID string) ([]auth.APIKey, error) {
	return nil, nil
}
func (stubTeamAuthenticator) RevokeAPIKey(ctx context.Context, keyHash string) error {
	return nil
}
func (stubTeamAuthenticator) AddTeamMember(ctx context.Context, teamID, userID, role string) error {
	return nil
}
func (stubTeamAuthenticator) ListTeamMembers(ctx context.Context, teamID string) ([]auth.Membership, error) {
	return nil, nil
}
func (stubTeamAuthenticator) ListTeamsForUser(ctx context.Context, userID string) ([]auth.Team, error) {
	return nil, nil
}

func TestResolveAuth(t *testing.T) {
	t.Run("nil, nil when HUPI_REQUIRE_AUTH is unset — Tier 1/2, no auth required", func(t *testing.T) {
		t.Setenv("HUPI_REQUIRE_AUTH", "")
		got, err := resolveAuth(&bootstrap.Deps{})
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		if got != nil {
			t.Errorf("resolveAuth() = %v, want nil", got)
		}
	})

	t.Run("errors when HUPI_REQUIRE_AUTH=true but the Tier-3 extension isn't present", func(t *testing.T) {
		t.Setenv("HUPI_REQUIRE_AUTH", "true")
		original := auth.NewTeamAuthenticator
		auth.NewTeamAuthenticator = nil
		defer func() { auth.NewTeamAuthenticator = original }()

		_, err := resolveAuth(&bootstrap.Deps{})
		if err == nil {
			t.Fatal("resolveAuth: expected an error when NewTeamAuthenticator is nil, got none")
		}
	})

	t.Run("delegates to auth.NewTeamAuthenticator when the Tier-3 extension is present", func(t *testing.T) {
		t.Setenv("HUPI_REQUIRE_AUTH", "true")
		original := auth.NewTeamAuthenticator
		auth.NewTeamAuthenticator = func(db *sql.DB, keys *crypto.KeyStore) auth.TeamAuthenticator {
			return stubTeamAuthenticator{}
		}
		defer func() { auth.NewTeamAuthenticator = original }()

		got, err := resolveAuth(&bootstrap.Deps{})
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		if got == nil {
			t.Error("resolveAuth() = nil, want the stub authenticator")
		}
	})
}
