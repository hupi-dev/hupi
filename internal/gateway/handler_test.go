package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hupi/internal/identity"
	"hupi/internal/provider"
)

// fakeRetriever records whether Retrieve was ever called — a ghost-text
// completion opting out via X-Hupi-Memory should skip this path entirely
// (already-existing behavior; asserted here as a baseline alongside the
// new capture opt-out).
type fakeRetriever struct {
	calls int
}

func (f *fakeRetriever) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message) (RetrievalResult, error) {
	f.calls++
	return RetrievalResult{Gate: GateSkipped}, nil
}

// fakeCapturer records every episode it's asked to capture — this is what
// the new X-Hupi-Capture opt-out must prevent from ever being called.
type fakeCapturer struct {
	episodes []Episode
}

func (f *fakeCapturer) Capture(ctx context.Context, scope identity.Scope, ep Episode) error {
	f.episodes = append(f.episodes, ep)
	return nil
}

func newTestHandler(t *testing.T, retriever Retriever, capturer Capturer) *Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "fake-model",
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": "hello from upstream"}},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	t.Cleanup(upstream.Close)

	reg, err := provider.NewRegistry(provider.Config{
		// Only Chat() is exercised by HandleChatCompletions, but
		// NewRegistry validates every role resolves to a defined
		// profile — reuse the one fake profile for all of them.
		ActiveChatProvider:          "test",
		ActiveConsolidationProvider: "test",
		ActiveEmbeddingProvider:     "test",
		Providers: map[string]provider.ProfileConfig{
			"test": {Kind: provider.KindOpenAICompat, Vendor: "test", BaseURL: upstream.URL, Model: "fake-model"},
		},
	})
	if err != nil {
		t.Fatalf("provider.NewRegistry: %v", err)
	}

	return &Handler{Registry: reg, Retriever: retriever, Capturer: capturer}
}

func postChatCompletion(t *testing.T, h *Handler, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, req)
	return w
}

func TestHandleChatCompletions_CapturesByDefault(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if retriever.calls != 1 {
		t.Errorf("retriever.calls = %d, want 1", retriever.calls)
	}
	if len(capturer.episodes) != 1 {
		t.Fatalf("len(capturer.episodes) = %d, want 1", len(capturer.episodes))
	}
	if capturer.episodes[0].OutputText != "hello from upstream" {
		t.Errorf("captured OutputText = %q", capturer.episodes[0].OutputText)
	}
}

// This is the actual bug fix under test: a ghost-text-style completion
// (VS Code's inlineCompletionProvider.ts) sends X-Hupi-Capture: off on
// every debounced request to avoid flooding memory with near-meaningless
// single-line completions — see vscode-extension/CHANGELOG.md for the
// incident this responds to. Retrieval is a separate, pre-existing
// opt-out (X-Hupi-Memory) — both are set together by that client, but
// each must work independently.
func TestHandleChatCompletions_CaptureOptOut(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Capture": "off"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if retriever.calls != 1 {
		t.Errorf("retriever.calls = %d, want 1 (X-Hupi-Capture must not affect retrieval)", retriever.calls)
	}
	if len(capturer.episodes) != 0 {
		t.Errorf("len(capturer.episodes) = %d, want 0 — capture should have been skipped", len(capturer.episodes))
	}
}

func TestHandleChatCompletions_MemoryOptOutSkipsRetrievalButStillCaptures(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Memory": "off"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if retriever.calls != 0 {
		t.Errorf("retriever.calls = %d, want 0", retriever.calls)
	}
	if len(capturer.episodes) != 1 {
		t.Errorf("len(capturer.episodes) = %d, want 1 — X-Hupi-Memory must not affect capture", len(capturer.episodes))
	}
}

func TestHandleChatCompletions_BothOptOutsTogether(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Memory": "off", "X-Hupi-Capture": "off"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if retriever.calls != 0 {
		t.Errorf("retriever.calls = %d, want 0", retriever.calls)
	}
	if len(capturer.episodes) != 0 {
		t.Errorf("len(capturer.episodes) = %d, want 0", len(capturer.episodes))
	}
}
