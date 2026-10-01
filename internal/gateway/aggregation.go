package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"hupi/internal/provider"
)

// aggregationExtractionSystemPrompt is this pass's one real LLM call
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md, Gap 4 mechanism 2 follow-up)
// — deliberately narrow, matching the pattern that's worked everywhere
// else in this investigation (per-episode extraction, grounding checks,
// contradiction detection): ask the model only to find and list what's
// relevant, not to also pair, order, or compute anything. The two
// earlier prompt-only fix attempts against the real charity-events case
// both asked a single call to do perception, pairing, and arithmetic at
// once and both showed zero measured effect — this narrows the model's
// job to the one part it's actually reliable at.
const aggregationExtractionSystemPrompt = `You will be given a question that requires comparing, ordering, or counting multiple dated events, and a block of retrieved context that may contain the relevant facts. Extract every fact in the context that states a specific date and is relevant to answering the question. Respond with exactly one JSON object, nothing else, no markdown fences:

{"facts": [{"description": "<a single concrete, self-contained fact>", "date": "YYYY-MM-DD"}, ...]}

Be exhaustive, not selective: the context may mention several similar-sounding events on different dates, and the question may depend on a specific pair or subset of them — list every one you find, even ones that seem redundant or only mentioned in passing, rather than stopping once you've found one or two. Missing a relevant date here means the question can't be answered correctly later, so err toward including a borderline case rather than omitting it.

Only include a fact if the context actually states a specific calendar date for it — do not infer, guess, or resolve a relative date ("last week") yourself. If fewer than two such facts exist, respond with {"facts": []}.`

func buildAggregationExtractionPrompt(question, contextMessage string) string {
	return fmt.Sprintf("Question: %s\n\nRetrieved context:\n%s", question, contextMessage)
}

type aggregationFact struct {
	Description string `json:"description"`
	Date        string `json:"date"`
}

// extractAggregationFacts is this pass's only LLM call — same safe-degrade
// direction as groundingCheck/attributionCheck: a call failure or an
// unparseable/malformed response degrades to "no facts extracted," which
// resolveAggregationHint already treats as "nothing to add," never an
// error that blocks the turn.
func extractAggregationFacts(ctx context.Context, judge provider.Provider, question, contextMessage string) []aggregationFact {
	resp, err := judge.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: aggregationExtractionSystemPrompt},
			{Role: provider.RoleUser, Content: buildAggregationExtractionPrompt(question, contextMessage)},
		},
	})
	if err != nil {
		slog.Warn("gateway: aggregation fact extraction call failed, skipping aggregation hint", "error", err)
		return nil
	}

	var parsed struct {
		Facts []aggregationFact `json:"facts"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
		slog.Warn("gateway: malformed aggregation fact extraction response, skipping aggregation hint", "error", err)
		return nil
	}
	return parsed.Facts
}

// parsedAggregationFact is aggregationFact with its date actually parsed
// — resolveAggregationHint only ever works with facts whose date the
// model stated in a form it can trust, never a guess.
type parsedAggregationFact struct {
	description string
	date        time.Time
}

func parseAggregationFacts(facts []aggregationFact) []parsedAggregationFact {
	var parsed []parsedAggregationFact
	for _, f := range facts {
		if strings.TrimSpace(f.Description) == "" {
			continue
		}
		d, err := time.Parse("2006-01-02", f.Date)
		if err != nil {
			continue
		}
		parsed = append(parsed, parsedAggregationFact{description: f.Description, date: d})
	}
	return parsed
}

// consecutiveKeywords/orderKeywords mirror internal/store/retrieve.go's
// own orderingKeywords convention (cheap, deliberately generous substring
// match) — this pass only needs to distinguish its two real output
// shapes (a specific adjacent pair vs. a full ordered list), not
// re-detect whether the question is ordering-shaped at all (Handler
// already gates this on RetrievalResult.NeedsAggregationPass).
var consecutiveKeywords = []string{"in a row", "consecutive", "back to back"}

func looksLikeConsecutivePairRequest(question string) bool {
	lower := strings.ToLower(question)
	for _, kw := range consecutiveKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// resolveAggregationHint is the deterministic half of this pass — pure
// code, no LLM judgment, the same "shift work out of the LLM and into
// code" approach relativeDateLabel already proved for the single-fact
// case (docs/CONSOLIDATION_COMPLETENESS_PLAN.md). Needs at least 2 dated
// facts to say anything at all — with 0 or 1, either there's nothing to
// combine, or the existing single-fact date-label mechanism already
// covers it, so this returns "" and the turn proceeds exactly as if this
// pass didn't exist.
//
// Only handles the "consecutive pair" shape — real-verified removal of
// the "order" shape this used to also cover (resolveOrderHint, now
// deleted): a real "order of the sports events I watched in January"
// question had its extraction step miss one of three real facts (the
// same already-known ~50-65% per-call extraction reliability), producing
// an incomplete chronological-order hint. The answering model deferred
// to that incomplete hint wholesale instead of using the full context —
// which, by then, writeKeyFacts' own relevance-ranking fix (the same
// investigation, built right before this one) already surfaced reliably
// on its own (15/15 real runs correct with no hint at all). The hint
// text's own "use this only if it directly answers the question"
// caveat didn't save it: the model used an incomplete list anyway. For
// "order," ranking alone already solves the problem this pass set out to
// help with; injecting a hint that can silently omit an event is a net
// negative there, not a redundant-but-harmless extra. The "pair" shape
// is different and keeps real value: ranking can surface all the right
// facts prominently but can't itself identify *which two* form the
// specific adjacent pair a "consecutive days" question needs — that
// still requires this pass's own resolution step.
func resolveAggregationHint(question string, facts []parsedAggregationFact, now time.Time) string {
	if len(facts) < 2 || !looksLikeConsecutivePairRequest(question) {
		return ""
	}
	sorted := make([]parsedAggregationFact, len(facts))
	copy(sorted, facts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].date.Before(sorted[j].date) })
	return resolveConsecutivePairHint(sorted, now)
}

// maxConsecutivePairGapDays bounds how far apart two dates can be and
// still plausibly be called "in a row" / "consecutive days" — real,
// measured need (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): the first
// version of this function reported whatever pair happened to be
// closest among the extracted facts regardless of the actual gap,
// surfacing a real 16-day-apart pair as "the closest pair of matching
// events" for a question that explicitly said "consecutive days" — a
// hint the answering model correctly recognized as not actually
// satisfying the question and ignored, but which would mislead a less
// careful model, or simply waste the one thing this pass was supposed
// to hand over pre-verified. A small buffer above exactly 1 day, not a
// stricter ==1 check, since consolidation's own daily-summary dates can
// occasionally be off by a day from the source conversation's real
// timestamp (see this repo's own established tolerance elsewhere for
// consolidation-derived dates).
const maxConsecutivePairGapDays = 2

// resolveConsecutivePairHint finds the closest adjacent pair among
// sorted facts (by construction, sorted is already date-ascending) and
// computes the elapsed time from that pair's later date to now — the
// real, motivating shape (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
// "how many months since I participated in two charity events in a
// row?" needs exactly this (identify the pair, then compute the gap to
// now), not the model doing both while also reading a busy context.
// Returns "" if the closest available pair still isn't close enough to
// plausibly be "consecutive" — an honest no-hint is safer than a
// confidently wrong one (same direction as every other best-effort pass
// in this codebase: degrade to nothing, never mislead).
func resolveConsecutivePairHint(sorted []parsedAggregationFact, now time.Time) string {
	bestI, bestGap := -1, time.Duration(math.MaxInt64)
	for i := 0; i+1 < len(sorted); i++ {
		gap := sorted[i+1].date.Sub(sorted[i].date)
		if gap < bestGap {
			bestI, bestGap = i, gap
		}
	}
	if bestI < 0 {
		return ""
	}
	a, b := sorted[bestI], sorted[bestI+1]
	gapDays := int(bestGap.Hours() / 24)
	if gapDays > maxConsecutivePairGapDays {
		return ""
	}
	sinceLater := relativeDelta(b.date, now)
	if sinceLater == "" {
		return ""
	}
	return fmt.Sprintf(
		"A separate computation over the retrieved facts found a pair of matching events on consecutive days: %q on %s and %q on %s (%d day(s) apart). As of now, the later of those two was %s. Use this only if it directly answers the question — otherwise ignore it.",
		a.description, a.date.Format("2006-01-02"), b.description, b.date.Format("2006-01-02"), gapDays, sinceLater,
	)
}

// relativeDelta is a small, self-contained "N weeks/months/years before
// now" computation — independently implemented from
// internal/store/temporal.go's own relativeDateLabel rather than shared
// across packages (same tradeoff internal/consolidation/period.go's own
// doc comment already accepts: both are small and stable, and the
// cross-package coupling importing one from the other would need
// outweighs deduplicating roughly a dozen lines). Empty string for a
// future or same-day date — not a real "ago" case this hint needs to
// state.
func relativeDelta(start, now time.Time) string {
	days := int(now.Sub(start).Hours() / 24)
	switch {
	case days < 0:
		return ""
	case days == 0:
		return "today"
	case days == 1:
		return "1 day ago"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	case days < 60:
		return pluralAgo(math.Round(float64(days)/7), "week")
	default:
		if months := math.Round(float64(days) / 30.44); months < 12 {
			return pluralAgo(months, "month")
		}
		return pluralAgo(math.Round(float64(days)/365.25), "year")
	}
}

func pluralAgo(n float64, unit string) string {
	count := int(n)
	if count <= 0 {
		count = 1
	}
	if count == 1 {
		return fmt.Sprintf("1 %s ago", unit)
	}
	return fmt.Sprintf("%d %ss ago", count, unit)
}

// AggregationHint is this pass's public entry point — Handler calls it
// whenever RetrievalResult.NeedsAggregationPass is true (Gap 4 mechanism
// 2, docs/CONSOLIDATION_COMPLETENESS_PLAN.md). That retrieval-side gate
// is deliberately broader than what this pass actually acts on (it also
// covers "order of" questions, still used to widen retrieval breadth —
// Phase D item 2 — independent of this pass) — so check the narrower
// shape this pass itself helps with *before* paying for the extraction
// call, not just when deciding what to do with its result. Otherwise:
// extract, then resolve. Best-effort throughout — any failure anywhere
// in this pipeline returns "", and the turn proceeds exactly as it would
// have without this pass, never blocking or degrading the primary
// answer.
func AggregationHint(ctx context.Context, judge provider.Provider, question, contextMessage string, now time.Time) string {
	if !looksLikeConsecutivePairRequest(question) {
		return ""
	}
	facts := extractAggregationFacts(ctx, judge, question, contextMessage)
	parsed := parseAggregationFacts(facts)
	hint := resolveAggregationHint(question, parsed, now)

	// HUPI_DEBUG_AGGREGATION is a real, permanent diagnostic escape
	// hatch, same pattern and same reasoning as internal/store/retrieve.go's
	// HUPI_DEBUG_FUSION — added while real-verifying this pass against
	// the actual charity-events LongMemEval case specifically because
	// there was no way to see what got extracted or why a hint did or
	// didn't come out without it. Off by default, zero cost when unset.
	if os.Getenv("HUPI_DEBUG_AGGREGATION") != "" {
		fmt.Fprintf(os.Stderr, "AGGREGATION_DEBUG raw_facts=%d parsed_facts=%d\n", len(facts), len(parsed))
		for _, f := range parsed {
			fmt.Fprintf(os.Stderr, "AGGREGATION_DEBUG fact date=%s description=%q\n", f.date.Format("2006-01-02"), f.description)
		}
		fmt.Fprintf(os.Stderr, "AGGREGATION_DEBUG hint=%q\n", hint)
	}
	return hint
}
