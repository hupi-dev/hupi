package gateway

import "strings"

// estimateImportance is the cheap, local-only heuristic from
// MEMORY_FORMAT.md § Episode record: no network or LLM call, specifically
// so Capture never depends on one (see ARCHITECTURE.md § Capture, durable
// and synchronous). It's deliberately crude — real salience judgment comes
// from consolidation's grounding-checked summaries, not from this number.
func estimateImportance(input, output string) float64 {
	text := strings.ToLower(input + " " + output)
	score := 0.2 // baseline: every captured turn is worth something

	for _, kw := range []string{
		"decide", "decided", "prefer", "always", "never", "remember",
		"project", "working directory", "plan to", "going forward",
	} {
		if strings.Contains(text, kw) {
			score += 0.1
		}
	}
	if strings.Contains(input, "?") {
		score += 0.05
	}
	if score > 1 {
		score = 1
	}
	return score
}
