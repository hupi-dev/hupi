package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"hupi/internal/provider"
)

// attributionSystemPrompt mirrors internal/consolidation/grounding.go's
// groundingCheck pattern exactly (numbered items + reference text + a
// single JSON verdict), swapping "is this fact grounded in source text"
// for "does this answer actually rely on this snippet" — Phase 2 of
// docs/ANSWER_CITATIONS_PLAN.md. Phase 1's Citations show what memory
// was *available* when the answer was generated; this verifies what the
// answer actually *used*.
const attributionSystemPrompt = `You will be given a generated answer and a numbered list of candidate memory snippets that were available when it was written. For each snippet, in order, decide whether the answer's content actually, specifically relies on it — not just topically related, but relied on to produce a detail in the answer. Respond with exactly one JSON object, nothing else: {"used": [true, false, ...]} — one boolean per snippet, same order and count as given.`

func buildAttributionPrompt(answer string, citations []Citation) string {
	var sb strings.Builder
	sb.WriteString("Generated answer:\n" + answer + "\n\nCandidate memory snippets:\n")
	for i, c := range citations {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, c.Snippet)
	}
	return sb.String()
}

// attributionCheckBatchSize caps how many citations go into one
// attribution call — the identical reasoning
// internal/consolidation/grounding.go's groundingCheckBatchSize already
// applies to grounding checks. Citations moved to fact granularity in
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's Phase 0 (one per key fact/
// attribute, not one per summary/entity), which can turn what used to
// be a handful of bundled citations per request into several times
// that many — the same "large count overwhelms one LLM call, discard
// everything on a count mismatch" failure mode grounding checks were
// already hardened against, now reachable here too.
const attributionCheckBatchSize = 20

// attributionCheck asks judge which of citations the given answer
// actually relies on, splitting into attributionCheckBatchSize-sized
// batches (same pattern as internal/consolidation/grounding.go's
// groundingCheck) so one oversized request can't degrade every
// citation's verdict at once — a failure confined to one batch no
// longer costs the others. judge is typically the same Provider that
// generated the answer (handleNonStream passes target) — no separate
// "judge" provider role exists for this, unlike consolidation's
// grounding check, since this only runs when a caller explicitly asks
// for deep attribution and reusing the answer model needs no new
// config.
func attributionCheck(ctx context.Context, judge provider.Provider, answer string, citations []Citation) ([]bool, error) {
	if len(citations) == 0 {
		return nil, nil
	}
	if len(citations) > attributionCheckBatchSize {
		all := make([]bool, 0, len(citations))
		for start := 0; start < len(citations); start += attributionCheckBatchSize {
			end := start + attributionCheckBatchSize
			if end > len(citations) {
				end = len(citations)
			}
			batch, err := attributionCheckOne(ctx, judge, answer, citations[start:end])
			if err != nil {
				return nil, err
			}
			all = append(all, batch...)
		}
		return all, nil
	}
	return attributionCheckOne(ctx, judge, answer, citations)
}

// attributionCheckOne is one LLM call's worth of attributionCheck —
// never more than attributionCheckBatchSize citations.
//
// A verdict this can't parse, or that comes back the wrong length,
// returns an error rather than silently defaulting every citation to
// "not used" — a real, confirmed bug this used to have
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A2): Citation.Used's own
// contract (handler.go) is nil-means-"not checked", specifically so a
// caller can't mistake "not checked" with "checked and found unused" —
// but synthesizing a false-filled slice with a nil error made exactly
// that confusion happen for every malformed-judge-response case, not
// just the LLM-call-failure case the caller already handled correctly.
// Both call sites already leave Used nil whenever this returns a
// non-nil error (the LLM-call-failure path always worked this way);
// this change just routes the other two failure modes through that
// same, already-correct path instead of inventing a third, wrong
// outcome.
func attributionCheckOne(ctx context.Context, judge provider.Provider, answer string, citations []Citation) ([]bool, error) {
	resp, err := judge.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: attributionSystemPrompt},
			{Role: provider.RoleUser, Content: buildAttributionPrompt(answer, citations)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("attribution LLM call: %w", err)
	}

	var result struct {
		Used []bool `json:"used"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &result); err != nil {
		slog.Warn("gateway: could not parse attribution result, leaving citations unchecked", "error", err)
		return nil, fmt.Errorf("attribution: parse judge response: %w", err)
	}
	if len(result.Used) != len(citations) {
		slog.Warn("gateway: attribution check returned a mismatched result count, leaving citations unchecked",
			"got", len(result.Used), "want", len(citations))
		return nil, fmt.Errorf("attribution: judge returned %d verdicts, want %d", len(result.Used), len(citations))
	}
	return result.Used, nil
}

// extractJSON is duplicated from internal/consolidation/prompts.go
// rather than exported and imported across packages for one small,
// self-contained, dependency-free helper — same "small tools duplicate
// rather than force an awkward cross-package dependency" convention
// this repo already uses elsewhere (e.g. cmd/hupi-answer-question's own
// doc comment on sendChatTurn). Finds the first top-level balanced
// {...} object in s, tolerating surrounding prose/markdown fences a raw
// json.Unmarshal on the whole string would choke on.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return s
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// inside a string literal, braces don't count
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}
