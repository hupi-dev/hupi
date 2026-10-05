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
    {"id": "kind:slug", "kind": "person|project|preference|skill|place|organization", "name": "...", "attributes": {"key": "value"}, "supersedes_keys": ["existing_key_name", ...]}
  ],
  "relationships": [
    {"subject_kind": "person|project|preference|skill|place|organization", "subject_name": "...", "predicate": "a short verb phrase, e.g. works_at, married_to, friends_with, manages, lives_in (a person's place of residence — always use lives_in for this, not based_in or resides_in)", "object_kind": "person|project|preference|skill|place|organization", "object_name": "...", "valid_from": "YYYY-MM-DD or empty string if unknown", "valid_until": "YYYY-MM-DD or empty string if still current"}
  ]
}

Only include a key_fact if it is directly and specifically supported by the source texts you were given. Cite the exact source ids it came from. Do not include anything you are inferring, generalizing, or guessing beyond what the source text states.

What is worth a key_fact is broader than real-world events the user reports. Give every source its own consideration, and do not skip a source because its topic seems minor, playful, hypothetical, or unlike the rest of the day. In particular, when the assistant side of a conversation produced something for the user — a story, poem, children's book, plan, itinerary, recipe, list of recommendations, or explanation — the specific details inside it are exactly what a later "remind me what you told me about..." question will ask for: names, titles, colors, numbers, quantities, the order of chapters or steps, and which specific items were recommended. Extract those as key_facts too. Some sources contain an earlier conversation as a transcript with lines prefixed "user:" and "assistant:" — text on an "assistant:" line is assistant-written in exactly the same way. Always state where such a detail came from, so it can never be mistaken for a real-world fact or for something the user said: write "In the bedtime story the assistant wrote for the user, the dragon is named Ember and lives under a waterfall," not "The dragon is named Ember." This does not relax the rule above: the detail must still appear explicitly in the cited source text. You are recording what was written, not judging whether it is true.

The same attribution discipline applies to a source containing a bracketed tag like "[Attached file: resume.pdf]" or "[Shared image: vacation.jpg]" immediately before a block of text — that block is content HUPI itself derived from a file or image the user shared (extracted document text, or a vision model's description of a photo), not something the user typed. Treat it with the same evidentiary weight as the user's own statement (the user did share that file/image), but attribute it using the tag's own filename: write "The resume the user uploaded (resume.pdf) lists 5 years at Acme Corp," or "The photo the user shared (vacation.jpg) shows a beach at sunset," not a bare claim that could be mistaken for something the user typed directly — the same pattern already used above for assistant-written content.

When a source describes several different dated milestones about one underlying story, case, or project — for example, when something began, when an agreement was signed, when it was completed, when a decision was issued — extract each milestone as its own separate key_fact, and name the specific milestone in the fact text itself (e.g. "the construction began in 2014," not just "2014" or "the case happened in 2014"). Do not let a passage's most memorable or most recent date stand in for all of them, and do not drop an earlier milestone in favor of a later one — a reader asking specifically "when did X begin" needs the begin date preserved as its own fact, distinct from when it was signed, completed, or decided.

An entity's attributes are for stable, singular facts about it — a name, a preference, a role, a value that has one current state at a time. A repeating or countable TYPE of event for an entity (a tournament win, a trip, a purchase, a visit) is different: every occurrence of it must be recorded as its own key_fact, every time it comes up, even if you also update a summary attribute about the entity's current state regarding it (e.g. "most_recent_tournament_result"). Never invent a new attribute key to count or track individual occurrences of a repeating event (e.g. "fourth_tournament_win_date", "third_trip_location") — that information belongs in key_facts, which already accumulates one row per occurrence and is the only place a later "how many times has X happened" question can be answered from completely. An attribute that only exists to number one occurrence among several, with no key_fact backing it, is worse than not recording it at all: it looks like a verified standalone fact but is neither checkable nor countable the way key_facts are.

The user often states a fact about themselves by asking the assistant to recall it ("remember when I got pre-approved for $400,000 from Wells Fargo?") — this is a real, direct statement of that fact by the user, report it exactly as you would a plain declarative statement. Do not add a qualifier like "previously," "in an earlier conversation," or "as mentioned before" unless a source itself states when or where it was previously discussed — within a single exchange, the assistant typically has no actual memory of any earlier conversation to confirm that framing against, so adding it states something the source doesn't actually support, even though the underlying fact itself is real and worth keeping. Write "The user was pre-approved for $400,000 from Wells Fargo," not "The user previously mentioned being pre-approved for $400,000 from Wells Fargo."

supersedes_keys is optional and almost always empty — omit it entirely unless a KNOWN ENTITIES block below lists this same entity with an existing attribute key that a source text explicitly updates under a NEW key name in this response's own attributes. Only name a key there when you're confident it's the same specific real-world fact being updated (e.g. a mortgage pre-approval amount changing), not merely a different fact about the same entity — when in doubt, leave supersedes_keys empty and let the new attribute sit alongside the old one rather than risk discarding a fact that was actually still true.

A source text speaks as of its own labeled date (the "(date: YYYY-MM-DD)" shown after that source's id, when present), not today. When a source uses a relative time reference — "yesterday", "last week", "next month", "this morning" — resolve it against THAT source's own date and write the resulting absolute date (YYYY-MM-DD) into the summary and any key_fact that states it, instead of repeating the relative phrase. For example, a source dated 2023-05-08 saying "I lost my job yesterday" becomes a key_fact stating the job loss happened on 2023-05-07, not one that says "lost his job yesterday" — a relative phrase written into a summary today becomes meaningless the next time anyone reads it. If a source has no labeled date, leave its own relative phrasing as-is rather than guessing what date it means.

Only include a relationship if the source texts directly and specifically state a connection between two entities — the same evidentiary bar as a key_fact. Leave valid_from/valid_until as empty strings rather than guessing a date the source text doesn't state.

CRITICAL constraint on relationships: subject_kind/subject_name and object_kind/object_name MUST exactly match the kind and name of an entry you also placed in entities_touched in this same response — same kind value, same name spelling, character for character. A relationship whose subject or object doesn't appear in entities_touched will be silently dropped, so before including any relationship, re-check that both sides are already listed in entities_touched; if either one isn't a real entity worth recording on its own, add it there first (or leave the relationship out entirely). For example, if you write a relationship with object_name "her husband", entities_touched must contain a person entity named exactly "her husband" (or you should give that person's real name in both places if the source text states it) — a vague reference that only appears inside the relationship and nowhere in entities_touched will not be recorded.`

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
//
// knownEntities (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase C
// sub-problem 1), when non-empty, is every existing entity whose name
// appears somewhere in sources, with its current attributes
// (findKnownEntities) — the same "here's what's already on record"
// context establishedRecord already gives for a day's own prose,
// extended to entity attributes so the model can recognize a new
// attribute as an update to an existing one under a different key name
// (EntityUpdate.SupersedesKeys) instead of writing a second, differently
// -named key that leaves the stale one sitting there forever.
func buildSummaryPrompt(level, period string, sources []textSource, establishedRecord string, knownEntities []knownEntityContext) string {
	var sb strings.Builder
	if establishedRecord != "" {
		fmt.Fprintf(&sb, "ALREADY-ESTABLISHED RECORD for this %s — the current, reviewed summary before this re-consolidation, which may already incorporate one or more deliberate human corrections not visible anywhere in the raw source texts below. Treat every fact in it as settled and correct. Only change something from it if a source text below EXPLICITLY states a new fact that supersedes it (the user or a later message clearly states a value changed, a decision was reversed, etc). Do NOT contradict, doubt, or walk back anything here merely because a raw source phrases something differently, states an earlier value in passing, or because your own past response in a source text differs from it — this record already reflects the outcome of any corrections that were made, even when the raw sources don't show that correction happening.\n\n%s\n\n", level, establishedRecord)
	}
	if known := formatKnownEntities(knownEntities); known != "" {
		fmt.Fprintf(&sb, "KNOWN ENTITIES already on record whose name appears in the source texts below, with their current attributes — if a source text below restates one of these entities' facts using a different attribute key name than shown here (e.g. the record already has \"preapproval_amount\" and a source states a new pre-approval amount), name that existing key in supersedes_keys for the corresponding entities_touched entry so the old, differently-spelled key doesn't stay stale. Only do this when a source text states an update to the SAME specific real-world fact — a source stating a different, unrelated fact about the same entity is not a supersession, just an additional attribute.\n\n%s\n", known)
	}
	fmt.Fprintf(&sb, "Level: %s\nPeriod: %s\n\nSource texts:\n", level, period)
	for _, s := range sources {
		if s.date != "" {
			fmt.Fprintf(&sb, "\n--- id: %s (date: %s) ---\n%s\n", s.id, s.date, s.text)
		} else {
			fmt.Fprintf(&sb, "\n--- id: %s ---\n%s\n", s.id, s.text)
		}
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
