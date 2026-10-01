package provider

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// startDroppingServer is a real regression-test fixture for review
// finding B4: a raw TCP listener (not an http.HandlerFunc — that would
// only run after the request is already read, same as this does, but
// via an http.Server that masks exactly what we need to control here)
// that fully reads one HTTP request per connection, then closes the
// connection without writing any response at all. That reproduces the
// real-world case the finding describes: the provider received the
// full request — and, for a billed call, may already be generating and
// charging for a response — before the connection dropped. Every
// connection accepted increments the counter so tests can assert
// exactly how many times the request was actually, fully delivered.
func startDroppingServer(t *testing.T, connections *int32) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(connections, 1)
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err == nil {
					io.Copy(io.Discard, req.Body)
				}
				// Deliberately no response written — the connection is
				// simply dropped once the full request has been read.
			}()
		}
	}()
	return ln.Addr().String()
}

// TestSendWithRetry_NonIdempotentDoesNotRetryAfterRequestFullySent is the
// core regression test for review finding B4: a network-level error
// (here, the connection dropping with no response) that surfaces *after*
// the request was fully sent used to be retried unconditionally, for
// every call site — including a billed, non-deterministic chat
// completion, which has no idempotency key to let a retry be recognized
// and deduplicated by the provider. A retry in that situation risks a
// real duplicate charge and a second, different generated answer.
// idempotent=false must now fail immediately instead of retrying.
func TestSendWithRetry_NonIdempotentDoesNotRetryAfterRequestFullySent(t *testing.T) {
	var connections int32
	addr := startDroppingServer(t, &connections)

	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, "http://"+addr+"/v1/chat/completions", strings.NewReader(`{}`))
	}

	_, _, err := sendWithRetry(context.Background(), http.DefaultClient, "test", false, buildReq)
	if err == nil {
		t.Fatal("sendWithRetry: expected an error (connection dropped after the request was fully sent), got nil")
	}
	if got := atomic.LoadInt32(&connections); got != 1 {
		t.Errorf("dropping server saw %d connections, want exactly 1 (no retry once a non-idempotent request was fully sent)", got)
	}
}

// TestSendWithRetry_IdempotentStillRetriesAfterRequestFullySent confirms
// the fix is scoped to idempotent=false only: a safely-retryable call
// (e.g. Embed) must keep retrying through the exact same kind of
// network failure exactly as before this finding was fixed.
func TestSendWithRetry_IdempotentStillRetriesAfterRequestFullySent(t *testing.T) {
	var connections int32
	droppingAddr := startDroppingServer(t, &connections)

	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer okServer.Close()

	var attempt int32
	buildReq := func() (*http.Request, error) {
		target := okServer.URL
		if atomic.AddInt32(&attempt, 1) == 1 {
			target = "http://" + droppingAddr
		}
		return http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
	}

	status, body, err := sendWithRetry(context.Background(), http.DefaultClient, "test", true, buildReq)
	if err != nil {
		t.Fatalf("sendWithRetry: %v (an idempotent call should have retried past the dropped connection)", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Errorf("status=%d body=%q, want 200/\"ok\"", status, string(body))
	}
	if got := atomic.LoadInt32(&connections); got != 1 {
		t.Errorf("dropping server saw %d connections, want 1", got)
	}
}

// TestSendWithRetry_NonIdempotentStillRetriesWhenRequestNeverFullySent
// confirms the fix is specifically about requests that were fully
// sent, not network errors in general: a connection that was never
// even established (nothing could have received the request) is always
// safe to retry, idempotent or not.
func TestSendWithRetry_NonIdempotentStillRetriesWhenRequestNeverFullySent(t *testing.T) {
	// Grab a real local address, then close it immediately — nothing is
	// listening, so client.Do fails at connect time, before any request
	// bytes are written.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	okServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer okServer.Close()

	var attempt int32
	buildReq := func() (*http.Request, error) {
		target := okServer.URL
		if atomic.AddInt32(&attempt, 1) == 1 {
			target = "http://" + deadAddr
		}
		return http.NewRequest(http.MethodPost, target, strings.NewReader(`{}`))
	}

	status, body, err := sendWithRetry(context.Background(), http.DefaultClient, "test", false, buildReq)
	if err != nil {
		t.Fatalf("sendWithRetry: %v (a connection that was never established is always safe to retry)", err)
	}
	if status != http.StatusOK || string(body) != "ok" {
		t.Errorf("status=%d body=%q, want 200/\"ok\"", status, string(body))
	}
}

// TestSendWithRetry_CancellationDuringBackoffIsWrappedWithProviderName and
// TestConnectWithRetry_CancellationDuringBackoffIsWrappedWithProviderName
// are the real regression tests for review finding C3: every other error
// path in sendWithRetry/connectWithRetry wraps the underlying error with
// "provider %s: ...", but the context-cancellation path inside the
// retry-backoff select returned bare ctx.Err() — the one error a caller
// logging just the error string would see with no indication of which
// provider it came from, unlike every other failure from the same
// function.
func TestSendWithRetry_CancellationDuringBackoffIsWrappedWithProviderName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A very long Retry-After so the backoff timer never fires first —
		// the test depends on the context cancelling before retryDelay
		// elapses, not a race between the two.
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	}
	_, _, err := sendWithRetry(ctx, http.DefaultClient, "my-provider", true, buildReq)
	if err == nil {
		t.Fatal("expected an error once the context was cancelled during backoff")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to still satisfy errors.Is(err, context.DeadlineExceeded)", err)
	}
	if !strings.Contains(err.Error(), "my-provider") {
		t.Errorf("err = %q, want it to name the provider (\"my-provider\"), like every other error path in sendWithRetry", err.Error())
	}
}

func TestConnectWithRetry_CancellationDuringBackoffIsWrappedWithProviderName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	buildReq := func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{}`))
	}
	_, err := connectWithRetry(ctx, http.DefaultClient, "my-provider", false, buildReq)
	if err == nil {
		t.Fatal("expected an error once the context was cancelled during backoff")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to still satisfy errors.Is(err, context.DeadlineExceeded)", err)
	}
	if !strings.Contains(err.Error(), "my-provider") {
		t.Errorf("err = %q, want it to name the provider (\"my-provider\"), like every other error path in connectWithRetry", err.Error())
	}
}
