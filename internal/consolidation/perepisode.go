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
const perEpisodeFactPrompt = `You are looking at a single conversation exchange for anything worth remembering as a standalone, checkable fact — something a future, unrelated question might ask about specifically: a purchase, a decision, a preference, an event attended or participated in, a date or recency something started, was downloaded, was tried, or changed, or a specific number or amount.

Most exchanges have nothing like this — that's expected and normal. Only report a fact if the exchange actually states one directly; do not infer, generalize, or guess beyond what's said. Each fact should be self-contained enough to be understood on its own, without needing to see the rest of this exchange.

Be exhaustive, not selective: a single exchange can contain several unrelated reportable facts (e.g. one about something the user just started doing, and a separate one about an unrelated detail mentioned later) — list every one you find, not just the most prominent. Pay particular attention to incidental scene-setting remarks early in the exchange ("I just started X", "I've been doing Y for N months") — these are exactly the kind of fact a later question is likely to ask about, even though they don't look like the main topic of the exchange.

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
// Real, deliberate cost tradeoff: one extra LLM call per episode, only
// on days already past clusterEpisodeThreshold (see generateDailySummary's
// own call site) — doubles episode-processing cost on exactly the days
// already paying for clustering's own extra calls, the same tradeoff
// this document's own Phase B writeup already named and accepted.
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
func (r *Runner) extractPerEpisodeFacts(ctx context.Context, sources []textSource) []KeyFactOutput {
	var facts []KeyFactOutput
	for _, s := range sources {
		req := provider.ChatRequest{
			Messages: []provider.Message{
				{Role: provider.RoleSystem, Content: perEpisodeFactPrompt},
				{Role: provider.RoleUser, Content: s.text},
			},
		}
		resp, err := r.consolidation.ChatCompletion(ctx, req)
		if err != nil {
			slog.Warn("consolidation: per-episode fact extraction call failed, skipping this episode", "episode", s.id, "error", err)
			continue
		}
		var parsed struct {
			Facts []string `json:"facts"`
		}
		if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
			slog.Warn("consolidation: malformed per-episode fact extraction response, skipping this episode", "episode", s.id, "error", err)
			continue
		}
		for _, f := range parsed.Facts {
			if strings.TrimSpace(f) == "" {
				continue
			}
			facts = append(facts, KeyFactOutput{Fact: f, SourceEpisodeIDs: []string{s.id}})
		}
	}
	return facts
}
