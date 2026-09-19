// Package demo backs the public hosted demo (docs/TODO.md #2): anonymous,
// short-lived guest sessions, capped on both message count and
// consolidate-now calls, scoped to a normal Tier-1/2 private-scope
// user — deliberately not a team. Team/OIDC auth is a Tier-3 sales
// conversation, not something a public demo of the free core should
// need; auth.Store.CreateUser already does exactly the scope
// provisioning (including DEK) a guest needs, with nothing Tier-3-only
// involved.
//
// cmd/hupi-demo wires a *Store in as gateway.Handler.Auth directly — it
// implements gateway.Authenticator, so a live chat turn gets 100% of the
// real retrieval/injection/capture path unmodified. cmd/hupi-demo-sweep
// calls Sweep on a cron schedule to delete expired guests' data.
package demo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"hupi/internal/auth"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// ConsolidationRunner is the one consolidation.Runner method ConsolidateNow
// needs — an interface so tests can stub it without a real LLM call, and
// so cmd/hupi-demo-sweep (which never consolidates) can pass nil.
type ConsolidationRunner interface {
	RunDaily(ctx context.Context, scope identity.Scope, date time.Time) error
}

// Limits are the demo's cost guards, all overridable by cmd/hupi-demo
// from environment variables. DefaultLimits are deliberately
// conservative — a missing env var should fail toward "too strict," not
// "wide open."
type Limits struct {
	SessionTTL                time.Duration
	MaxMessagesPerSession     int
	MaxConsolidatesPerSession int
	MaxSessionsPerDay         int
}

var DefaultLimits = Limits{
	SessionTTL:                3 * time.Hour,
	MaxMessagesPerSession:     18,
	MaxConsolidatesPerSession: 3,
	MaxSessionsPerDay:         150,
}

var (
	// ErrSessionInvalid covers "never existed," "expired," and (from
	// Resolve only) "over its message cap" with the same value on
	// purpose — gateway.Handler.resolveIdentity already collapses any
	// Resolve error to a generic "invalid API key" 401 regardless
	// (internal/gateway/handler.go), so the chat route never needed the
	// distinction. ConsolidateNow uses this same error only for the
	// gone/expired case, since that route can afford (and its frontend
	// needs) to tell that apart from ErrConsolidateLimitReached.
	ErrSessionInvalid          = errors.New("demo: session not found or expired")
	ErrConsolidateLimitReached = errors.New("demo: consolidate-now limit reached for this session")
	ErrDailySessionCapReached  = errors.New("demo: daily new-session cap reached")
)

type Store struct {
	db     *sql.DB
	auth   *auth.Store
	runner ConsolidationRunner
	limits Limits
}

func New(db *sql.DB, authStore *auth.Store, runner ConsolidationRunner, limits Limits) *Store {
	return &Store{db: db, auth: authStore, runner: runner, limits: limits}
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("demo: generate random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// Session is what CreateSession hands back — the raw token is never
// stored anywhere, same contract as auth.GenerateKey.
type Session struct {
	Token             string
	GuestUserID       string
	ExpiresAt         time.Time
	MessagesRemaining int
}

// CreateSession provisions a new guest identity (a normal private-scope
// user — auth.Store.CreateUser eagerly provisions its DEK, same as any
// other user) and the session row gating it. Refuses once
// Limits.MaxSessionsPerDay sessions have already been created in the
// last 24 hours — the real, restart-surviving spend backstop; the
// caller (cmd/hupi-demo) also applies a cheaper, in-memory per-IP limit
// in front of this one.
func (s *Store) CreateSession(ctx context.Context) (Session, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		`select count(*) from demo_sessions where created_at > now() - interval '1 day'`,
	).Scan(&count); err != nil {
		return Session{}, fmt.Errorf("demo: count today's sessions: %w", err)
	}
	if count >= s.limits.MaxSessionsPerDay {
		return Session{}, ErrDailySessionCapReached
	}

	guestSuffix, err := randomHex(8)
	if err != nil {
		return Session{}, err
	}
	guestID := "user:demo-" + guestSuffix
	if err := s.auth.CreateUser(ctx, guestID, ""); err != nil {
		return Session{}, fmt.Errorf("demo: provision guest user: %w", err)
	}

	tokenSuffix, err := randomHex(24)
	if err != nil {
		return Session{}, err
	}
	rawToken := "hupi_demo_" + tokenSuffix
	expiresAt := time.Now().Add(s.limits.SessionTTL)
	if _, err := s.db.ExecContext(ctx,
		`insert into demo_sessions (token_hash, guest_user_id, expires_at) values ($1, $2, $3)`,
		hashToken(rawToken), guestID, expiresAt,
	); err != nil {
		return Session{}, fmt.Errorf("demo: create session: %w", err)
	}

	return Session{
		Token:             rawToken,
		GuestUserID:       guestID,
		ExpiresAt:         expiresAt,
		MessagesRemaining: s.limits.MaxMessagesPerSession,
	}, nil
}

// Resolve implements gateway.Authenticator. One atomic statement checks
// the session is unexpired and under its message cap, and counts this
// call toward that cap in the same statement — Postgres serializes
// concurrent updates to the same row, so two requests racing on the same
// session can't both squeak past the limit.
func (s *Store) Resolve(ctx context.Context, rawToken string) (identity.Identity, error) {
	var guestID string
	err := s.db.QueryRowContext(ctx, `
		update demo_sessions
		set message_count = message_count + 1
		where token_hash = $1 and expires_at > now() and message_count < $2
		returning guest_user_id
	`, hashToken(rawToken), s.limits.MaxMessagesPerSession).Scan(&guestID)
	if errors.Is(err, sql.ErrNoRows) {
		return identity.Identity{}, ErrSessionInvalid
	}
	if err != nil {
		return identity.Identity{}, fmt.Errorf("demo: resolve session: %w", err)
	}
	return identity.Identity{UserID: guestID}, nil
}

// ConsolidateNow runs the real production consolidation pass
// (consolidation.Runner.RunDaily) for just this session's guest scope,
// for today — the "watch it consolidate" demo beat, not a simulation of
// one. Deliberately doesn't touch message_count: consolidating isn't
// sending a chat message, and gating it with its own, separate counter
// means a visitor can't use it as an unmetered side channel for extra
// LLM calls.
func (s *Store) ConsolidateNow(ctx context.Context, rawToken string) error {
	hash := hashToken(rawToken)
	var guestID string
	err := s.db.QueryRowContext(ctx, `
		update demo_sessions
		set consolidate_count = consolidate_count + 1
		where token_hash = $1 and expires_at > now() and consolidate_count < $2
		returning guest_user_id
	`, hash, s.limits.MaxConsolidatesPerSession).Scan(&guestID)
	if errors.Is(err, sql.ErrNoRows) {
		// The update above can return zero rows for two different
		// reasons (gone/expired vs. at its cap) — worth telling apart
		// here (unlike Resolve) since this route can, and its frontend
		// needs to know whether to offer "start over" or just "you've
		// seen this part already." ConsolidateNow is called at most a
		// handful of times per session, so one extra read costs nothing
		// worth avoiding.
		var expired bool
		lookupErr := s.db.QueryRowContext(ctx,
			`select expires_at < now() from demo_sessions where token_hash = $1`, hash,
		).Scan(&expired)
		if errors.Is(lookupErr, sql.ErrNoRows) || expired {
			return ErrSessionInvalid
		}
		return ErrConsolidateLimitReached
	}
	if err != nil {
		return fmt.Errorf("demo: check consolidate limit: %w", err)
	}

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: guestID}
	if err := s.runner.RunDaily(ctx, scope, time.Now()); err != nil {
		return fmt.Errorf("demo: consolidate: %w", err)
	}
	return nil
}

// Sweep deletes every guest session past its expiry, or past
// hardBackstopAge regardless of expires_at (a backstop in case TTL logic
// ever misbehaves), along with everything that guest wrote. Returns how
// many sessions were swept, for cmd/hupi-demo-sweep's own logging.
func (s *Store) Sweep(ctx context.Context, hardBackstopAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-hardBackstopAge)
	rows, err := s.db.QueryContext(ctx, `
		select guest_user_id from demo_sessions
		where expires_at < now() or created_at < $1
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("demo: sweep query: %w", err)
	}
	var guestIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("demo: scan sweep row: %w", err)
		}
		guestIDs = append(guestIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	rows.Close()

	for _, guestID := range guestIDs {
		if err := s.deleteGuest(ctx, guestID); err != nil {
			return 0, fmt.Errorf("demo: sweep guest %s: %w", guestID, err)
		}
	}
	return len(guestIDs), nil
}

// deleteGuest removes a guest's memory content, its encryption key, and
// its identity row. episodes/summaries/entities are plain
// scope_kind/scope_owner text columns, not FK'd to users (scope_owner
// can name a team instead), so they need explicit deletes; the users row
// deletion cascades to demo_sessions via its own FK
// (schema/0014_demo_sessions.sql). Those three tables have row-level
// security enabled (schema/0005) and hupi_app is a non-owner role, so
// the deletes must run inside a dbscope.Run transaction carrying this
// guest's own scope — the same requirement every other write against
// them already has (internal/store, internal/consolidation). scope_keys
// and users are not RLS-scoped (schema/0006's own doc comment), so
// deleting them in the same transaction is just convenient, not
// required.
func (s *Store) deleteGuest(ctx context.Context, guestID string) error {
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: guestID}
	return dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`delete from episodes where scope_kind = 'private' and scope_owner = $1`,
			`delete from summaries where scope_kind = 'private' and scope_owner = $1`,
			`delete from entities where scope_kind = 'private' and scope_owner = $1`,
			`delete from scope_keys where scope_kind = 'private' and scope_owner = $1`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, guestID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `delete from users where id = $1`, guestID)
		return err
	})
}
