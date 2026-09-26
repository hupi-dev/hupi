package provider

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestOpenAICompat_ChatCompletion_RetriesOn429 reproduces the real
// failure found running cmd/hupi-bench against a real OpenAI account at
// scale (30,000 TPM limit tripped mid-run): a single 429 used to fail
// the request outright with no retry at all. The fake server here
// returns 429 (with a tiny Retry-After so the test stays fast) for the
// first two requests, then succeeds on the third — ChatCompletion must
// transparently retry and return the successful result, not the 429.
func TestOpenAICompat_ChatCompletion_RetriesOn429(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"gpt-4.1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "test", Vendor: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "test-key",
	})
	resp, err := p.ChatCompletion(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("ChatCompletion: %v (should have retried past the 429s)", err)
	}
	if resp.Message.Content != "ok" {
		t.Errorf("Message.Content = %q, want %q", resp.Message.Content, "ok")
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("server saw %d attempts, want 3 (2 failed + 1 success)", got)
	}
}

// TestOpenAICompat_ChatCompletion_DoesNotRetryOn400 confirms a genuine
// client error (bad request) fails immediately, not after burning through
// every retry attempt — retrying an error that will fail identically
// every time just wastes time (and, against a real paid API, money) with
// zero chance of success.
func TestOpenAICompat_ChatCompletion_DoesNotRetryOn400(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "test", Vendor: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "test-key",
	})
	_, err := p.ChatCompletion(t.Context(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("server saw %d attempts, want 1 (a 400 must not be retried)", got)
	}
}

// TestRetryDelay_HonorsRetryAfterHeader confirms a real Retry-After value
// from the provider is used verbatim rather than the exponential
// fallback — this is what lets the test above stay fast (0.01s) instead
// of waiting out the real 500ms+ default backoff schedule.
func TestRetryDelay_HonorsRetryAfterHeader(t *testing.T) {
	d := retryDelay(1, "0.01")
	if d.Seconds() < 0.005 || d.Seconds() > 0.05 {
		t.Errorf("retryDelay(1, %q) = %v, want ~10ms", "0.01", d)
	}
}

// TestRetryDelay_FallsBackToExponentialWithoutHeader confirms the
// backoff still grows across attempts when no Retry-After is present
// (a 5xx, or a vendor that never sends one).
func TestRetryDelay_FallsBackToExponentialWithoutHeader(t *testing.T) {
	d1 := retryDelay(1, "")
	d3 := retryDelay(3, "")
	if d3 <= d1 {
		t.Errorf("retryDelay(3, \"\") = %v should be greater than retryDelay(1, \"\") = %v", d3, d1)
	}
}
