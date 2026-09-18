package consolidation

import (
	"fmt"
	"strings"
)

// summarySystemPrompt instructs the model to self-cite (source_episode_ids
// per fact) and to only assert what the source actually supports. That
// instruction is necessary but not sufficient — groundingCheck (see
// grounding.go) independently re-verifies every fact against the source
// text rather than trusting this instruction was followed.
const summarySystemPrompt = `You are HUPI's consolidation engine (see MEMORY_FORMAT.md § Grounding & correction). You will be given a set of labeled source texts and must respond with exactly one JSON object, nothing else, no markdown fences, of this shape:

{
  "summary": "one paragraph of prose, for human skimming only, not treated as fact",
  "key_facts": [
    {"fact": "a single concrete, checkable fact", "source_episode_ids": ["<id>", ...]}
  ],
  "entities_touched": [
    {"id": "kind:slug", "kind": "person|project|preference|skill|place|organization", "name": "...", "attributes": {"key": "value"}}
  ]
}

Only include a key_fact if it is directly and specifically supported by the source texts you were given. Cite the exact source ids it came from. Do not include anything you are inferring, generalizing, or guessing beyond what the source text states.`

// buildSummaryPrompt assembles the consolidation LLM's user message.
// establishedRecord, when non-empty, is the prose of the day's *current*
// draft before this re-consolidation run — RunDaily passes this when a
// day that already has a summary accumulates more episodes and gets
// re-consolidated (internal/store/retrieve.go's supersede-not-fork fix).
//
// This exists because of a real failure mode found by testing that exact
// path end to end: regenerating purely from raw episode transcripts with
// no memory of what was already established meant a later re-run could
// see the user's original raw statement (e.g. "500 concurrent jobs") and
// the assistant's own later, correctly-corrected answer (e.g. "5,000",
// grounded in a real hupi-correct) as two conflicting claims — with
// nothing in the raw episodes revealing that the correction was
// deliberate and authoritative, not a hallucination. The model reasonably
// but wrongly treated its own past answer as the less trustworthy one and
// walked the correction back. Telling it the established record already
// reflects any corrections, and to only override it on explicit new
// evidence in the sources, prevents that regression.
func buildSummaryPrompt(level, period string, sources []textSource, establishedRecord string) string {
	var sb strings.Builder
	if establishedRecord != "" {
		fmt.Fprintf(&sb, "ALREADY-ESTABLISHED RECORD for this %s — the current, reviewed summary before this re-consolidation, which may already incorporate one or more deliberate human corrections not visible anywhere in the raw source texts below. Treat every fact in it as settled and correct. Only change something from it if a source text below EXPLICITLY states a new fact that supersedes it (the user or a later message clearly states a value changed, a decision was reversed, etc). Do NOT contradict, doubt, or walk back anything here merely because a raw source phrases something differently, states an earlier value in passing, or because your own past response in a source text differs from it — this record already reflects the outcome of any corrections that were made, even when the raw sources don't show that correction happening.\n\n%s\n\n", level, establishedRecord)
	}
	fmt.Fprintf(&sb, "Level: %s\nPeriod: %s\n\nSource texts:\n", level, period)
	for _, s := range sources {
		fmt.Fprintf(&sb, "\n--- id: %s ---\n%s\n", s.id, s.text)
	}
	return sb.String()
}

// extractJSON pulls the first complete, balanced {...} object out of a
// model response, since even when instructed to return "JSON only,"
// models sometimes wrap it in prose or a markdown code fence. Scans by
// brace depth rather than just first-'{'/last-'}', so it stops at the
// first object's real closing brace instead of spanning all the way to
// an unrelated '}' in trailing prose — and skips over braces inside
// quoted string values (e.g. a fact whose text happens to mention "{" or
// "}") so those don't miscount the depth. A markdown fence needs no
// special-casing: the scan already ignores everything before the first
// '{' and after that object's matching '}', fence characters included.
//
// A hardened build should use each provider's structured-output/
// tool-calling mode instead of parsing free text at all
// (internal/provider has no such plumbing today) — this is a more
// careful stand-in, not that fix.
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
			// braces inside a quoted string don't affect depth
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	// Never balanced — same fallback as the original: hand back whatever
	// followed the opening brace and let json.Unmarshal produce the real
	// error.
	return s[start:]
}
