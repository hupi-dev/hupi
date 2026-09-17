package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hupi/internal/identity"
)

// This file holds real end-user/team authentication — isolated here
// specifically so it can be lifted into a separately-licensed package
// later (see the open-core split plan). Everything here only ever
// matters once HUPI_REQUIRE_AUTH is turned on (cmd/hupi's resolveAuth),
// which is itself a Tier-3-only concept per docs/INSTALL.md's own tier
// definitions — Tier 1/2 never construct a TeamStore, never call
// Resolve, and never issue an API key. Unlike Store (auth.go), TeamStore
// doesn't need a *crypto.KeyStore: none of these operations provision an
// encryption key themselves (CreateUser/CreateTeam in auth.go already do
// that at scope-creation time).

// ErrInvalidKey is returned by Resolve for a key that doesn't hash to any
// non-revoked row — deliberately the same error for "key never existed"
// and "key was revoked," so a caller can't distinguish the two and use
// that to probe for valid-but-revoked keys.
var ErrInvalidKey = errors.New("auth: invalid or revoked API key")

// TeamStore resolves API keys and provisions/reads team membership — the
// real multi-user authentication surface, as opposed to Store's scope
// bookkeeping and operator login.
type TeamStore struct {
	db *sql.DB
}

func NewTeamStore(db *sql.DB) *TeamStore {
	return &TeamStore{db: db}
}

// Resolve maps a raw API key to the Identity it authenticates, including
// every team the underlying user is a member of.
func (s *TeamStore) Resolve(ctx context.Context, rawKey string) (identity.Identity, error) {
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

// CreateAPIKey generates a new key for userID and returns the raw value —
// the only time it's ever available in plaintext. Only the hash is
// persisted.
func (s *TeamStore) CreateAPIKey(ctx context.Context, userID string) (string, error) {
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

func (s *TeamStore) AddTeamMember(ctx context.Context, teamID, userID, role string) error {
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

// Membership and APIKey back cmd/hupi-admin-ui's read views over team
// membership and issued keys — see auth.go's User/Team for the same
// reasoning applied to scope bookkeeping.

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

func (s *TeamStore) ListTeamMembers(ctx context.Context, teamID string) ([]Membership, error) {
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
func (s *TeamStore) ListTeamsForUser(ctx context.Context, userID string) ([]Team, error) {
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

func (s *TeamStore) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
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
func (s *TeamStore) RevokeAPIKey(ctx context.Context, keyHash string) error {
	_, err := s.db.ExecContext(ctx, `
		update api_keys set revoked_at = now() where key_hash = $1 and revoked_at is null
	`, keyHash)
	if err != nil {
		return fmt.Errorf("auth: revoke API key: %w", err)
	}
	return nil
}
