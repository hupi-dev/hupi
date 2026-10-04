package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"hupi/internal/provider"
)

// queryExpansionEnabled gates a real production behavior/cost change
// (an extra LLM call plus a couple of extra embedding slots on every
// retrieval that opts in) — off by default, same "off unless explicitly
// turned on" posture as every other real-cost toggle in this codebase
// (e.g. HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS). Checked at call time,
// not cached, so a config reload takes effect without a restart — same
// convention keywordSearchEnabled already uses.
func queryExpansionEnabled() bool {
	return os.Getenv("HUPI_ENABLE_QUERY_EXPANSION") == "true"
}

// queryExpansionMaxParaphrases bounds the real cost this adds: each
// paraphrase is one more vector search (and one more per-fact search) in
// fusedSearchSummaries, run as a loop over queryVectors — see that
// function's own doc comment.
const queryExpansionMaxParaphrases = 2

// queryExpansionSystemPrompt is deliberately narrow and single-purpose,
// same reasoning as contradictionCheckPrompt's own doc comment: one
// focused rephrasing task, not general-purpose chat. Targets the real,
// confirmed failure shape (docs/LONGMEMEVAL_ACCURACY_PLAN.md's 852ce960
// case) where a stored fact's own wording ("pre-approved for $400,000
// from Wells Fargo") and the query's wording ("mortgage pre-approval
// amount") share little vocabulary — a single query embedding can miss
// that gap even though the fact is exactly what's being asked for.
const queryExpansionSystemPrompt = `You are rephrasing a question into alternate phrasings that ask for the exact same information using different concrete words, so a semantic search can also match text that states the same fact in different terms than the question itself uses.

Respond with exactly one JSON object, nothing else, no markdown fences:
{"paraphrases": ["<alternate phrasing 1>", "<alternate phrasing 2>"]}

Each paraphrase must be a complete, free-standing question or statement (not a keyword list, not a sentence fragment) that asks for precisely what the original question asks for — nothing broader, nothing narrower, nothing added. Prefer different concrete vocabulary for the same specific thing (a different way of naming an amount, a relationship, an event, or a place) over simply reordering the same words.`

func buildQueryExpansionPrompt(query string) string {
	return "Question: " + query
}

// generateQueryParaphrases is best-effort by design (see its one caller
// in retrieve.go): a failure here should degrade to the single-query
// behavior every retrieval already has, not fail the whole turn.
func (s *Store) generateQueryParaphrases(ctx context.Context, query string) ([]string, error) {
	resp, err := s.chatProvider.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: queryExpansionSystemPrompt},
			{Role: provider.RoleUser, Content: buildQueryExpansionPrompt(query)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("query expansion chat call: %w", err)
	}
	var parsed struct {
		Paraphrases []string `json:"paraphrases"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
		return nil, fmt.Errorf("parse query expansion response: %w", err)
	}
	if len(parsed.Paraphrases) > queryExpansionMaxParaphrases {
		parsed.Paraphrases = parsed.Paraphrases[:queryExpansionMaxParaphrases]
	}
	return parsed.Paraphrases, nil
}

// extractJSON is internal/gateway/attribution.go's own helper, duplicated
// rather than imported — this package deliberately has no dependency on
// gateway's internals beyond the gateway.Capturer/gateway.Retriever
// interfaces it implements (see store.go's own doc comment), the same
// "small tools duplicate rather than force an awkward cross-package
// dependency" convention already used for extractJSON in
// internal/consolidation/prompts.go. Finds the first top-level balanced
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
