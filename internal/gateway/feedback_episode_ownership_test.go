package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleFeedback_ReturnsBadRequestWhenEpisodeNotInScope is the
// handler-level half of the real regression test for review finding
// B17: the feedback endpoint built and persisted a feedback row pointing
// at any client-supplied episode_id with no verification it belonged to
// the caller's own scope at all. Capturer.Capture now returns
// ErrFeedbackEpisodeNotFound for exactly this case (internal/store's own
// real, Postgres-backed test covers the actual ownership check) — this
// confirms the handler maps that specific sentinel to 400, not the
// generic 500 every other Capture failure gets, since a bad episode_id
// is the client's mistake, not an infrastructure failure.
func TestHandleFeedback_ReturnsBadRequestWhenEpisodeNotInScope(t *testing.T) {
	h := &Handler{Capturer: &fakeCapturer{err: ErrFeedbackEpisodeNotFound}}

	body := strings.NewReader(`{"episode_id":"ep_not_mine","rating":"memory_wrong"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	w := httptest.NewRecorder()
	h.HandleFeedback(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, body = %q, want %d", w.Code, w.Body.String(), http.StatusBadRequest)
	}
}

// TestHandleFeedback_ReturnsServerErrorForOtherCaptureFailures confirms
// the new error-type check doesn't swallow a genuine infrastructure
// failure into a (wrong) 400 — only ErrFeedbackEpisodeNotFound
// specifically gets the client-error treatment.
func TestHandleFeedback_ReturnsServerErrorForOtherCaptureFailures(t *testing.T) {
	h := &Handler{Capturer: &fakeCapturer{err: errors.New("simulated database outage")}}

	body := strings.NewReader(`{"episode_id":"ep_whatever","rating":"memory_wrong"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	w := httptest.NewRecorder()
	h.HandleFeedback(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, body = %q, want %d", w.Code, w.Body.String(), http.StatusInternalServerError)
	}
}
