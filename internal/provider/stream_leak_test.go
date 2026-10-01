package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"
)

// TestOpenAICompat_StreamChatCompletion_StoppingDrainDoesNotLeakGoroutine is
// a real regression test (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A1):
// the stream-reading goroutine used to send every chunk — including the
// final error chunk produced when the request context is cancelled — with
// an unconditional `out <- chunk` on an unbuffered channel. A caller that
// stops draining `out` after a context cancellation (exactly what the
// gateway's SSE handler does on client disconnect — it `break`s out of its
// own read loop the moment ctx.Done() fires, never reading `out` again)
// left that goroutine blocked forever on its own send, and its deferred
// resp.Body.Close() never ran — a permanent goroutine + connection leak.
//
// The fake server here writes one real chunk, then stalls indefinitely
// without closing the connection, simulating a stream still "open" from
// the server's side when the client gives up. The test reads exactly one
// chunk, cancels the request context, and then — critically — never
// touches `out` again, the same as the real abandoned-caller scenario.
// Cancellation unblocks the stalled body Read via net/http's own
// context-aborts-the-transport behavior; the scanner then errors out and
// the goroutine attempts one final send. Before the fix, that send had no
// reader and blocked forever. The only external signal of "did the
// goroutine actually exit" without reading `out` is the process's own
// goroutine count returning to baseline — so that's what this test polls,
// bounded by an explicit deadline so a real regression fails the test
// instead of hanging it.
func TestOpenAICompat_StreamChatCompletion_StoppingDrainDoesNotLeakGoroutine(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`data: {"choices":[{"delta":{"content":"hello"}}]}` + "\n\n"))
		flusher.Flush()
		// Stall without closing the connection — only the client's own
		// cancellation (below) ends this, not a server-side timeout.
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	p := NewOpenAICompat(OpenAICompatConfig{
		Name: "test", Vendor: "openai", Model: "gpt-4.1", BaseURL: srv.URL, APIKey: "test-key",
	})

	baseline := countSettledGoroutines(t)

	ctx, cancel := context.WithCancel(context.Background())
	out, err := p.StreamChatCompletion(ctx, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	first := <-out
	if first.Delta != "hello" {
		t.Fatalf("first chunk = %+v, want Delta=%q", first, "hello")
	}

	cancel() // simulate the request context ending (client disconnect)
	// Deliberately never read from `out` again — the real abandoned-caller
	// scenario this test exists to catch.

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countSettledGoroutines(t) <= baseline {
			return // goroutine count back to baseline: the leak didn't happen
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream goroutine leaked: goroutine count stayed above baseline (%d) for 2s after context cancellation", baseline)
}

// countSettledGoroutines forces a GC (to clear anything merely pending
// finalization) and gives the runtime a moment to schedule, then returns
// NumGoroutine — a coarse but real signal for "did a goroutine we started
// actually exit," used here only to detect a goroutine parked forever on a
// channel send, which is exactly what this bug produces.
func countSettledGoroutines(t *testing.T) int {
	t.Helper()
	runtime.Gosched()
	runtime.GC()
	return runtime.NumGoroutine()
}

// TestAnthropic_StreamChatCompletion_StoppingDrainDoesNotLeakGoroutine is
// the same real regression as the OpenAICompat version above, against the
// second, independent adapter (different SSE event framing, same
// underlying unconditional-channel-send bug, same fix shape).
func TestAnthropic_StreamChatCompletion_StoppingDrainDoesNotLeakGoroutine(t *testing.T) {
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter does not support flushing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"hello\"}}\n\n"))
		flusher.Flush()
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	p := NewAnthropic(AnthropicConfig{
		Name: "test", Vendor: "anthropic", Model: "claude-sonnet-5", BaseURL: srv.URL, APIKey: "test-key", APIVersion: "2023-06-01",
	})

	baseline := countSettledGoroutines(t)

	ctx, cancel := context.WithCancel(context.Background())
	out, err := p.StreamChatCompletion(ctx, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	first := <-out
	if first.Delta != "hello" {
		t.Fatalf("first chunk = %+v, want Delta=%q", first, "hello")
	}

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if countSettledGoroutines(t) <= baseline {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream goroutine leaked: goroutine count stayed above baseline (%d) for 2s after context cancellation", baseline)
}
