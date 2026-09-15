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

// teamSummarySystemPrompt is summarySystemPrompt's counterpart for
// scope_kind='shared' consolidation runs (docs/TIER3_PLAN.md §5): written
// in a neutral team voice rather than any one member's, since a shared
// summary will be read by everyone on the team, not the one person who
// happened to have the conversation.
const teamSummarySystemPrompt = `You are HUPI's consolidation engine, writing a SHARED team summary (see MEMORY_FORMAT.md § Grounding & correction). Multiple team members' conversations may be in the source texts below. Write in a neutral, third-person team-knowledge voice — "the team decided X", not "I decided X" or addressing any one member directly. You must respond with exactly one JSON object, nothing else, no markdown fences, of this shape:

{
  "summary": "one paragraph of prose, for human skimming only, not treated as fact",
  "key_facts": [
    {"fact": "a single concrete, checkable fact", "source_episode_ids": ["<id>", ...]}
  ],
  "entities_touched": [
    {"id": "kind:slug", "kind": "person|project|preference|skill|place|organization", "name": "...", "attributes": {"key": "value"}}
  ]
}

Only include a key_fact if it is directly and specifically supported by the source texts you were given. Cite the exact source ids it came from. Do not include anything you are inferring, generalizing, or guessing beyond what the source text states. Never include any individual's personal preferences or private context here — only what's relevant to the team.`

func buildSummaryPrompt(level, period string, sources []textSource) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Level: %s\nPeriod: %s\n\nSource texts:\n", level, period)
	for _, s := range sources {
		fmt.Fprintf(&sb, "\n--- id: %s ---\n%s\n", s.id, s.text)
	}
	return sb.String()
}

// extractJSON pulls the first {...} block out of a model response, since
// even when instructed to return "JSON only," models sometimes wrap it in
// prose or a markdown code fence. A hardened build should use each
// provider's structured-output/tool-calling mode instead of parsing free
// text — this is a pragmatic stand-in for the sketch.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end < start {
		return s
	}
	return s[start : end+1]
}
