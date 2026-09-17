// Package auth provisions identity against Postgres, and backs
// cmd/hupi-admin-ui's own operator login. Split across two files along a
// licensing seam (see the open-core split plan): this file (Store) holds
// what every tier uses — scope provisioning (CreateUser/CreateTeam,
// needed by hupi-export/hupi-import's backup/restore for any tier) and
// operator credentials (the admin UI's own login, which Tier 1/2 can use
// too via install.sh --admin-ui). team.go (TeamStore) holds real
// end-user/team authentication — API keys, Resolve, team membership —
// which only ever matters once HUPI_REQUIRE_AUTH is turned on, a Tier-3
// concept per docs/INSTALL.md's own tier definitions.
//
// It's a separate package from internal/store on purpose: store is about
// memory records (episodes/summaries/entities), auth is about who's
// allowed to touch them, and the two shouldn't blur together as Tier 3
// grows (e.g. RLS session variables in a later phase belong here, not
// mixed into query logic that has nothing to do with identity).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/identity"
)

// Store provisions scopes (users/teams — used by every tier's
// export/import backup-restore path, docs/HPMF) and resolves/provisions
// admin-UI operator credentials (used by any tier that opts into the
// admin UI). See TeamStore (team.go) for real end-user/team
// authentication, which this type deliberately does not hold.
type Store struct {
	db   *sql.DB
	keys *crypto.KeyStore // provisions each new scope's DEK eagerly, see D6 below
}

func New(db *sql.DB, keys *crypto.KeyStore) *Store {
	return &Store{db: db, keys: keys}
}

// hashKey is sha256 hex of the raw key — api_keys.key_hash never stores
// the raw key itself (schema/0002_tier3_phase1_identity.sql).
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateKey produces a new random raw API key. It is never stored or
// logged anywhere in plaintext by this package — the caller (an admin
// tool) is responsible for displaying it to the user exactly once.
func GenerateKey() (string, error) {
	return generateToken("hupi_sk_")
}

// GenerateOperatorToken is GenerateKey's counterpart for admin-UI operator
// credentials (schema/0008_admin_operators.sql) — a distinct prefix so a
// leaked-credential report can tell at a glance which kind it's looking
// at, same random-byte strength as an API key.
func GenerateOperatorToken() (string, error) {
	return generateToken("hupi_op_")
}

func generateToken(prefix string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return prefix + hex.EncodeToString(buf), nil
}

// CreateUser and CreateTeam are the scope-provisioning operations every
// tier's backup/restore path needs (cmd/hupi-export's loadActiveScopes,
// cmd/hupi-import's ensureScopeExists) — see TeamStore (team.go) for the
// CreateAPIKey/AddTeamMember provisioning that's genuinely Tier-3-only.

// CreateUser also eagerly provisions the new user's private-scope DEK
// (docs/HARDENING_PLAN.md D6) — key lifecycle stays colocated with
// identity lifecycle, rather than lazily created on whichever code path
// happens to write first.
func (s *Store) CreateUser(ctx context.Context, userID, email string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into users (id, email) values ($1, $2)
		on conflict (id) do nothing
	`, userID, nullableEmail(email))
	if err != nil {
		return fmt.Errorf("auth: create user %s: %w", userID, err)
	}
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: userID}
	if _, _, err := s.keys.GetOrCreate(ctx, scope); err != nil {
		return fmt.Errorf("auth: provision encryption key for user %s: %w", userID, err)
	}
	return nil
}

func nullableEmail(email string) any {
	if email == "" {
		return nil
	}
	return email
}

// CreateTeam also eagerly provisions the new team's shared-scope DEK —
// see CreateUser's doc comment (D6).
func (s *Store) CreateTeam(ctx context.Context, teamID, name string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into teams (id, name) values ($1, $2)
		on conflict (id) do nothing
	`, teamID, name)
	if err != nil {
		return fmt.Errorf("auth: create team %s: %w", teamID, err)
	}
	scope := identity.Scope{Kind: identity.ScopeKindShared, Owner: teamID}
	if _, _, err := s.keys.GetOrCreate(ctx, scope); err != nil {
		return fmt.Errorf("auth: provision encryption key for team %s: %w", teamID, err)
	}
	return nil
}

// User and Team back cmd/hupi-admin-ui's read views — the CLI
// (cmd/hupi-admin) never needed to read anything back, only create. A web
// UI does: you can't render a page of "who already exists" without a
// list query.

type User struct {
	ID        string
	Email     string // "" if not set
	CreatedAt time.Time
}

type Team struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `select id, coalesce(email, ''), created_at from users order by created_at`)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) ListTeams(ctx context.Context) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, `select id, name, created_at from teams order by created_at`)
	if err != nil {
		return nil, fmt.Errorf("auth: list teams: %w", err)
	}
	defer rows.Close()

	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// The types and methods below are Tier 3's admin-UI operator model
// (schema/0008_admin_operators.sql, docs/GAP_CLOSURE_PLAN.md §4.3) —
// named credentials replacing hupi-admin-ui's single shared
// HUPI_ADMIN_UI_TOKEN, so an audit_log "admin_ui_view" entry can say who,
// not just "the admin UI." Deliberately its own small set of methods
// rather than reusing CreateUser/CreateAPIKey/Resolve: an operator is not
// a user (no private memory scope, no DEK, never appears in
// team_members) and ErrInvalidOperator is intentionally the same error
// for "never existed" and "revoked," mirroring ErrInvalidKey's reasoning.

var ErrInvalidOperator = errors.New("auth: invalid or revoked operator token")

type Operator struct {
	Name      string
	CreatedAt time.Time
	RevokedAt *time.Time
}

// CreateOperator generates a new operator credential and returns the raw
// token — the only time it's ever available in plaintext, same contract
// as CreateAPIKey.
func (s *Store) CreateOperator(ctx context.Context, name string) (string, error) {
	rawToken, err := GenerateOperatorToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		insert into operators (name, token_hash) values ($1, $2)
	`, name, hashKey(rawToken))
	if err != nil {
		return "", fmt.Errorf("auth: create operator %s: %w", name, err)
	}
	return rawToken, nil
}

// ResolveOperator maps a raw operator token to the operator name that
// authenticates it — cmd/hupi-admin-ui's replacement for comparing
// against a single shared HUPI_ADMIN_UI_TOKEN.
func (s *Store) ResolveOperator(ctx context.Context, rawToken string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `
		select name from operators where token_hash = $1 and revoked_at is null
	`, hashKey(rawToken)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidOperator
	}
	if err != nil {
		return "", fmt.Errorf("auth: resolve operator token: %w", err)
	}
	return name, nil
}

func (s *Store) ListOperators(ctx context.Context) ([]Operator, error) {
	rows, err := s.db.QueryContext(ctx, `select name, created_at, revoked_at from operators order by created_at`)
	if err != nil {
		return nil, fmt.Errorf("auth: list operators: %w", err)
	}
	defer rows.Close()

	var out []Operator
	for rows.Next() {
		var o Operator
		if err := rows.Scan(&o.Name, &o.CreatedAt, &o.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RevokeOperator is idempotent, same reasoning as RevokeAPIKey.
func (s *Store) RevokeOperator(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `
		update operators set revoked_at = now() where name = $1 and revoked_at is null
	`, name)
	if err != nil {
		return fmt.Errorf("auth: revoke operator %s: %w", name, err)
	}
	return nil
}
