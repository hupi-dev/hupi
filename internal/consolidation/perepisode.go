package consolidation

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"hupi/internal/provider"
)

// perEpisodeFactPrompt is deliberately narrow — a single yes/no-shaped
// judgment per episode ("does this contain a standalone fact"), not full
// consolidation. Kept separate from summarySystemPrompt so this pass
// stays cheap and fast per call, matching contradictionCheckPrompt's own
// reasoning for staying narrow rather than reusing the bigger prompt.
const perEpisodeFactPrompt = `You are looking at a single conversation exchange for anything worth remembering as a standalone, checkable fact — something a future, unrelated question might ask about specifically: a purchase, a decision, a preference, an event attended or participated in, a date or recency something started, was downloaded, was tried, or changed, a specific number or amount, or a specific detail inside something the assistant produced for the user (a story, plan, recipe, recommendation list, or explanation): a name, title, color, number, the order of steps or chapters, or exactly which items were recommended. Some exchanges contain an earlier conversation as a transcript with lines prefixed "user:" and "assistant:" — text on an "assistant:" line is assistant-written in exactly the same way.

Small talk and generic back-and-forth often have nothing like this — that's expected and normal. Only report a fact if the exchange actually states one directly; do not infer, generalize, or guess beyond what's said. Each fact should be self-contained enough to be understood on its own, without needing to see the rest of this exchange. For a detail drawn from something the assistant wrote, always state where it came from, so it can never be mistaken for a real-world fact: "In the children's book the assistant wrote, the Plesiosaur has a blue scaly body," not "The Plesiosaur has a blue scaly body." For a long piece of assistant-written content, list its distinctive specific details (typically up to about 8), not a restatement of the whole thing.

The same attribution discipline applies to a bracketed tag like "[Attached file: resume.pdf]" or "[Shared image: vacation.jpg]" immediately before a block of text — that block is content HUPI itself derived from a file or image the user shared (extracted document text, or a vision model's description of a photo), not something the user typed. Report it with the same evidentiary weight as the user's own statement (the user did share that file/image), attributed using the tag's own filename: "The resume the user uploaded (resume.pdf) lists 5 years at Acme Corp," or "The photo the user shared (vacation.jpg) shows a beach at sunset," not a bare claim that could be mistaken for something the user typed directly.

The user often states a fact about themselves by asking the assistant to recall it ("remember when I got pre-approved for $400,000 from Wells Fargo?", "you remember I mentioned I started the new job in March, right?") — this is a real, direct statement of that fact by the user, report it exactly as you would a plain declarative statement. Do not add a qualifier like "previously," "in an earlier conversation," or "as mentioned before" unless the exchange itself states when or where it was previously discussed — within a single exchange, the assistant typically has no actual memory of any earlier conversation to confirm that framing against, so adding it states something the source doesn't actually support, even though the underlying fact itself is real and worth keeping. Report "The user was pre-approved for $400,000 from Wells Fargo," not "The user previously mentioned being pre-approved for $400,000 from Wells Fargo."

Be exhaustive, not selective: a single exchange can contain several unrelated reportable facts (e.g. one about something the user just started doing, and a separate one about an unrelated detail mentioned later) — list every one you find, not just the most prominent. Pay particular attention to incidental scene-setting remarks early in the exchange ("I just started X", "I've been doing Y for N months") — these are exactly the kind of fact a later question is likely to ask about, even though they don't look like the main topic of the exchange.

When an exchange describes several different dated milestones about one underlying story, case, or project — for example, when something began, when an agreement was signed, when it was completed, when a decision was issued — report each milestone as its own separate fact, and name the specific milestone in the fact text itself (e.g. "the construction began in 2014," not just "2014" or "the case happened in 2014"). Do not let a passage's most memorable or most recent date stand in for all of them, and do not drop an earlier milestone in favor of a later one — a reader asking specifically "when did X begin" needs the begin date reported as its own fact, distinct from when it was signed, completed, or decided.

Respond with exactly one JSON object, nothing else, no markdown fences:
{"facts": ["<a single concrete, self-contained fact>", ...]}

If there's nothing worth keeping, respond with {"facts": []}.`

// extractPerEpisodeFacts runs one independent, narrow fact-extraction
// pass per episode — Phase B's second design option
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md), confirmed necessary by
// Phase D item 3's real finding that even a raised clustering cap has a
// real ceiling on especially topic-diverse days (the Ibotta case: ~26
// sessions each genuinely about a different single topic — more than
// any reasonable cluster cap can keep from crowding some of them out).
// Entirely independent of clustering's own narrative summary: every
// episode gets its own dedicated consideration regardless of which
// cluster (if any) it ended up in, or how busy its day was.
//
// Real, deliberate cost tradeoff: one extra LLM call per episode, every
// day (see generateDailySummary's own call site) — originally gated to
// days already past clusterEpisodeThreshold, extended to every day once
// two real, independent LongMemEval failures (89527b6b, 852ce960, see
// generateDailySummary's own doc comment) showed a light day's single
// combined summary call can bury a real, specific detail as a trailing
// aside just as easily as a busy day's crowding can. Bounded by
// clusterEpisodeThreshold on a light day (at most 8 extra calls), the
// same cost a busy day already pays alongside clustering's own extra
// calls — the tradeoff this document's own Phase B writeup named and
// accepted, now paid on every day instead of only busy ones.
// Best-effort per episode: a failed or malformed call for one episode is
// logged and skipped, not allowed to fail the whole day's consolidation
// over what's already a supplementary insurance pass.
//
// Facts from this pass are appended to whatever clustering separately
// produces, not deduplicated against them — some redundancy here is far
// cheaper than a missed fact, the same real tradeoff Phase C's
// contradiction-check prompt leans on ("a missed contradiction is far
// less costly than incorrectly discarding a fact that was actually still
// true").
//
// perEpisodeChunkCharLimit bounds how much text one extraction call sees
// — real, measured finding (852ce960, "...by the way, remember when I
// got pre-approved for $400,000 from Wells Fargo?" buried near the start
// of a long exchange): this pass's own reliability on a single short
// exchange (~1,400 chars) measured 10/10 in direct, repeated testing
// against the real model, but dropped to 3/5 on the same real content
// once cmd/hupi-bench's own benchmark-replay shape (an entire multi-turn
// LongMemEval session sent as one "episode," here ~11,600 characters
// across 12 turns) was the input — the exact same needle-in-a-haystack
// effect this file's sibling mechanisms (writeKeyFacts' relevance
// ranking, centeredExcerpt's dense-cluster windowing) already exist to
// counter elsewhere, recurring here at the raw-extraction-input stage.
// Chunking a long episode's text into windows at most this large and
// extracting from each independently — rather than asking one call to
// find everything worth keeping across an arbitrarily long transcript —
// measured 5/5 on the same real case once the relevant turn landed in a
// ~4,000-character chunk. A real production episode (one actual user
// exchange, not a replayed multi-session transcript) is virtually always
// well under this limit, so this only adds real extra calls on
// unusually long episodes, not the common case.
const perEpisodeChunkCharLimit = 4000

// chunkText splits text into runs of at most limit runes (not bytes —
// mirrors truncateToBudget's own rune-aware fix in internal/store, since
// a naive byte split can land mid-character on non-ASCII content).
// Deliberately simple fixed-size, non-overlapping chunks: this is an
// extraction pass, not the final assembled context, so a fact's
// supporting sentence occasionally landing split across a chunk boundary
// is an acceptable, rare cost against the real, measured gain of keeping
// each individual call's input small enough to stay reliable.
func chunkText(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}
	chunks := make([]string, 0, (len(runes)+limit-1)/limit)
	for start := 0; start < len(runes); start += limit {
		end := min(start+limit, len(runes))
		chunks = append(chunks, string(runes[start:end]))
	}
	return chunks
}

func (r *Runner) extractPerEpisodeFacts(ctx context.Context, sources []textSource) []KeyFactOutput {
	var facts []KeyFactOutput
	for _, s := range sources {
		for _, chunk := range chunkText(s.text, perEpisodeChunkCharLimit) {
			req := provider.ChatRequest{
				Messages: []provider.Message{
					{Role: provider.RoleSystem, Content: perEpisodeFactPrompt},
					{Role: provider.RoleUser, Content: chunk},
				},
			}
			resp, err := r.consolidation.ChatCompletion(ctx, req)
			if err != nil {
				slog.Warn("consolidation: per-episode fact extraction call failed, skipping this chunk", "episode", s.id, "error", err)
				continue
			}
			var parsed struct {
				Facts []string `json:"facts"`
			}
			if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
				slog.Warn("consolidation: malformed per-episode fact extraction response, skipping this chunk", "episode", s.id, "error", err)
				continue
			}
			for _, f := range parsed.Facts {
				if strings.TrimSpace(f) == "" {
					continue
				}
				facts = append(facts, KeyFactOutput{Fact: f, SourceEpisodeIDs: []string{s.id}})
			}
		}
	}
	return facts
}
