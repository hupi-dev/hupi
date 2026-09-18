package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hupi/internal/metrics"
	"hupi/internal/provider"
)

// groundingSystemPrompt deliberately does not see the model's own citation
// (source_episode_ids) — only the raw source text and the bare fact
// strings — so a plausible-looking but unsupported self-citation can't
// talk its way past the check. See MEMORY_FORMAT.md § Grounding &
// correction.
const groundingSystemPrompt = `You are a fact-checker. You will be given source text and a numbered list of claimed facts. For each fact, in order, decide whether the source text actually, specifically supports it — not just plausible, not just related, but stated. Respond with exactly one JSON object, nothing else: {"grounded": [true, false, ...]} — one boolean per fact, same order and count as given.`

func buildGroundingPrompt(sourceText string, facts []KeyFactOutput) string {
	var sb strings.Builder
	sb.WriteString("Source text:\n" + sourceText + "\n\nClaimed facts:\n")
	for i, f := range facts {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, f.Fact)
	}
	return sb.String()
}

// groundingCheck is ARCHITECTURE.md § Consolidation integrity safeguards'
// second pass: a separate LLM call (r.grounding, which may be a
// smaller/cheaper model than r.consolidation) re-verifies every generated
// fact against the actual source text before it's trusted as retrievable
// memory. Facts that fail are not deleted — storeSummary still writes
// them, with grounded=false — since a false-negative here shouldn't
// destroy real information, only keep it out of retrieval until reviewed.
func (r *Runner) groundingCheck(ctx context.Context, sourceText string, facts []KeyFactOutput) ([]bool, error) {
	if len(facts) == 0 {
		return nil, nil
	}

	resp, err := r.grounding.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: groundingSystemPrompt},
			{Role: provider.RoleUser, Content: buildGroundingPrompt(sourceText, facts)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grounding LLM call: %w", err)
	}

	var result struct {
		Grounded []bool `json:"grounded"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &result); err != nil {
		return nil, fmt.Errorf("parse grounding result: %w", err)
	}
	if len(result.Grounded) != len(facts) {
		return nil, fmt.Errorf("grounding check returned %d results for %d facts", len(result.Grounded), len(facts))
	}
	for _, g := range result.Grounded {
		if g {
			metrics.GroundingFactsTotal.WithLabelValues("true").Inc()
		} else {
			metrics.GroundingFactsTotal.WithLabelValues("false").Inc()
		}
	}
	return result.Grounded, nil
}
