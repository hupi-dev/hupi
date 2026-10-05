package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"hupi/internal/provider"
)

// inferenceExtractionEnabled gates Phase 4 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md — off by default, same
// posture as redundancyDedupEnabled/queryExpansionEnabled: this mutates
// stored consolidation data (adds new, permanent key_facts), not just a
// ranking change. Checked live, not cached, same convention as every
// other toggle in this package.
func inferenceExtractionEnabled() bool {
	return os.Getenv("HUPI_ENABLE_INFERENCE_EXTRACTION") == "true"
}

// inferenceExtractionPrompt is deliberately a wholly separate system
// prompt from summarySystemPrompt, not an addition to it — real,
// live-verified necessity, not a style preference. Two attempts at
// adding this as an exception to summarySystemPrompt's own extraction
// rules (appended as its own paragraph, then inserted as a carve-out
// directly next to the specific rule it needed to override) both
// failed to fire at all against the real traced case this is anchored
// to (Joanna's allergy profile implying asthma), even though an
// isolated check with no surrounding task framing confirmed the model
// can make the identical inference instantly and correctly. The
// suppression is specific to summarySystemPrompt's own dominant,
// repeatedly-reinforced "extract only what's literally stated" framing
// (its very first key_facts instruction) — a prompt whose only job is
// this one judgment, with no competing framing, fires reliably on the
// same case and correctly declines a real near-miss (a loose,
// non-diagnostic attribute combination), both confirmed live before
// this was wired into production code.
const inferenceExtractionPrompt = `You are looking at a list of known entities and their existing recorded attributes, plus today's conversation mentioning them. Your only job: check whether any entity's existing attributes, combined together, directly and specifically imply an unstated medical or general condition/classification — the kind of confident, specific conclusion a careful expert would draw from that exact combination of facts, not a loose guess or association.

Example: a person's attributes show allergic_to: most reptiles and animals with fur, and allergic_to_cockroaches: yes. Combined, this specific allergen pattern directly implies asthma as the underlying condition, even though asthma is never stated anywhere.

Only report an inference this direct and this confident — the attributes must essentially name the condition's own diagnostic pattern, not just a vague association. If no entity's attributes support this kind of inference, report nothing for it.

Respond with exactly one JSON object, nothing else: {"inferences": [{"entity_id": "<id, exactly as shown>", "fact": "<the inferred fact, stated plainly>", "inferred_from_attribute_keys": ["<key>", ...]}]}. If nothing qualifies, respond with {"inferences": []}.`

// extractedInference is one inferenceExtractionPrompt result.
type extractedInference struct {
	EntityID                  string   `json:"entity_id"`
	Fact                      string   `json:"fact"`
	InferredFromAttributeKeys []string `json:"inferred_from_attribute_keys"`
}

// buildInferencePrompt assembles extractInferences' user message —
// known is the same findKnownEntities result buildSummaryPrompt's own
// KNOWN ENTITIES block already uses, reused here rather than re-queried.
func buildInferencePrompt(known []knownEntityContext, combinedSourceText string) string {
	var sb strings.Builder
	sb.WriteString("KNOWN ENTITIES:\n")
	if formatted := formatKnownEntities(known); formatted != "" {
		sb.WriteString(formatted)
	} else {
		sb.WriteString("(none)\n")
	}
	sb.WriteString("\nToday's conversation:\n")
	sb.WriteString(combinedSourceText)
	return sb.String()
}

// extractInferences runs the dedicated inference-extraction call —
// best-effort, like every other optional enrichment step in this
// package (embedSummary, checkCrossPeriodContradictions): a failure
// here degrades to "no inferences extracted today," never blocks the
// rest of consolidation, since the literal facts generateSummary
// already produced are the part that must not be lost.
func (r *Runner) extractInferences(ctx context.Context, known []knownEntityContext, combinedSourceText string) ([]extractedInference, error) {
	if len(known) == 0 {
		return nil, nil
	}
	temperature := consolidationTemperature
	resp, err := r.consolidation.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: inferenceExtractionPrompt},
			{Role: provider.RoleUser, Content: buildInferencePrompt(known, combinedSourceText)},
		},
		Temperature: &temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("inference extraction LLM call: %w", err)
	}
	var out struct {
		Inferences []extractedInference `json:"inferences"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		return nil, fmt.Errorf("parse inference extraction response: %w", err)
	}
	return out.Inferences, nil
}
