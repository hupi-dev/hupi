package demo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/auth"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// testDB/kek follow the exact convention already used by
// internal/crypto/keystore_test.go and internal/audit/audit_test.go —
// same HUPI_TEST_DATABASE_URL, same fixed all-zero test KEK.
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

type stubRunner struct {
	calls int
	err   error
}

func (s *stubRunner) RunDaily(ctx context.Context, scope identity.Scope, date time.Time) error {
	s.calls++
	return s.err
}

// newTestStore wires a real Store against the test database, cleaning up
// every guest it creates afterward — demo_sessions/users (unlike
// audit_log) are not insert-only for hupi_app, so this cleanup actually
// works and prevents cross-test pollution of CreateSession's global
// daily-count query.
func newTestStore(t *testing.T, db *sql.DB, runner ConsolidationRunner, limits Limits) *Store {
	t.Helper()
	kek := make([]byte, 32)
	keys := crypto.NewKeyStore(db, kek)
	authStore := auth.New(db, keys)
	return New(db, authStore, runner, limits)
}

// trackGuest registers a guest id (from a Session returned by
// CreateSession) for cleanup once the test ends.
func trackGuest(t *testing.T, db *sql.DB, guestID string) {
	t.Helper()
	t.Cleanup(func() {
		// scope_keys is not RLS-protected (schema/0006), and none of
		// these tests write episodes/summaries/entities for a guest it
		// tracks this way (TestSweep_DeletesExpiredGuestAndItsData is
		// the one test that does, and it relies on Sweep — via
		// dbscope.Run — to clean those up instead, not this helper).
		db.Exec(`delete from demo_sessions where guest_user_id = $1`, guestID)
		db.Exec(`delete from scope_keys where scope_kind = 'private' and scope_owner = $1`, guestID)
		db.Exec(`delete from users where id = $1`, guestID)
	})
}

func testLimits() Limits {
	return Limits{
		SessionTTL:                time.Hour,
		MaxMessagesPerSession:     2,
		MaxConsolidatesPerSession: 1,
		MaxSessionsPerDay:         1_000_000, // effectively unlimited unless a test overrides it
	}
}

func TestCreateSession_ProvisionsGuestAndSession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	if sess.Token == "" || sess.GuestUserID == "" {
		t.Fatalf("expected non-empty token and guest id, got %+v", sess)
	}
	if sess.MessagesRemaining != testLimits().MaxMessagesPerSession {
		t.Fatalf("MessagesRemaining = %d, want %d", sess.MessagesRemaining, testLimits().MaxMessagesPerSession)
	}

	var userExists bool
	if err := db.QueryRowContext(ctx, `select exists(select 1 from users where id = $1)`, sess.GuestUserID).Scan(&userExists); err != nil {
		t.Fatalf("check guest user: %v", err)
	}
	if !userExists {
		t.Fatalf("guest user %s was not provisioned", sess.GuestUserID)
	}
}

func TestResolve_EnforcesMessageCapAndUnknownToken(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	limits := testLimits() // MaxMessagesPerSession = 2
	s := newTestStore(t, db, &stubRunner{}, limits)

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	for i := 0; i < limits.MaxMessagesPerSession; i++ {
		id, err := s.Resolve(ctx, sess.Token)
		if err != nil {
			t.Fatalf("Resolve call %d: unexpected error: %v", i+1, err)
		}
		if id.UserID != sess.GuestUserID {
			t.Fatalf("Resolve call %d: UserID = %q, want %q", i+1, id.UserID, sess.GuestUserID)
		}
	}

	if _, err := s.Resolve(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("Resolve after cap exhausted: err = %v, want ErrSessionInvalid", err)
	}

	if _, err := s.Resolve(ctx, "hupi_demo_totally-unknown-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("Resolve with unknown token: err = %v, want ErrSessionInvalid", err)
	}
}

func TestResolve_ExpiredSessionRejected(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	if _, err := db.ExecContext(ctx,
		`update demo_sessions set expires_at = now() - interval '1 minute' where token_hash = $1`,
		hashToken(sess.Token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}

	if _, err := s.Resolve(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("Resolve on expired session: err = %v, want ErrSessionInvalid", err)
	}
}

func TestConsolidateNow_RunsOnceThenCaps(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	runner := &stubRunner{}
	limits := testLimits() // MaxConsolidatesPerSession = 1
	s := newTestStore(t, db, runner, limits)

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	if err := s.ConsolidateNow(ctx, sess.Token); err != nil {
		t.Fatalf("first ConsolidateNow: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.calls = %d, want 1", runner.calls)
	}

	if err := s.ConsolidateNow(ctx, sess.Token); !errors.Is(err, ErrConsolidateLimitReached) {
		t.Fatalf("second ConsolidateNow: err = %v, want ErrConsolidateLimitReached", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner.calls after cap = %d, want still 1 (RunDaily must not run once capped)", runner.calls)
	}
}

func TestConsolidateNow_ExpiredSessionNeverCallsRunner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	runner := &stubRunner{}
	s := newTestStore(t, db, runner, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	if _, err := db.ExecContext(ctx,
		`update demo_sessions set expires_at = now() - interval '1 minute' where token_hash = $1`,
		hashToken(sess.Token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}

	if err := s.ConsolidateNow(ctx, sess.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("ConsolidateNow on expired session: err = %v, want ErrSessionInvalid", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner.calls = %d, want 0 (must not consolidate an expired/unknown session)", runner.calls)
	}
}

func TestCreateSession_DailyCapEnforced(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var existing int
	if err := db.QueryRowContext(ctx,
		`select count(*) from demo_sessions where created_at > now() - interval '1 day'`,
	).Scan(&existing); err != nil {
		t.Fatalf("count existing sessions: %v", err)
	}

	// Adapts to however many demo_sessions rows already exist in this
	// shared test database rather than assuming a clean table — the
	// daily cap counts *all* sessions system-wide, so a fixed small
	// limit would make this test order-dependent against other tests in
	// this same file.
	limits := testLimits()
	limits.MaxSessionsPerDay = existing + 1
	s := newTestStore(t, db, &stubRunner{}, limits)

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession (should still be under the cap): %v", err)
	}
	trackGuest(t, db, sess.GuestUserID)

	if _, err := s.CreateSession(ctx); !errors.Is(err, ErrDailySessionCapReached) {
		t.Fatalf("CreateSession over the cap: err = %v, want ErrDailySessionCapReached", err)
	}
}

func TestSweep_DeletesExpiredGuestAndItsData(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// No trackGuest here on purpose: Sweep itself is expected to remove
	// everything, and the assertions below confirm that directly.

	// episodes has row-level security (schema/0005) and the test DB
	// connects as hupi_app (a non-owner role, per this repo's CI/test
	// convention), so this insert must run inside a dbscope.Run
	// transaction carrying the guest's own scope, same as any other
	// write against this table.
	guestScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: sess.GuestUserID}
	err = dbscope.Run(ctx, db, guestScope, guestScope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`insert into episodes (id, ts, type, hash, scope_kind, scope_owner) values ($1, now(), 'interaction', $2, 'private', $3)`,
			"demo-sweep-test-episode", "demo-sweep-test-hash", sess.GuestUserID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`update demo_sessions set expires_at = now() - interval '1 minute' where token_hash = $1`,
		hashToken(sess.Token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}

	swept, err := s.Sweep(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if swept < 1 {
		t.Fatalf("Sweep swept %d sessions, want at least 1", swept)
	}

	var userExists, episodeExists, sessionExists bool
	db.QueryRowContext(ctx, `select exists(select 1 from users where id = $1)`, sess.GuestUserID).Scan(&userExists)
	db.QueryRowContext(ctx, `select exists(select 1 from episodes where scope_owner = $1)`, sess.GuestUserID).Scan(&episodeExists)
	db.QueryRowContext(ctx, `select exists(select 1 from demo_sessions where token_hash = $1)`, hashToken(sess.Token)).Scan(&sessionExists)

	if userExists || episodeExists || sessionExists {
		t.Fatalf("Sweep left data behind: user=%v episode=%v session=%v", userExists, episodeExists, sessionExists)
	}
}
