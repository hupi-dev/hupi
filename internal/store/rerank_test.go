package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// histogramSampleCount reads a plain (non-vector) Histogram's cumulative
// observation count directly via its Write method — testutil.CollectAndCount
// returns the number of metric *series* (always 1 for a non-vector
// histogram, regardless of how many observations it's recorded), not the
// observation count itself, so it can't distinguish "never observed"
// from "observed many times."
func histogramSampleCount(t *testing.T, h interface{ Write(*dto.Metric) error }) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

func TestBuildRerankPrompt_IncludesIDPeriodAndTruncatesText(t *testing.T) {
	long := strings.Repeat("x", rerankMaxCandidateChars+50)
	got := buildRerankPrompt("how many tournaments has Nate won?", []rerankCandidate{
		{id: "sum_a", period: "2022-01-21", text: "Nate won his first tournament"},
		{id: "sum_b", period: "2022-02-01", text: long},
	})
	if !strings.Contains(got, "how many tournaments has Nate won?") {
		t.Error("prompt missing the question")
	}
	if !strings.Contains(got, "id=sum_a, period=2022-01-21") {
		t.Error("prompt missing first candidate's id/period")
	}
	if !strings.Contains(got, "id=sum_b, period=2022-02-01") {
		t.Error("prompt missing second candidate's id/period")
	}
	if strings.Contains(got, long) {
		t.Error("prompt should truncate an over-long candidate, not include it verbatim")
	}
}

type fakeRerankProvider struct {
	response string
	err      error
}

func (fakeRerankProvider) Name() string   { return "fake" }
func (fakeRerankProvider) Vendor() string { return "fake" }
func (fakeRerankProvider) Model() string  { return "fake-chat" }

func (f fakeRerankProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	if f.err != nil {
		return provider.ChatResponse{}, f.err
	}
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: f.response}}, nil
}

func (fakeRerankProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (fakeRerankProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errFakeNotImplemented
}

func TestRerankSummaries_ParsesScores(t *testing.T) {
	s := &Store{chatProvider: fakeRerankProvider{response: `{"scores": [{"id": "sum_a", "score": 9}, {"id": "sum_b", "score": 1}]}`}}
	got, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}, {id: "sum_b"}})
	if err != nil {
		t.Fatalf("rerankSummaries: %v", err)
	}
	if got["sum_a"] != 9 || got["sum_b"] != 1 {
		t.Errorf("got %v, want sum_a=9 sum_b=1", got)
	}
}

func TestRerankSummaries_ToleratesMarkdownFence(t *testing.T) {
	s := &Store{chatProvider: fakeRerankProvider{response: "```json\n{\"scores\": [{\"id\": \"sum_a\", \"score\": 5}]}\n```"}}
	got, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}})
	if err != nil {
		t.Fatalf("rerankSummaries: %v", err)
	}
	if got["sum_a"] != 5 {
		t.Errorf("got %v, want sum_a=5", got)
	}
}

func TestRerankSummaries_CallFailureReturnsError(t *testing.T) {
	s := &Store{chatProvider: fakeRerankProvider{err: errFakeNotImplemented}}
	if _, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}}); err == nil {
		t.Error("rerankSummaries() with a failing provider = nil error, want non-nil")
	}
}

func TestRerankSummaries_MalformedResponseReturnsError(t *testing.T) {
	s := &Store{chatProvider: fakeRerankProvider{response: "not json at all"}}
	if _, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}}); err == nil {
		t.Error("rerankSummaries() with a malformed response = nil error, want non-nil")
	}
}

// TestRerankSummaries_SuccessIncrementsOkCounterAndDuration confirms the
// real observability this feature needs before being safe to turn on in
// production (docs/BENCHMARKS.md §9: each call costs ~3.5-4.3s measured
// directly, not estimated — a number that needs to be watchable, not
// just grep-able from a log line).
func TestRerankSummaries_SuccessIncrementsOkCounterAndDuration(t *testing.T) {
	okBefore := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("ok"))
	errBefore := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("error"))
	countBefore := histogramSampleCount(t, metrics.RerankDuration)

	s := &Store{chatProvider: fakeRerankProvider{response: `{"scores": [{"id": "sum_a", "score": 9}]}`}}
	if _, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}}); err != nil {
		t.Fatalf("rerankSummaries: %v", err)
	}

	if got := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("ok")); got != okBefore+1 {
		t.Errorf(`hupi_rerank_calls_total{result="ok"} went from %v to %v, want exactly +1`, okBefore, got)
	}
	if got := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("error")); got != errBefore {
		t.Errorf(`hupi_rerank_calls_total{result="error"} changed on a success, want unchanged: %v -> %v`, errBefore, got)
	}
	if got := histogramSampleCount(t, metrics.RerankDuration); got != countBefore+1 {
		t.Errorf("hupi_rerank_duration_seconds observation count went from %v to %v, want exactly +1", countBefore, got)
	}
}

// TestRerankSummaries_FailureIncrementsErrorCounter is
// SuccessIncrementsOkCounterAndDuration's negative counterpart — a
// failed call still costs real wall-clock time (duration is always
// observed) but must not be miscounted as "ok".
func TestRerankSummaries_FailureIncrementsErrorCounter(t *testing.T) {
	okBefore := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("ok"))
	errBefore := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("error"))

	s := &Store{chatProvider: fakeRerankProvider{err: errFakeNotImplemented}}
	if _, err := s.rerankSummaries(context.Background(), "irrelevant", []rerankCandidate{{id: "sum_a"}}); err == nil {
		t.Fatal("rerankSummaries() with a failing provider = nil error, want non-nil")
	}

	if got := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("error")); got != errBefore+1 {
		t.Errorf(`hupi_rerank_calls_total{result="error"} went from %v to %v, want exactly +1`, errBefore, got)
	}
	if got := testutil.ToFloat64(metrics.RerankCallsTotal.WithLabelValues("ok")); got != okBefore {
		t.Errorf(`hupi_rerank_calls_total{result="ok"} changed on a failure, want unchanged: %v -> %v`, okBefore, got)
	}
}

// TestFusedSearchSummaries_RerankFlipsAStructurallyWrongRRFPick is the
// real, motivating regression test (docs/BENCHMARK_IMPROVEMENT_PLAN.md's
// "aunt" case): a candidate found by BOTH vector and keyword search, even
// weakly on each, structurally outranks a candidate found strongly by
// only one mechanism, regardless of which one is actually relevant. This
// reproduces that shape directly (not the exact real ranks, which need a
// much bigger candidate pool to force naturally) — a generic decoy
// matches the query's embedding exactly (similarity 1.0, vector rank 0)
// and shares the query's own keyword (keyword rank 0), while the
// specific, actually-relevant summary only partially matches the
// embedding (similarity 0.8, vector rank 1) and shares no keyword term at
// all — confirms plain RRF picks the decoy, then confirms a stubbed
// rerank response flips the pick to the relevant one.
func TestFusedSearchSummaries_RerankFlipsAStructurallyWrongRRFPick(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rerank-flip"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	queryVec := zeroExceptDim(0)
	decoyVec := zeroExceptDim(0) // identical to the query: similarity 1.0
	targetVec := make([]float32, len(queryVec))
	targetVec[0] = 0.8 // partial match: similarity 0.8, strictly worse than the decoy's 1.0

	insertSummaryWithEmbedding(t, s, scope, "sum_decoy", period,
		"The zorbathon was a huge, well-attended annual community event with many booths and a parade.", decoyVec)
	insertSummaryWithEmbedding(t, s, scope, "sum_target", period,
		"Maria's aunt gave the family emergency money during a hard financial period.", targetVec)

	queryVector := pgfmt.VectorLiteral(queryVec)
	// Ordering-shaped on purpose (contains "which came first") — reranking
	// is gated to only this query shape (looksLikeOrderingRequest), not
	// every retrieval, so the test must actually trigger that gate, not
	// just set up the RRF-vs-relevance conflict.
	question := "Which came first, Maria's family getting emergency money or the zorbathon?"

	runFusedSearch := func() string {
		var sb strings.Builder
		strongHit := false
		var citations []gateway.Citation
		err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
			_, err := s.fusedSearchSummaries(ctx, tx, scope, []string{queryVector}, []string{"zorbathon"}, &sb, &strongHit, &citations,
				0.1, 1, question, time.Now(), keywordSearchFull, nil)
			return err
		})
		if err != nil {
			t.Fatalf("fusedSearchSummaries: %v", err)
		}
		return sb.String()
	}

	// Without reranking: plain RRF should pick the decoy (dual-signal:
	// vector rank 0 + keyword rank 0) over the target (single-signal:
	// vector rank 1 only) — this assertion documents the real structural
	// problem, not just sets up the fix.
	got := runFusedSearch()
	if !strings.Contains(got, "zorbathon") {
		t.Fatalf("expected plain RRF to pick the dual-signal decoy, got: %q", got)
	}
	if strings.Contains(got, "aunt") {
		t.Fatalf("plain RRF unexpectedly picked the correct target on its own — test no longer reproduces the real failure shape, got: %q", got)
	}

	// With reranking enabled and a stubbed response scoring the target
	// far higher: the pick should flip.
	t.Setenv("HUPI_ENABLE_LLM_RERANK", "true")
	s.EnableQueryExpansion(fakeRerankProvider{response: `{"scores": [{"id": "sum_decoy", "score": 1}, {"id": "sum_target", "score": 9}]}`})

	got = runFusedSearch()
	if !strings.Contains(got, "aunt") {
		t.Errorf("reranking enabled: expected the correct target to be picked, got: %q", got)
	}
	if strings.Contains(got, "zorbathon") {
		t.Errorf("reranking enabled: decoy should no longer be picked, got: %q", got)
	}
}

// TestFusedSearchSummaries_RerankFailureDegradesToRRF confirms the
// best-effort contract: a failing rerank call must leave the RRF-fused
// pick exactly as it would have been without reranking at all, never
// block or alter the turn.
func TestFusedSearchSummaries_RerankFailureDegradesToRRF(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rerank-degrade"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	queryVec := zeroExceptDim(0)
	insertSummaryWithEmbedding(t, s, scope, "sum_decoy", period,
		"The zorbathon was a huge, well-attended annual community event.", zeroExceptDim(0))
	targetVec := make([]float32, len(queryVec))
	targetVec[0] = 0.8
	insertSummaryWithEmbedding(t, s, scope, "sum_target", period,
		"Maria's aunt gave the family emergency money during a hard financial period.", targetVec)

	t.Setenv("HUPI_ENABLE_LLM_RERANK", "true")
	s.EnableQueryExpansion(fakeRerankProvider{err: errFakeNotImplemented})

	queryVector := pgfmt.VectorLiteral(queryVec)
	var sb strings.Builder
	strongHit := false
	var citations []gateway.Citation
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := s.fusedSearchSummaries(ctx, tx, scope, []string{queryVector}, []string{"zorbathon"}, &sb, &strongHit, &citations,
			0.1, 1, "Which came first, Maria's family getting emergency money or the zorbathon?", time.Now(), keywordSearchFull, nil)
		return err
	})
	if err != nil {
		t.Fatalf("fusedSearchSummaries: %v", err)
	}
	if !strings.Contains(sb.String(), "zorbathon") {
		t.Errorf("a failing rerank call should degrade to the unchanged RRF pick (the decoy), got: %q", sb.String())
	}
}

// TestFusedSearchSummaries_RerankSkipsOrdinaryNonOrderingQuestions is the
// real regression test for this feature's own latency-scoping gate: no
// other best-effort LLM pass in this codebase adds mandatory latency to
// every live chat turn (attributionCheck requires explainMode=="deep",
// AggregationHint requires an ordering-shaped question, grounding/
// contradiction checks run at consolidation time only) — a measured
// ~3.5-4.3s per rerank call means this must stay opt-in by query shape
// too, not fire on every retrieval just because the env flag is set.
// Confirms an ordinary (non-ordering-shaped) question never even calls
// the stubbed provider, by configuring it to fail the test outright if
// invoked.
func TestFusedSearchSummaries_RerankSkipsOrdinaryNonOrderingQuestions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rerank-skip-ordinary"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	queryVec := zeroExceptDim(0)
	insertSummaryWithEmbedding(t, s, scope, "sum_decoy", period,
		"The zorbathon was a huge, well-attended annual community event.", zeroExceptDim(0))

	t.Setenv("HUPI_ENABLE_LLM_RERANK", "true")
	s.EnableQueryExpansion(rerankCallMustNotHappenProvider{t: t})

	queryVector := pgfmt.VectorLiteral(queryVec)
	var sb strings.Builder
	strongHit := false
	var citations []gateway.Citation
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := s.fusedSearchSummaries(ctx, tx, scope, []string{queryVector}, []string{"zorbathon"}, &sb, &strongHit, &citations,
			0.1, 1, "What happened at the zorbathon?", time.Now(), keywordSearchFull, nil)
		return err
	})
	if err != nil {
		t.Fatalf("fusedSearchSummaries: %v", err)
	}
}

// rerankCallMustNotHappenProvider fails the test outright if
// ChatCompletion is ever called — a stronger assertion than checking the
// output afterward, since it catches the call happening at all, not just
// whether it happened to change the result.
type rerankCallMustNotHappenProvider struct{ t *testing.T }

func (rerankCallMustNotHappenProvider) Name() string   { return "fake" }
func (rerankCallMustNotHappenProvider) Vendor() string { return "fake" }
func (rerankCallMustNotHappenProvider) Model() string  { return "fake-chat" }

func (p rerankCallMustNotHappenProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	p.t.Error("rerank ChatCompletion called for a non-ordering-shaped question — reranking should be gated by query shape")
	return provider.ChatResponse{}, errFakeNotImplemented
}

func (rerankCallMustNotHappenProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (rerankCallMustNotHappenProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errFakeNotImplemented
}
