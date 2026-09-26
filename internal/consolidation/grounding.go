package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
		// Same safe-degrade direction as the count-mismatch case just
		// below, for the same reason: a grounding verdict this package
		// can't trust is informationally no different from "couldn't
		// determine groundedness" either way, and defaulting to
		// ungrounded costs nothing but a later manual review — unlike
		// failing outright, which would discard the whole day's
		// otherwise-good summary along with it.
		slog.Warn("consolidation: could not parse grounding result, treating all facts as ungrounded", "error", err)
		result.Grounded = make([]bool, len(facts))
	}
	if len(result.Grounded) != len(facts) {
		// A real, reproduced failure mode: the grounding model doesn't
		// always return exactly one boolean per fact (e.g. it can split
		// an ambiguous fact into sub-judgments). Failing outright here —
		// the original behavior — discarded an otherwise-good summary
		// and every other correctly-grounded fact in it over one
		// miscounted response. Since an ungrounded fact is still stored,
		// just excluded from retrieval until reviewed (this function's
		// own doc comment), defaulting every fact to ungrounded when the
		// count can't be trusted is the same safe direction this package
		// already treats a genuine "no" as — never destroys information,
		// only degrades to "not yet verified."
		slog.Warn("consolidation: grounding check returned a mismatched result count, treating all facts as ungrounded",
			"got", len(result.Grounded), "want", len(facts))
		result.Grounded = make([]bool, len(facts))
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
