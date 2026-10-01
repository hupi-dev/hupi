package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// fakeRetriever records whether Retrieve was ever called — a ghost-text
// completion opting out via X-Hupi-Memory should skip this path entirely
// (already-existing behavior; asserted here as a baseline alongside the
// new capture opt-out).
type fakeRetriever struct {
	calls  int
	result RetrievalResult // zero value (GateSkipped) unless a test sets one
}

func (f *fakeRetriever) Retrieve(ctx context.Context, actingUser, workspace identity.Scope, messages []provider.Message, now time.Time) (RetrievalResult, error) {
	f.calls++
	if f.result.Gate == "" {
		return RetrievalResult{Gate: GateSkipped}, nil
	}
	return f.result, nil
}

// fakeCapturer records every episode it's asked to capture — this is what
// the new X-Hupi-Capture opt-out must prevent from ever being called.
type fakeCapturer struct {
	episodes []Episode
	err      error // returned by every Capture call when non-nil, instead of recording
}

func (f *fakeCapturer) Capture(ctx context.Context, scope identity.Scope, ep Episode) error {
	if f.err != nil {
		return f.err
	}
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

// TestHandleChatCompletions_OptOutHeadersAreCaseInsensitive is the real
// regression test for review finding C2: both opt-out headers used to be
// exact-match comparisons against the literal string "off" — a client
// sending "Off" or "OFF" (e.g. a proxy/library that normalizes header
// casing) silently got the opposite of what it asked for, with retrieval
// or capture running unexpectedly. Fails toward the safe default (memory
// stays on), which is why this was filed as minor rather than a real
// bug, but it's still a real behavior mismatch worth closing.
func TestHandleChatCompletions_OptOutHeadersAreCaseInsensitive(t *testing.T) {
	for _, variant := range []string{"Off", "OFF", "oFF"} {
		t.Run(variant, func(t *testing.T) {
			retriever := &fakeRetriever{}
			capturer := &fakeCapturer{}
			h := newTestHandler(t, retriever, capturer)

			w := postChatCompletion(t, h, map[string]string{"X-Hupi-Memory": variant, "X-Hupi-Capture": variant})

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if retriever.calls != 0 {
				t.Errorf("X-Hupi-Memory: %q: retriever.calls = %d, want 0", variant, retriever.calls)
			}
			if len(capturer.episodes) != 0 {
				t.Errorf("X-Hupi-Capture: %q: len(capturer.episodes) = %d, want 0", variant, len(capturer.episodes))
			}
		})
	}
}

// TestHandleChatCompletions_OnRetrieveSeesExactResult is the seam
// docs/EVALMEM_INTEGRATION_PLAN.md's retrieve_original/C_original
// requirement needs: external diagnostic tooling must see the *exact*
// RetrievalResult a request's own retrieval step computed, not a
// separately re-run Retrieve() call that risks drifting from what the
// request actually used.
func TestHandleChatCompletions_OnRetrieveSeesExactResult(t *testing.T) {
	want := RetrievalResult{
		Gate:           GateFull,
		ContextMessage: "the exact context this request actually used",
		Refs:           []identity.Ref{{Kind: identity.RefKindSummary, ID: "sum_test"}},
	}
	retriever := &fakeRetriever{result: want}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	var got *RetrievalResult
	h.OnRetrieve = func(r RetrievalResult) {
		got = &r
	}

	w := postChatCompletion(t, h, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if got == nil {
		t.Fatal("OnRetrieve was never called")
	}
	if got.ContextMessage != want.ContextMessage {
		t.Errorf("ContextMessage = %q, want %q", got.ContextMessage, want.ContextMessage)
	}
	if got.Gate != want.Gate {
		t.Errorf("Gate = %q, want %q", got.Gate, want.Gate)
	}
}

// TestHandleChatCompletions_OnRetrieveNilIsNoOp confirms leaving
// OnRetrieve unset (every real deployment today) changes nothing —
// the zero-value default a nil-checked seam must have.
func TestHandleChatCompletions_OnRetrieveNilIsNoOp(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)
	// h.OnRetrieve left nil deliberately.

	w := postChatCompletion(t, h, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
}

// TestHandleChatCompletions_ExplainHeaderIncludesCitations confirms
// X-Hupi-Explain: on surfaces RetrievalResult.Citations in the response
// (docs/ANSWER_CITATIONS_PLAN.md) — opt-in, not the default shape.
func TestHandleChatCompletions_ExplainHeaderIncludesCitations(t *testing.T) {
	want := RetrievalResult{
		Gate:           GateFull,
		ContextMessage: "the exact context this request actually used",
		Refs:           []identity.Ref{{Kind: identity.RefKindSummary, ID: "sum_test"}},
		Citations:      []Citation{{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}},
	}
	retriever := &fakeRetriever{result: want}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Explain": "on"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Citations) != 1 {
		t.Fatalf("Citations = %v, want 1 entry", resp.Citations)
	}
	if resp.Citations[0].Snippet != "the source text" {
		t.Errorf("Citations[0].Snippet = %q, want %q", resp.Citations[0].Snippet, "the source text")
	}
	if resp.Citations[0].Ref.ID != "sum_test" {
		t.Errorf("Citations[0].Ref.ID = %q, want %q", resp.Citations[0].Ref.ID, "sum_test")
	}
}

// TestHandleChatCompletions_NoExplainHeaderOmitsCitations confirms the
// default (no X-Hupi-Explain header) response shape is unchanged even
// when the retriever did compute citations — omitted, not just empty,
// via json:"...,omitempty" so existing/strict clients see nothing new.
func TestHandleChatCompletions_NoExplainHeaderOmitsCitations(t *testing.T) {
	want := RetrievalResult{
		Gate:      GateFull,
		Citations: []Citation{{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}},
	}
	retriever := &fakeRetriever{result: want}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	w := postChatCompletion(t, h, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "hupi_citations") {
		t.Errorf("response included hupi_citations without X-Hupi-Explain: %s", w.Body.String())
	}
}

// TestHandleChatCompletions_DeepExplainRunsAttributionCheck confirms
// X-Hupi-Explain: deep makes a real second LLM call (attributionCheck)
// and populates Citation.Used from its verdict — Phase 2 of
// docs/ANSWER_CITATIONS_PLAN.md, distinct from plain "on" (Phase 1,
// asserted not to make this extra call, since it costs a real request).
func TestHandleChatCompletions_DeepExplainRunsAttributionCheck(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		isAttribution := false
		for _, m := range msgs {
			msg, _ := m.(map[string]any)
			if content, _ := msg["content"].(string); strings.Contains(content, "Candidate memory snippets") {
				isAttribution = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		content := "hello from upstream"
		if isAttribution {
			content = `{"used": [true]}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-model",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()

	reg, err := provider.NewRegistry(provider.Config{
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

	citation := Citation{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}
	retriever := &fakeRetriever{result: RetrievalResult{Gate: GateFull, Citations: []Citation{citation}}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Explain": "deep"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 (answer + attribution)", upstreamCalls)
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Citations) != 1 || resp.Citations[0].Used == nil || !*resp.Citations[0].Used {
		t.Fatalf("Citations = %+v, want one citation with Used = true", resp.Citations)
	}
}

// TestHandleChatCompletions_DeepExplainLeavesUsedNilOnMalformedJudgeResponse
// is a real regression test (docs/CODEBASE_SURVEY_AND_REVIEW.md finding
// A2), end to end through the real HTTP response: a malformed attribution
// judge response used to populate every Citation.Used with false (via
// attributionCheck's old silent-default behavior) rather than leaving it
// nil/omitted — indistinguishable, to a real API caller, from a genuine
// "checked and confirmed unused" verdict.
func TestHandleChatCompletions_DeepExplainLeavesUsedNilOnMalformedJudgeResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		isAttribution := false
		for _, m := range msgs {
			msg, _ := m.(map[string]any)
			if content, _ := msg["content"].(string); strings.Contains(content, "Candidate memory snippets") {
				isAttribution = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		content := "hello from upstream"
		if isAttribution {
			content = "I'm not sure how to answer that." // malformed: not the requested JSON shape
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-model",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()

	reg, err := provider.NewRegistry(provider.Config{
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

	citation := Citation{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}
	retriever := &fakeRetriever{result: RetrievalResult{Gate: GateFull, Citations: []Citation{citation}}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Explain": "deep"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Citations) != 1 || resp.Citations[0].Used != nil {
		t.Fatalf("Citations = %+v, want one citation with Used = nil (not checked), not false (checked and confirmed unused)", resp.Citations)
	}
}

// TestHandleChatCompletions_PlainExplainDoesNotRunAttributionCheck
// confirms "on" (Phase 1 only) never triggers the extra LLM call "deep"
// does — the cost-control distinction docs/ANSWER_CITATIONS_PLAN.md's
// two-level design exists for.
func TestHandleChatCompletions_PlainExplainDoesNotRunAttributionCheck(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-model",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "hello from upstream"}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()

	reg, err := provider.NewRegistry(provider.Config{
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

	citation := Citation{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}
	retriever := &fakeRetriever{result: RetrievalResult{Gate: GateFull, Citations: []Citation{citation}}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	w := postChatCompletion(t, h, map[string]string{"X-Hupi-Explain": "on"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (answer only, no attribution call)", upstreamCalls)
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Citations) != 1 || resp.Citations[0].Used != nil {
		t.Fatalf("Citations = %+v, want one citation with Used == nil (not checked)", resp.Citations)
	}
}

// TestHandleChatCompletions_StreamDeepExplainIncludesCitationsOnTerminalChunk
// confirms streaming responses (chatParticipant.ts/chatViewProvider.ts's
// actual code path, not the non-streamed one — docs/ANSWER_CITATIONS_PLAN.md)
// carry citations too: attached only to the terminal (finish_reason)
// chunk, with Used populated when X-Hupi-Explain: deep ran attribution
// against the fully-assembled streamed answer.
func TestHandleChatCompletions_StreamDeepExplainIncludesCitationsOnTerminalChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		bodyBytes, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(bodyBytes, &body)

		if streaming, _ := body["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hello "}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"world"}}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}

		// Non-streaming: this is the attribution call.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "fake-model",
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": `{"used": [true]}`}}},
			"usage":   map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer upstream.Close()

	reg, err := provider.NewRegistry(provider.Config{
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

	citation := Citation{Ref: identity.Ref{Kind: identity.RefKindSummary, ID: "sum_test"}, Snippet: "the source text"}
	retriever := &fakeRetriever{result: RetrievalResult{Gate: GateFull, Citations: []Citation{citation}}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	body := strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("X-Hupi-Explain", "deep")
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// Parse the SSE body: find the terminal chunk (has finish_reason set)
	// and confirm it carries the citation with Used populated.
	var sawTerminalCitation bool
	for _, line := range strings.Split(w.Body.String(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("decode SSE chunk %q: %v", line, err)
		}
		if len(chunk.Citations) > 0 {
			sawTerminalCitation = true
			if chunk.Citations[0].Used == nil || !*chunk.Citations[0].Used {
				t.Errorf("terminal chunk Citations[0].Used = %v, want true", chunk.Citations[0].Used)
			}
		}
	}
	if !sawTerminalCitation {
		t.Fatalf("no SSE chunk carried citations; full body:\n%s", w.Body.String())
	}
}

// TestHandleChatCompletions_StreamMidStreamErrorIncrementsProviderCallErrorsTotal
// is a real regression test for review finding B1
// (docs/CODEBASE_SURVEY_AND_REVIEW.md): handleStream's drain loop only
// logged a chunk.Err arriving mid-stream (e.g. the upstream connection
// dropping or sending a malformed event partway through) — unlike the
// initial-connect failure path a few lines above it, which does
// increment metrics.ProviderCallErrorsTotal. A failure that happens to
// land after streaming has already started was invisible to the same
// alert/dashboard the initial-connect path feeds.
func TestHandleChatCompletions_StreamMidStreamErrorIncrementsProviderCallErrorsTotal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"hello"}}]}`+"\n\n")
		// A malformed mid-stream event — internal/provider/openai_compat.go's
		// StreamChatCompletion sends StreamChunk{Err: ...} and stops when a
		// "data:" line fails to decode, simulating a real provider dropping
		// the connection or sending garbage partway through.
		fmt.Fprint(w, "data: {not valid json\n\n")
	}))
	defer upstream.Close()

	reg, err := provider.NewRegistry(provider.Config{
		ActiveChatProvider:          "stream-error-test",
		ActiveConsolidationProvider: "stream-error-test",
		ActiveEmbeddingProvider:     "stream-error-test",
		Providers: map[string]provider.ProfileConfig{
			"stream-error-test": {Kind: provider.KindOpenAICompat, Vendor: "stream-error-test-vendor", BaseURL: upstream.URL, Model: "fake-model"},
		},
	})
	if err != nil {
		t.Fatalf("provider.NewRegistry: %v", err)
	}

	h := &Handler{Registry: reg, Retriever: &fakeRetriever{}, Capturer: &fakeCapturer{}}

	body := strings.NewReader(`{"model":"stream-error-test","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, req)

	got := testutil.ToFloat64(metrics.ProviderCallErrorsTotal.WithLabelValues("stream-error-test", "stream-error-test-vendor"))
	if got != 1 {
		t.Errorf("hupi_provider_call_errors_total{provider=%q,vendor=%q} = %v, want 1", "stream-error-test", "stream-error-test-vendor", got)
	}
}

// TestHandleChatCompletions_RejectsInvalidMessageRole is the real
// regression test for review finding C1: a message's role used to be
// forwarded to the vendor API completely unvalidated — an invalid role
// would surface as an opaque upstream error instead of a clear 400 at
// HUPI's own gateway.
func TestHandleChatCompletions_RejectsInvalidMessageRole(t *testing.T) {
	retriever := &fakeRetriever{}
	capturer := &fakeCapturer{}
	h := newTestHandler(t, retriever, capturer)

	body := strings.NewReader(`{"model":"test","messages":[{"role":"nonsense","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	w := httptest.NewRecorder()
	h.HandleChatCompletions(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	respBody, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(respBody), `invalid message role "nonsense"`) {
		t.Errorf("response body = %q, want it to name the invalid role", string(respBody))
	}
	if retriever.calls != 0 {
		t.Error("an invalid role must be rejected before retrieval ever runs")
	}
}

// TestHandleChatCompletions_AcceptsEveryValidRole confirms the new check
// isn't overly strict — system/user/assistant must all still work,
// including system, which no existing test in this file exercises.
func TestHandleChatCompletions_AcceptsEveryValidRole(t *testing.T) {
	for _, role := range []string{"system", "user", "assistant"} {
		t.Run(role, func(t *testing.T) {
			h := newTestHandler(t, &fakeRetriever{}, &fakeCapturer{})
			body := strings.NewReader(fmt.Sprintf(`{"model":"test","messages":[{"role":%q,"content":"hi"}]}`, role))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
			w := httptest.NewRecorder()
			h.HandleChatCompletions(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("role %q: status = %d, body = %q, want 200", role, w.Code, w.Body.String())
			}
		})
	}
}
