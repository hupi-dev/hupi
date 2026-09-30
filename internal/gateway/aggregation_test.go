package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hupi/internal/provider"
)

// erroringJudge always fails — fakeJudge (attribution_test.go) has no way
// to simulate a call failure, only a malformed response.
type erroringJudge struct{}

func (erroringJudge) Name() string   { return "erroring-judge" }
func (erroringJudge) Vendor() string { return "test" }
func (erroringJudge) Model() string  { return "erroring-judge-model" }
func (erroringJudge) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("simulated provider error")
}
func (erroringJudge) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	panic("not used")
}
func (erroringJudge) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	panic("not used")
}

func TestExtractAggregationFacts_ParsesFacts(t *testing.T) {
	judge := &fakeJudge{content: `{"facts": [{"description": "Went to a charity gala", "date": "2023-02-14"}, {"description": "Attended a charity auction", "date": "2023-02-15"}]}`}
	got := extractAggregationFacts(context.Background(), judge, "how many months since two charity events in a row?", "some context")
	if len(got) != 2 {
		t.Fatalf("extractAggregationFacts() returned %d facts, want 2", len(got))
	}
	if got[0].Date != "2023-02-14" || got[1].Date != "2023-02-15" {
		t.Errorf("extractAggregationFacts() = %+v, want dates 2023-02-14 and 2023-02-15", got)
	}
}

func TestExtractAggregationFacts_ToleratesMarkdownFence(t *testing.T) {
	judge := &fakeJudge{content: "```json\n{\"facts\": [{\"description\": \"x\", \"date\": \"2023-01-01\"}]}\n```"}
	got := extractAggregationFacts(context.Background(), judge, "q", "c")
	if len(got) != 1 {
		t.Fatalf("extractAggregationFacts() returned %d facts, want 1", len(got))
	}
}

func TestExtractAggregationFacts_CallFailureReturnsNil(t *testing.T) {
	if got := extractAggregationFacts(context.Background(), erroringJudge{}, "q", "c"); got != nil {
		t.Errorf("extractAggregationFacts() = %v, want nil on call failure", got)
	}
}

func TestExtractAggregationFacts_MalformedResponseReturnsNil(t *testing.T) {
	judge := &fakeJudge{content: "I'm not sure."}
	if got := extractAggregationFacts(context.Background(), judge, "q", "c"); got != nil {
		t.Errorf("extractAggregationFacts() = %v, want nil on malformed response", got)
	}
}

func TestParseAggregationFacts_SkipsUnparseableDatesAndEmptyDescriptions(t *testing.T) {
	facts := []aggregationFact{
		{Description: "good fact", Date: "2023-02-14"},
		{Description: "bad date", Date: "not-a-date"},
		{Description: "", Date: "2023-02-15"},
	}
	got := parseAggregationFacts(facts)
	if len(got) != 1 || got[0].description != "good fact" {
		t.Errorf("parseAggregationFacts() = %+v, want only the one good fact", got)
	}
}

func TestResolveAggregationHint_FewerThanTwoFactsReturnsEmpty(t *testing.T) {
	now := time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)
	if got := resolveAggregationHint("how many months since two events in a row?", nil, now); got != "" {
		t.Errorf("resolveAggregationHint(0 facts) = %q, want empty", got)
	}
	one := []parsedAggregationFact{{description: "x", date: now}}
	if got := resolveAggregationHint("how many months since two events in a row?", one, now); got != "" {
		t.Errorf("resolveAggregationHint(1 fact) = %q, want empty", got)
	}
}

// TestResolveConsecutivePairHint_FindsClosestPairAndComputesGap is the
// real, motivating case (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): the
// charity-events question needs the closest adjacent pair identified
// among several candidates, then the gap from that pair's later date to
// now — not the model doing both at once over raw prose.
func TestResolveConsecutivePairHint_FindsClosestPairAndComputesGap(t *testing.T) {
	facts := []parsedAggregationFact{
		{description: "Volunteered at a food bank", date: time.Date(2022, 11, 1, 0, 0, 0, 0, time.UTC)}, // distractor, far from the real pair
		{description: "Attended a charity gala", date: time.Date(2023, 2, 14, 0, 0, 0, 0, time.UTC)},
		{description: "Attended a charity auction", date: time.Date(2023, 2, 15, 0, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2023, 5, 15, 0, 0, 0, 0, time.UTC) // 3 months after Feb 15
	got := resolveAggregationHint("how many months have passed since I participated in two charity events in a row?", facts, now)
	if got == "" {
		t.Fatal("resolveAggregationHint() = empty, want a computed hint")
	}
	for _, want := range []string{"charity gala", "2023-02-14", "charity auction", "2023-02-15", "1 day(s) apart", "3 months ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("resolveAggregationHint() = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "food bank") {
		t.Errorf("resolveAggregationHint() = %q, should not mention the distractor fact", got)
	}
}

// TestResolveConsecutivePairHint_RejectsPairTooFarApart is a real
// regression test (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): the first
// version of this function reported whatever pair was closest among the
// extracted facts regardless of the actual gap, surfacing a real 16-day-
// apart pair as if it satisfied a question that explicitly said
// "consecutive days" — the answering model correctly ignored it, but an
// honest "no hint" is the right output here, not a technically-closest-
// but-implausible one.
func TestResolveConsecutivePairHint_RejectsPairTooFarApart(t *testing.T) {
	facts := []parsedAggregationFact{
		{description: "Attended a charity gala", date: time.Date(2023, 1, 30, 0, 0, 0, 0, time.UTC)},
		{description: "Volunteered at a charity book sort", date: time.Date(2023, 2, 15, 0, 0, 0, 0, time.UTC)}, // 16 days later
	}
	now := time.Date(2023, 3, 20, 0, 0, 0, 0, time.UTC)
	got := resolveAggregationHint("how many months since two charity events in a row, on consecutive days?", facts, now)
	if got != "" {
		t.Errorf("resolveAggregationHint() = %q, want empty — 16 days apart is not \"consecutive\"", got)
	}
}

func TestResolveOrderHint_OrdersChronologically(t *testing.T) {
	facts := []parsedAggregationFact{
		{description: "Watched the triathlon", date: time.Date(2023, 1, 20, 0, 0, 0, 0, time.UTC)},
		{description: "Watched the 5K", date: time.Date(2023, 1, 5, 0, 0, 0, 0, time.UTC)},
		{description: "Watched the soccer match", date: time.Date(2023, 1, 12, 0, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2023, 2, 1, 0, 0, 0, 0, time.UTC)
	got := resolveAggregationHint("what is the order of the sports events I watched in January?", facts, now)

	i5K := strings.Index(got, "5K")
	iSoccer := strings.Index(got, "soccer")
	iTriathlon := strings.Index(got, "triathlon")
	if i5K < 0 || iSoccer < 0 || iTriathlon < 0 {
		t.Fatalf("resolveAggregationHint() = %q, missing an expected fact", got)
	}
	if !(i5K < iSoccer && iSoccer < iTriathlon) {
		t.Errorf("resolveAggregationHint() = %q, want 5K before soccer before triathlon (chronological order)", got)
	}
}

func TestRelativeDelta_BasicTiers(t *testing.T) {
	now := time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		start time.Time
		want  string
	}{
		{time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC), "today"},
		{time.Date(2024, 6, 4, 0, 0, 0, 0, time.UTC), "1 day ago"},
		{time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC), "3 weeks ago"}, // 21 days
		{time.Date(2023, 6, 5, 0, 0, 0, 0, time.UTC), "1 year ago"},  // 366 days
	}
	for _, tc := range cases {
		if got := relativeDelta(tc.start, now); got != tc.want {
			t.Errorf("relativeDelta(%v) = %q, want %q", tc.start, got, tc.want)
		}
	}
}

func TestRelativeDelta_FutureStartReturnsEmpty(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := relativeDelta(time.Date(2024, 6, 5, 0, 0, 0, 0, time.UTC), now); got != "" {
		t.Errorf("relativeDelta(future) = %q, want empty", got)
	}
}

func TestLooksLikeConsecutivePairRequest(t *testing.T) {
	cases := []struct {
		q    string
		want bool
	}{
		{"how many months since two charity events in a row?", true},
		{"how many consecutive days did I run?", true},
		{"what is the order of the sports events I watched?", false},
		{"which came first, the triathlon or the 5K?", false},
	}
	for _, tc := range cases {
		if got := looksLikeConsecutivePairRequest(tc.q); got != tc.want {
			t.Errorf("looksLikeConsecutivePairRequest(%q) = %v, want %v", tc.q, got, tc.want)
		}
	}
}

// TestAggregationHint_EndToEnd exercises the full extract-then-resolve
// pipeline together, the same shape Handler actually calls.
func TestAggregationHint_EndToEnd(t *testing.T) {
	judge := &fakeJudge{content: `{"facts": [
		{"description": "Attended a charity gala", "date": "2023-02-14"},
		{"description": "Attended a charity auction", "date": "2023-02-15"}
	]}`}
	now := time.Date(2023, 5, 15, 0, 0, 0, 0, time.UTC)
	got := AggregationHint(context.Background(), judge, "how many months since two charity events in a row?", "some context", now)
	if got == "" {
		t.Fatal("AggregationHint() = empty, want a computed hint")
	}
	if !strings.Contains(got, "3 months ago") {
		t.Errorf("AggregationHint() = %q, want it to compute ~3 months", got)
	}
}

func TestAggregationHint_NoFactsReturnsEmpty(t *testing.T) {
	judge := &fakeJudge{content: `{"facts": []}`}
	got := AggregationHint(context.Background(), judge, "q", "c", time.Now())
	if got != "" {
		t.Errorf("AggregationHint() = %q, want empty when extraction finds nothing", got)
	}
}

// TestHandleChatCompletions_AggregationPassInjectsHintWhenNeeded is the
// wiring test, not just the pure-logic ones above — confirms Handler
// actually gates on RetrievalResult.NeedsAggregationPass, makes the one
// extra extraction call, and injects the resulting hint into what the
// final answer call sees. Mirrors
// TestHandleChatCompletions_DeepExplainRunsAttributionCheck's own
// upstream-call-counting/content-detection pattern.
//
// postChatCompletion's request body always asks a fixed "hi" — not an
// "in a row"/"consecutive" phrase — so resolveAggregationHint correctly
// takes the order-hint branch here, not the pair-hint branch (that
// distinction is already covered directly by
// TestResolveConsecutivePairHint_.../TestResolveOrderHint_... above);
// this test's own job is only to confirm the wiring actually fires and
// actually injects whatever hint comes back.
func TestHandleChatCompletions_AggregationPassInjectsHintWhenNeeded(t *testing.T) {
	var upstreamCalls int
	var finalAnswerMessages []any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msgs, _ := body["messages"].([]any)
		isExtraction := false
		for _, m := range msgs {
			msg, _ := m.(map[string]any)
			if content, _ := msg["content"].(string); strings.Contains(content, "Retrieved context") {
				isExtraction = true
			}
		}

		w.Header().Set("Content-Type", "application/json")
		content := "hello from upstream"
		if isExtraction {
			content = `{"facts": [{"description": "Attended a charity gala", "date": "2023-02-14"}, {"description": "Attended a charity auction", "date": "2023-02-15"}]}`
		} else {
			finalAnswerMessages = msgs
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

	retriever := &fakeRetriever{result: RetrievalResult{
		Gate:                 GateFull,
		ContextMessage:       "some retrieved context",
		NeedsAggregationPass: true,
	}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	w := postChatCompletion(t, h, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls = %d, want 2 (extraction + answer)", upstreamCalls)
	}

	foundHint := false
	for _, m := range finalAnswerMessages {
		msg, _ := m.(map[string]any)
		if content, _ := msg["content"].(string); strings.Contains(content, "charity gala") && strings.Contains(content, "computation over the retrieved facts") {
			foundHint = true
		}
	}
	if !foundHint {
		t.Errorf("final answer call's messages = %+v, want one containing the computed aggregation hint", finalAnswerMessages)
	}
}

// TestHandleChatCompletions_NoAggregationPassWhenNotNeeded confirms the
// gate actually gates — no extra call, no cost, for the common case.
func TestHandleChatCompletions_NoAggregationPassWhenNotNeeded(t *testing.T) {
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

	retriever := &fakeRetriever{result: RetrievalResult{
		Gate:                 GateFull,
		ContextMessage:       "some retrieved context",
		NeedsAggregationPass: false,
	}}
	h := &Handler{Registry: reg, Retriever: retriever, Capturer: &fakeCapturer{}}

	w := postChatCompletion(t, h, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d, want 1 (answer only, no aggregation pass)", upstreamCalls)
	}
}
