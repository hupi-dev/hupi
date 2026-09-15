// Package auth resolves API keys to identity.Identity and provisions the
// users/teams/api_keys rows Tier 3 needs — see docs/TIER3_PLAN.md Phase 3.
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

// ErrInvalidKey is returned by Resolve for a key that doesn't hash to any
// non-revoked row — deliberately the same error for "key never existed"
// and "key was revoked," so a caller can't distinguish the two and use
// that to probe for valid-but-revoked keys.
var ErrInvalidKey = errors.New("auth: invalid or revoked API key")

// Store resolves and provisions identity against Postgres.
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

// Resolve maps a raw API key to the Identity it authenticates, including
// every team the underlying user is a member of.
func (s *Store) Resolve(ctx context.Context, rawKey string) (identity.Identity, error) {
	var userID string
	err := s.db.QueryRowContext(ctx, `
		select user_id from api_keys where key_hash = $1 and revoked_at is null
	`, hashKey(rawKey)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, ErrInvalidKey
	}
	if err != nil {
		return identity.Identity{}, fmt.Errorf("auth: resolve API key: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `select team_id from team_members where user_id = $1`, userID)
	if err != nil {
		return identity.Identity{}, fmt.Errorf("auth: load team memberships for %s: %w", userID, err)
	}
	defer rows.Close()

	var teamIDs []string
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			return identity.Identity{}, err
		}
		teamIDs = append(teamIDs, teamID)
	}
	if err := rows.Err(); err != nil {
		return identity.Identity{}, err
	}

	return identity.Identity{UserID: userID, TeamIDs: teamIDs}, nil
}

// CreateUser, CreateAPIKey, CreateTeam, and AddTeamMember are the minimal
// provisioning operations Tier 3 needs. cmd/hupi-admin (Phase 6) is a
// thin CLI wrapper over exactly these methods; cmd/hupi-admin-ui wraps
// the same methods plus the List*/Revoke* ones below for a browser-based
// operator surface — see docs/ADMIN_UI.md for why that reverses
// TIER3_PLAN.md's original "CLI only" non-goal.

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

// CreateAPIKey generates a new key for userID and returns the raw value —
// the only time it's ever available in plaintext. Only the hash is
// persisted.
func (s *Store) CreateAPIKey(ctx context.Context, userID string) (string, error) {
	rawKey, err := GenerateKey()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `
		insert into api_keys (key_hash, user_id) values ($1, $2)
	`, hashKey(rawKey), userID)
	if err != nil {
		return "", fmt.Errorf("auth: create API key for %s: %w", userID, err)
	}
	return rawKey, nil
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

func (s *Store) AddTeamMember(ctx context.Context, teamID, userID, role string) error {
	if role == "" {
		role = "member"
	}
	_, err := s.db.ExecContext(ctx, `
		insert into team_members (team_id, user_id, role) values ($1, $2, $3)
		on conflict (team_id, user_id) do update set role = excluded.role
	`, teamID, userID, role)
	if err != nil {
		return fmt.Errorf("auth: add %s to team %s: %w", userID, teamID, err)
	}
	return nil
}

// The types and methods below exist only for cmd/hupi-admin-ui — the CLI
// (cmd/hupi-admin) never needed to read anything back, only create. A web
// UI does: you can't render a page of "who already exists" without a
// list query, and you can't offer a revoke button without one.

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

type Membership struct {
	UserID string
	Role   string
}

type APIKey struct {
	// KeyHash identifies the row for revocation purposes. It is a sha256
	// hash, never the raw key — the raw key was never persisted anywhere
	// (see GenerateKey/CreateAPIKey) and can't be recovered from this.
	KeyHash   string
	CreatedAt time.Time
	RevokedAt *time.Time // nil if still active
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

func (s *Store) ListTeamMembers(ctx context.Context, teamID string) ([]Membership, error) {
	rows, err := s.db.QueryContext(ctx, `
		select user_id, role from team_members where team_id = $1 order by user_id
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("auth: list members of %s: %w", teamID, err)
	}
	defer rows.Close()

	var out []Membership
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.UserID, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListTeamsForUser is the inverse of ListTeamMembers — used by the admin
// UI's user detail page so an operator can see a user's memberships
// without cross-referencing every team separately.
func (s *Store) ListTeamsForUser(ctx context.Context, userID string) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, `
		select t.id, t.name, t.created_at
		from teams t
		join team_members m on m.team_id = t.id
		where m.user_id = $1
		order by t.created_at
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list teams for %s: %w", userID, err)
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

func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		select key_hash, created_at, revoked_at from api_keys where user_id = $1 order by created_at
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list API keys for %s: %w", userID, err)
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.KeyHash, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey is idempotent: revoking an already-revoked or nonexistent
// hash is not an error, matching ErrInvalidKey's "don't let a caller
// distinguish never-existed from already-gone" posture above.
func (s *Store) RevokeAPIKey(ctx context.Context, keyHash string) error {
	_, err := s.db.ExecContext(ctx, `
		update api_keys set revoked_at = now() where key_hash = $1 and revoked_at is null
	`, keyHash)
	if err != nil {
		return fmt.Errorf("auth: revoke API key: %w", err)
	}
	return nil
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
