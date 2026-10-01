package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
)

// countEpisode is a scoped existence check — a plain, unscoped
// s.db.QueryRowContext would silently return 0 regardless of the real
// answer (RLS fails closed with no session variables set, same gotcha
// this package's own tests have hit before), so every count assertion
// below must run inside a real dbscope.Run transaction for the scope
// actually being asserted about.
func countEpisode(t *testing.T, s *Store, scope identity.Scope, id string) int {
	t.Helper()
	var count int
	err := dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRow(`select count(*) from episodes where id = $1`, id).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count episode %s: %v", id, err)
	}
	return count
}

// TestCapture_RejectsFeedbackReferringToEpisodeInAnotherScope is the
// real, Postgres-backed regression test for review finding B17:
// episodes.refers_to was never checked at all — a feedback row could be
// stored pointing at any client-supplied episode id, including one
// belonging to a completely different scope, with no verification it
// was ever the caller's own. Seeds a real episode in scope B, then
// submits feedback from scope A referring to it — must be rejected with
// ErrFeedbackEpisodeNotFound, the same as a genuinely made-up id (the
// RLS policy already protecting episodes is what makes a real episode
// from a different scope indistinguishable from one that doesn't exist
// at all, from scope A's own transaction).
func TestCapture_RejectsFeedbackReferringToEpisodeInAnotherScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scopeA := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-feedback-ownership-a"}
	scopeB := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-feedback-ownership-b"}
	t.Cleanup(func() { cleanupScope(t, s, scopeA) })
	t.Cleanup(func() { cleanupScope(t, s, scopeB) })

	otherEpisodeID := "ep_test_feedback_ownership_other_scope"
	if err := s.Capture(ctx, scopeB, gateway.Episode{
		ID: otherEpisodeID, TS: time.Now(), Type: "interaction",
		InputText: "hi", OutputText: "hello", Hash: "sha256:test-ownership-other",
	}); err != nil {
		t.Fatalf("seed episode in scope B: %v", err)
	}

	err := s.Capture(ctx, scopeA, gateway.Episode{
		ID: "ep_test_feedback_ownership_feedback", TS: time.Now(), Type: "feedback",
		RefersTo: otherEpisodeID, Rating: gateway.RatingWrong,
	})
	if !errors.Is(err, gateway.ErrFeedbackEpisodeNotFound) {
		t.Errorf("Capture() error = %v, want errors.Is(err, gateway.ErrFeedbackEpisodeNotFound)", err)
	}

	if count := countEpisode(t, s, scopeA, "ep_test_feedback_ownership_feedback"); count != 0 {
		t.Errorf("feedback row was stored despite the rejected cross-scope reference, want 0 rows, got %d", count)
	}
}

// TestCapture_RejectsFeedbackReferringToNonExistentEpisode confirms the
// same rejection for an episode id that was never real at all, not just
// one that belongs to a different scope.
func TestCapture_RejectsFeedbackReferringToNonExistentEpisode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-feedback-ownership-nonexistent"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	err := s.Capture(ctx, scope, gateway.Episode{
		ID: "ep_test_feedback_nonexistent_feedback", TS: time.Now(), Type: "feedback",
		RefersTo: "ep_this_was_never_real", Rating: gateway.RatingMissing,
	})
	if !errors.Is(err, gateway.ErrFeedbackEpisodeNotFound) {
		t.Errorf("Capture() error = %v, want errors.Is(err, gateway.ErrFeedbackEpisodeNotFound)", err)
	}
}

// TestCapture_AcceptsFeedbackReferringToOwnEpisode confirms the fix
// doesn't break the legitimate case: feedback referring to a real
// episode in the caller's own scope must still succeed.
func TestCapture_AcceptsFeedbackReferringToOwnEpisode(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-feedback-ownership-own"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	ownEpisodeID := "ep_test_feedback_ownership_own_episode"
	if err := s.Capture(ctx, scope, gateway.Episode{
		ID: ownEpisodeID, TS: time.Now(), Type: "interaction",
		InputText: "hi", OutputText: "hello", Hash: "sha256:test-ownership-own",
	}); err != nil {
		t.Fatalf("seed own episode: %v", err)
	}

	feedbackID := "ep_test_feedback_ownership_own_feedback"
	if err := s.Capture(ctx, scope, gateway.Episode{
		ID: feedbackID, TS: time.Now(), Type: "feedback",
		RefersTo: ownEpisodeID, Rating: gateway.RatingCorrect,
	}); err != nil {
		t.Errorf("Capture() for feedback on the caller's own episode should succeed, got: %v", err)
	}

	if count := countEpisode(t, s, scope, feedbackID); count != 1 {
		t.Errorf("feedback row = %d, want exactly 1 (the legitimate case must still be stored)", count)
	}
}
