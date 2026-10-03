package demo

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync"
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

// TestSweep_DeletesGuestDataButKeepsSessionRowForCapWindow is a real
// regression test (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A11): this
// used to assert the *opposite* — that Sweep removed the demo_sessions
// row too, which was exactly the bug. Deleting the guest's users row now
// leaves the session row behind (schema/0016's "on delete set null"),
// specifically so it keeps counting toward CreateSession's daily cap
// until the row is old enough for phase 2 to remove it (see
// TestSweep_RemovesOldSessionRowsEvenWithoutAGuest below) — real,
// sensitive data (the user, their episode) is still gone promptly.
func TestSweep_DeletesGuestDataButKeepsSessionRowForCapWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// Not trackGuest: the whole point under test is that Sweep's phase 1
	// does NOT remove the demo_sessions row, so this test cleans that row
	// up itself rather than relying on either path.
	t.Cleanup(func() { db.Exec(`delete from demo_sessions where token_hash = $1`, hashToken(sess.Token)) })

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
		t.Fatalf("Sweep swept %d guests' data, want at least 1", swept)
	}

	var userExists, episodeExists bool
	var sessionGuestID sql.NullString
	db.QueryRowContext(ctx, `select exists(select 1 from users where id = $1)`, sess.GuestUserID).Scan(&userExists)
	db.QueryRowContext(ctx, `select exists(select 1 from episodes where scope_owner = $1)`, sess.GuestUserID).Scan(&episodeExists)
	err = db.QueryRowContext(ctx, `select guest_user_id from demo_sessions where token_hash = $1`, hashToken(sess.Token)).Scan(&sessionGuestID)
	if err != nil {
		t.Fatalf("expected the demo_sessions row to still exist after Sweep (it must survive until capWindow, not just the TTL): %v", err)
	}

	if userExists || episodeExists {
		t.Fatalf("Sweep left real guest data behind: user=%v episode=%v", userExists, episodeExists)
	}
	if sessionGuestID.Valid {
		t.Errorf("demo_sessions.guest_user_id = %q, want null — the FK's \"on delete set null\" should have cleared it when the users row was deleted", sessionGuestID.String)
	}
}

// TestSweep_DeletesGuestWithEntityRelationships is the regression test
// for a real production failure (2026-10-03):
// entity_relationships.source_summary_id/subject_id/object_id reference
// summaries/entities with no ON DELETE action (schema/0015, added after
// deleteGuest was first written), so a guest who had triggered the
// entity-relationship extraction path couldn't be swept at all — the
// delete from summaries/entities failed with a foreign key violation.
// deleteGuest must delete entity_relationships first.
func TestSweep_DeletesGuestWithEntityRelationships(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { db.Exec(`delete from demo_sessions where token_hash = $1`, hashToken(sess.Token)) })

	guestScope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: sess.GuestUserID}
	err = dbscope.Run(ctx, db, guestScope, guestScope, func(tx *sql.Tx) error {
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, 'person', 'Test Person', 'private', $2)`,
				[]any{"person:demo-sweep-test", sess.GuestUserID}},
			{`insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, 'project', 'Test Project', 'private', $2)`,
				[]any{"project:demo-sweep-test", sess.GuestUserID}},
			{`insert into summaries (id, period, level, status, grounding_checked, scope_kind, scope_owner) values ($1, '2026-10-02', 'daily', 'draft', true, 'private', $2)`,
				[]any{"sum-demo-sweep-test", sess.GuestUserID}},
			{`insert into entity_relationships (id, scope_kind, scope_owner, subject_id, predicate, object_id, source_summary_id) values ($1, 'private', $2, $3, 'works_on', $4, $5)`,
				[]any{"rel-demo-sweep-test", sess.GuestUserID, "person:demo-sweep-test", "project:demo-sweep-test", "sum-demo-sweep-test"}},
		} {
			if _, err := tx.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entity/summary/relationship: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		`update demo_sessions set expires_at = now() - interval '1 minute' where token_hash = $1`,
		hashToken(sess.Token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}

	if _, err := s.Sweep(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Sweep: %v (should have deleted entity_relationships before summaries/entities)", err)
	}

	var relExists, summaryExists, entityExists bool
	db.QueryRowContext(ctx, `select exists(select 1 from entity_relationships where scope_owner = $1)`, sess.GuestUserID).Scan(&relExists)
	db.QueryRowContext(ctx, `select exists(select 1 from summaries where scope_owner = $1)`, sess.GuestUserID).Scan(&summaryExists)
	db.QueryRowContext(ctx, `select exists(select 1 from entities where scope_owner = $1)`, sess.GuestUserID).Scan(&entityExists)
	if relExists || summaryExists || entityExists {
		t.Errorf("Sweep left data behind: entity_relationships=%v summaries=%v entities=%v", relExists, summaryExists, entityExists)
	}
}

// TestSweep_RemovesOldSessionRowsEvenWithoutAGuest is phase 2's own
// regression test: a demo_sessions row old enough to no longer matter
// for CreateSession's daily cap must eventually be deleted too, even
// though phase 1 never touches rows whose guest_user_id is already null.
func TestSweep_RemovesOldSessionRowsEvenWithoutAGuest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := newTestStore(t, db, &stubRunner{}, testLimits())

	tokenHash := hashToken("hupi_demo_test-phase-2-row")
	if _, err := db.ExecContext(ctx, `
		insert into demo_sessions (token_hash, guest_user_id, created_at, expires_at)
		values ($1, null, now() - interval '25 hours', now() - interval '22 hours')
	`, tokenHash); err != nil {
		t.Fatalf("seed an old, guest-less demo_sessions row: %v", err)
	}
	t.Cleanup(func() { db.Exec(`delete from demo_sessions where token_hash = $1`, tokenHash) })

	if _, err := s.Sweep(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	var exists bool
	db.QueryRowContext(ctx, `select exists(select 1 from demo_sessions where token_hash = $1)`, tokenHash).Scan(&exists)
	if exists {
		t.Error("expected Sweep's phase 2 to have deleted a demo_sessions row older than capWindow, even with no guest attached to it")
	}
}

// TestCreateSession_CapStillCountsASweptSession is the real, end-to-end
// proof of the fix: a session whose guest data Sweep already cleaned up
// (phase 1) must still count toward CreateSession's daily cap until
// capWindow has actually passed — this is the exact mechanism finding
// A11 reports as broken before the fix (the real achievable session
// volume running to roughly capWindow/SessionTTL times the configured
// limit).
func TestCreateSession_CapStillCountsASweptSession(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var existing int
	if err := db.QueryRowContext(ctx, `select count(*) from demo_sessions where created_at > now() - interval '1 day'`).Scan(&existing); err != nil {
		t.Fatalf("count existing sessions: %v", err)
	}

	limits := testLimits()
	limits.MaxSessionsPerDay = existing + 1
	s := newTestStore(t, db, &stubRunner{}, limits)

	sess, err := s.CreateSession(ctx)
	if err != nil {
		t.Fatalf("CreateSession (should still be under the cap): %v", err)
	}
	t.Cleanup(func() { db.Exec(`delete from demo_sessions where token_hash = $1`, hashToken(sess.Token)) })

	if _, err := db.ExecContext(ctx,
		`update demo_sessions set expires_at = now() - interval '1 minute' where token_hash = $1`,
		hashToken(sess.Token),
	); err != nil {
		t.Fatalf("force-expire session: %v", err)
	}
	if _, err := s.Sweep(ctx, 24*time.Hour); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// The whole point: even though Sweep already ran and the session is
	// long expired and its guest data gone, a new session must still be
	// refused — the swept session's row is still within capWindow (it was
	// created seconds ago), so it must still count.
	if _, err := s.CreateSession(ctx); !errors.Is(err, ErrDailySessionCapReached) {
		t.Errorf("CreateSession() after sweeping an expired-but-recent session = %v, want %v — a swept session must still count toward the cap until capWindow has passed", err, ErrDailySessionCapReached)
	}
}

// TestCreateSession_ConcurrentCallsNeverOvershootTheDailyCap is the real
// regression test for review finding B21 — a separate race from A11
// above: even with A11's counting fixed, CreateSession's own
// count-check-then-insert was two independent statements with nothing
// tying them together, so concurrent callers arriving while the count
// was one under the cap could all read the same pre-increment count and
// all succeed, overshooting the configured cap.
func TestCreateSession_ConcurrentCallsNeverOvershootTheDailyCap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	var existing int
	if err := db.QueryRowContext(ctx,
		`select count(*) from demo_sessions where created_at > now() - interval '1 day'`,
	).Scan(&existing); err != nil {
		t.Fatalf("count existing sessions: %v", err)
	}

	const allowMore = 5
	const concurrency = 20
	limits := testLimits()
	limits.MaxSessionsPerDay = existing + allowMore
	s := newTestStore(t, db, &stubRunner{}, limits)

	var wg sync.WaitGroup
	results := make(chan struct {
		guestID string
		err     error
	}, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := s.CreateSession(ctx)
			results <- struct {
				guestID string
				err     error
			}{sess.GuestUserID, err}
		}()
	}
	wg.Wait()
	close(results)

	succeeded, overCap := 0, 0
	for r := range results {
		switch {
		case r.err == nil:
			succeeded++
			trackGuest(t, db, r.guestID)
		case errors.Is(r.err, ErrDailySessionCapReached):
			overCap++
		default:
			t.Fatalf("CreateSession: unexpected error: %v", r.err)
		}
	}
	if succeeded != allowMore {
		t.Errorf("succeeded = %d, want exactly %d — the daily cap must hold exactly under concurrency, not overshoot it", succeeded, allowMore)
	}
	if overCap != concurrency-allowMore {
		t.Errorf("rejected-for-cap count = %d, want %d", overCap, concurrency-allowMore)
	}
}
