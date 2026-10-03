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
//
// Verdicts are index-tagged ({"i": N, "ok": bool}), not a bare positional
// boolean array. A live reproduction against the real gpt-4.1 grounding
// profile (see TestLiveGroundingCheckReproducesBatchMismatchWithRealData)
// confirmed the model reliably returns one extra or one missing verdict
// for some real, varied 20-fact batches (synthetic placeholder facts did
// not reproduce it) — a bare array has no way to recover from that short
// of discarding the whole batch, which is what groundingCheckOne used to
// do. Tagging each verdict with the fact number it judges lets
// groundingCheckOne realign by index and salvage every fact whose index
// resolves cleanly instead.
const groundingSystemPrompt = `You are a fact-checker. You will be given source text and a numbered list of claimed facts. For each fact, in order, decide whether the source text actually, specifically supports it — not just plausible, not just related, but stated. Respond with exactly one JSON object, nothing else: {"grounded": [{"i": 1, "ok": true}, {"i": 2, "ok": false}, ...]} — one entry per fact, "i" matching that fact's number in the list above, "ok" the true/false verdict. Return exactly one entry per fact number, 1 through the last number shown — never merge two fact numbers into one entry, never split one fact number across two entries, never add an entry for a number that wasn't listed.

A source's own labeled date ("(date: YYYY-MM-DD)" after its id, when present) is part of what it states. If a fact gives an absolute date (e.g. "2023-05-07") that is the correct resolution of a relative reference in that labeled source ("yesterday", "last week", etc. relative to that source's own date), treat the date as grounded — the source is stating that day, just not spelling out the calendar date itself. Only mark it ungrounded if the resolution is wrong (the arithmetic doesn't match the source's own date) or the source has no date label to resolve against at all.

A user often states a fact about themselves by asking the assistant to recall it ("remember when I got pre-approved for $400,000 from Wells Fargo?") — the user's own question is itself a direct statement of that fact, grounded regardless of how the assistant responds. In particular, when the assistant's reply denies having access to earlier conversations or says it doesn't recall (a normal, expected reply when the assistant genuinely has no memory of prior sessions), that denial does not undermine or contradict the fact — it is not evidence against what the user just stated. Mark such a fact grounded on the strength of the user's own statement alone.`

func buildGroundingPrompt(sourceText string, facts []KeyFactOutput) string {
	var sb strings.Builder
	sb.WriteString("Source text:\n" + sourceText + "\n\nClaimed facts:\n")
	for i, f := range facts {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, f.Fact)
	}
	return sb.String()
}

// groundingCheckBatchSize caps how many facts go into one grounding call.
// Real, measured failure mode (docs/CONSOLIDATION_COMPLETENESS_PLAN.md,
// per-episode fact extraction verification): busy days that combine
// clustering with per-episode extraction can produce 70-100+ facts for a
// single summary, and the count-mismatch degrade below — which discards
// every fact's grounding, not just the miscounted one — was observed to
// trigger on *every* real attempt at that volume, not as a rare fluke.
// Splitting into fixed-size batches keeps each individual call's list
// short enough for the model to reliably echo back one boolean per fact,
// and confines any remaining mismatch to that one batch instead of the
// whole summary.
const groundingCheckBatchSize = 20

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
	if len(facts) > groundingCheckBatchSize {
		all := make([]bool, 0, len(facts))
		for start := 0; start < len(facts); start += groundingCheckBatchSize {
			end := start + groundingCheckBatchSize
			if end > len(facts) {
				end = len(facts)
			}
			batch, err := r.groundingCheckOne(ctx, sourceText, facts[start:end])
			if err != nil {
				return nil, err
			}
			all = append(all, batch...)
		}
		return all, nil
	}
	return r.groundingCheckOne(ctx, sourceText, facts)
}

// groundingVerdict is one entry of the model's index-tagged response —
// see groundingSystemPrompt's own doc comment for why a bare positional
// boolean array isn't trusted here.
type groundingVerdict struct {
	Index int  `json:"i"`
	OK    bool `json:"ok"`
}

// indexVerdicts parses the raw model response into a map keyed by fact
// number (1-based, matching buildGroundingPrompt's numbering). A
// duplicate index keeps the first value seen — treated the same as any
// other oddity the model produces: not fatal, just something
// groundingCheckOne's own completeness check below will likely flag as
// imperfect and retry.
func indexVerdicts(content string) (map[int]bool, error) {
	var result struct {
		Grounded []groundingVerdict `json:"grounded"`
	}
	if err := json.Unmarshal([]byte(extractJSON(content)), &result); err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(result.Grounded))
	for _, v := range result.Grounded {
		if _, seen := out[v.Index]; !seen {
			out[v.Index] = v.OK
		}
	}
	return out, nil
}

// groundingCheckOne is the single-call implementation groundingCheck
// batches on top of. It tolerates the model returning a verdict count
// that doesn't match the fact count — a real, reproduced failure mode
// (see TestLiveGroundingCheckReproducesBatchMismatchWithRealData: a live
// call against real facts and real source text came back with 21
// verdicts for 20 facts) — by realigning on the index each verdict is
// tagged with instead of positional order, salvaging every fact whose
// index resolves cleanly rather than discarding the whole batch the way
// a bare positional array forced.
func (r *Runner) groundingCheckOne(ctx context.Context, sourceText string, facts []KeyFactOutput) ([]bool, error) {
	verdicts, err := r.groundingCheckOneAttempt(ctx, sourceText, facts)
	if err != nil {
		return nil, err
	}
	if !verdicts.complete(len(facts)) {
		// Belt-and-suspenders: a mismatch isn't always perfectly
		// deterministic (same batch, same model, re-asked), so retry
		// once before falling back to per-index salvage — cheap, since
		// this path is the rare case, not the common one.
		slog.Warn("consolidation: grounding check response didn't cleanly index every fact, retrying batch once",
			"facts", len(facts), "indexed", len(verdicts))
		retried, err := r.groundingCheckOneAttempt(ctx, sourceText, facts)
		if err == nil {
			verdicts = retried
		} else {
			slog.Warn("consolidation: grounding check retry failed, salvaging first attempt", "error", err)
		}
	}
	return verdicts.resolve(facts), nil
}

type groundingVerdicts map[int]bool

func (v groundingVerdicts) complete(n int) bool {
	for i := 1; i <= n; i++ {
		if _, ok := v[i]; !ok {
			return false
		}
	}
	return true
}

// resolve turns a (possibly incomplete) index->verdict map into exactly
// len(facts) booleans, one per fact, in fact order. A fact whose index
// never appeared in the model's response is forced to false — the same
// safe direction this package already treats a genuine "no" as: an
// ungrounded fact is still stored (storeSummary), just excluded from
// retrieval until reviewed, never destroyed. Unlike the previous
// all-or-nothing behavior, every other fact in the same batch whose
// index DID resolve keeps its real verdict.
func (v groundingVerdicts) resolve(facts []KeyFactOutput) []bool {
	out := make([]bool, len(facts))
	for i := range facts {
		factNum := i + 1
		ok, found := v[factNum]
		if !found {
			slog.Warn("consolidation: grounding check had no verdict for this fact number, treating as ungrounded",
				"fact_number", factNum)
			metrics.GroundingSalvageTotal.WithLabelValues("unindexed").Inc()
			out[i] = false
			continue
		}
		metrics.GroundingSalvageTotal.WithLabelValues("matched").Inc()
		out[i] = ok
		if ok {
			metrics.GroundingFactsTotal.WithLabelValues("true").Inc()
		} else {
			metrics.GroundingFactsTotal.WithLabelValues("false").Inc()
		}
	}
	return out
}

func (r *Runner) groundingCheckOneAttempt(ctx context.Context, sourceText string, facts []KeyFactOutput) (groundingVerdicts, error) {
	resp, err := r.grounding.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: groundingSystemPrompt},
			{Role: provider.RoleUser, Content: buildGroundingPrompt(sourceText, facts)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("grounding LLM call: %w", err)
	}
	verdicts, err := indexVerdicts(resp.Message.Content)
	if err != nil {
		// Couldn't parse at all — same empty-map behavior as any other
		// index that never resolves: every fact falls to resolve's
		// unindexed/false path, not a hard failure that would discard
		// the whole day's otherwise-good summary.
		slog.Warn("consolidation: could not parse grounding result", "error", err)
		return groundingVerdicts{}, nil
	}
	return verdicts, nil
}
