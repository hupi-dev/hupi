package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"hupi/internal/provider"
)

// rerankingEnabled gates a real production behavior/cost change (one
// extra LLM call, only for ordering-shaped retrievals — see this
// feature's own gate in fusedSearchSummaries) — off by default, same
// posture as queryExpansionEnabled. Checked at call time, not cached,
// same convention as every other toggle in this file.
func rerankingEnabled() bool {
	return os.Getenv("HUPI_ENABLE_LLM_RERANK") == "true"
}

// rerankMaxCandidateChars bounds the real cost this adds: the
// ordering-widened candidate pool can reach ~45 summaries
// (orderingSummaryMaxResults * summaryOverfetchFactor), and without a
// per-candidate cap a single rerank call could balloon past what's
// reasonable for one LLM request. Real measurement (not estimated):
// even bounded this way, one call costs ~3.5-4.3s (~20 candidates,
// ~10-12K prompt chars) — the reason this feature is gated to only the
// already-latency-accepting ordering-shaped case, not every retrieval
// (see fusedSearchSummaries' own call site doc comment).
const rerankMaxCandidateChars = 400

// rerankSystemPrompt is deliberately narrow, same register as
// queryExpansionSystemPrompt/aggregationExtractionSystemPrompt — one
// focused judgment (how directly does each excerpt bear on answering
// the question), not general-purpose chat. Targets the real, confirmed
// failure shape (docs/BENCHMARK_IMPROVEMENT_PLAN.md's "aunt" case and
// mostRelevantFactIndex's own documented lexical-overlap limitation):
// RRF's additive rank fusion and lexical overlap both can't distinguish
// "this excerpt actually answers the question" from "this excerpt is
// topically adjacent, found by the same mechanisms, but not what's
// actually needed."
const rerankSystemPrompt = `You are judging how directly a set of retrieved excerpts actually answers a question, not just whether they're topically related to it.

For each excerpt, score how directly and specifically it bears on answering the question, from 0 to 10: 10 means it directly states the answer or a fact essential to answering it; 0 means it is not relevant at all, or only superficially topical. An excerpt about the same general topic, person, or place as the question — but that doesn't itself provide information that helps answer it — should score low, even if it shares a lot of vocabulary with the question. Relevance means actually helping answer the question, not resembling its wording.

Respond with exactly one JSON object, nothing else, no markdown fences:
{"scores": [{"id": "<the excerpt's id, exactly as given>", "score": <0-10>}, ...]}

Score every excerpt given, using its exact id.`

// rerankCandidate is rerankSummaries' input shape — just enough to
// build the prompt and to key results back to the caller's own
// candidate pool (mmrCandidate itself carries no id back-reference, so
// the caller remaps by id after this returns).
type rerankCandidate struct {
	id     string
	period string
	text   string
}

func buildRerankPrompt(query string, candidates []rerankCandidate) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Question: %s\n\nExcerpts:\n", query)
	for _, c := range candidates {
		fmt.Fprintf(&sb, "\n[id=%s, period=%s]\n%s\n", c.id, c.period, truncateToBudget(c.text, rerankMaxCandidateChars))
	}
	return sb.String()
}

type rerankScore struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

type rerankResponse struct {
	Scores []rerankScore `json:"scores"`
}

// rerankSummaries is best-effort by design (see its one caller in
// retrieve.go): a failure here should degrade to the unchanged RRF-fused
// scores every retrieval already has, not fail the whole turn — same
// shape as generateQueryParaphrases.
func (s *Store) rerankSummaries(ctx context.Context, query string, candidates []rerankCandidate) (map[string]float64, error) {
	prompt := buildRerankPrompt(query, candidates)
	start := time.Now()
	resp, err := s.chatProvider.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: rerankSystemPrompt},
			{Role: provider.RoleUser, Content: prompt},
		},
	})
	slog.Default().Warn("rerank chat call timing", "candidates", len(candidates), "prompt_chars", len(prompt), "duration", time.Since(start), "error", err)
	if err != nil {
		return nil, fmt.Errorf("rerank chat call: %w", err)
	}
	var parsed rerankResponse
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
		return nil, fmt.Errorf("parse rerank response: %w", err)
	}
	scores := make(map[string]float64, len(parsed.Scores))
	for _, sc := range parsed.Scores {
		scores[sc.ID] = sc.Score
	}
	return scores, nil
}
