package store

import (
	"strings"
	"testing"
)

// TestMostRelevantFactIndexPicksClearWinner is the real hypothesis step 6
// of docs/BENCHMARK_IMPROVEMENT_PLAN.md exists to test: given several key
// facts under one summary, the one sharing the most query vocabulary
// should be identified so appendKeyFacts can promote it, instead of a
// question's answer sometimes being buried a few bullets down in
// insertion order for the model to find on its own.
func TestMostRelevantFactIndexPicksClearWinner(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
		"Melanie's daughter recently started kindergarten.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != 1 {
		t.Errorf("mostRelevantFactIndex() = %d, want 1 (the camping fact)", got)
	}
}

func TestMostRelevantFactIndexReturnsNegativeOneWhenNothingStandsOut(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie's daughter recently started kindergarten.",
	}
	queryTerms := []string{"what", "is", "the", "weather", "today"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no fact shares any real query vocabulary)", got)
	}
}

func TestMostRelevantFactIndexReturnsNegativeOneOnTie(t *testing.T) {
	facts := []string{
		"Melanie went camping at the beach.",
		"Melanie went camping in the mountains.",
	}
	queryTerms := []string{"melanie", "camping"}
	got := mostRelevantFactIndex(facts, queryTerms)
	if got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (both facts tie on shared vocabulary — no real winner to promote)", got)
	}
}

func TestMostRelevantFactIndexNoOpBelowTwoFacts(t *testing.T) {
	if got := mostRelevantFactIndex([]string{"only one fact"}, []string{"fact"}); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (nothing to reorder among a single fact)", got)
	}
	if got := mostRelevantFactIndex(nil, []string{"fact"}); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no facts at all)", got)
	}
}

func TestMostRelevantFactIndexNoOpWithoutQueryTerms(t *testing.T) {
	facts := []string{"Melanie went camping.", "Melanie enjoys painting."}
	if got := mostRelevantFactIndex(facts, nil); got != -1 {
		t.Errorf("mostRelevantFactIndex() = %d, want -1 (no query terms to rank against)", got)
	}
}

// TestSummaryCitationSnippetIncludesMostRelevantFact confirms the
// citation snippet (docs/ANSWER_CITATIONS_PLAN.md) reuses the same
// mostRelevantFactIndex ranking appendKeyFacts already applies to the
// injected context — a citation should never disagree with what was
// actually promoted for the model to see.
func TestSummaryCitationSnippetIncludesMostRelevantFact(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := summaryCitationSnippet("Melanie's hobbies and travels.", facts, queryTerms)
	want := "Melanie's hobbies and travels.\n  - (most relevant) Melanie has camped at the beach, in the mountains, and in the forest."
	if got != want {
		t.Errorf("summaryCitationSnippet() = %q, want %q", got, want)
	}
}

// TestSummaryCitationSnippetIsProseOnlyWithoutAClearWinner confirms no
// fabricated "(most relevant)" label when nothing actually stands out —
// mirrors appendKeyFacts' own no-reordering behavior in that case.
func TestSummaryCitationSnippetIsProseOnlyWithoutAClearWinner(t *testing.T) {
	got := summaryCitationSnippet("Melanie's hobbies and travels.", nil, []string{"melanie"})
	want := "Melanie's hobbies and travels."
	if got != want {
		t.Errorf("summaryCitationSnippet() = %q, want %q", got, want)
	}
}

// TestGuaranteedFactPrefersMostRelevant is the real behavior
// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 1 exists for:
// whatever guaranteedFact returns is protected from summaryDepthCap's
// truncation, so it must be the single most useful thing to keep, not
// just the first fact in insertion order.
func TestGuaranteedFactPrefersMostRelevant(t *testing.T) {
	facts := []string{
		"Melanie enjoys painting landscapes in her free time.",
		"Melanie has camped at the beach, in the mountains, and in the forest.",
	}
	queryTerms := []string{"where", "has", "melanie", "camped"}
	got := guaranteedFact("Melanie's hobbies and travels.", facts, queryTerms)
	want := "Melanie has camped at the beach, in the mountains, and in the forest."
	if got != want {
		t.Errorf("guaranteedFact() = %q, want the most relevant fact %q", got, want)
	}
}

// TestGuaranteedFactFallsBackToFirstFactWithoutAClearWinner mirrors
// mostRelevantFactIndex's own no-clear-winner behavior — still guarantee
// *a* fact over nothing, just not one fabricated as "most relevant."
func TestGuaranteedFactFallsBackToFirstFactWithoutAClearWinner(t *testing.T) {
	facts := []string{"Melanie went camping at the beach.", "Melanie went camping in the mountains."}
	got := guaranteedFact("Melanie's hobbies and travels.", facts, []string{"melanie", "camping"})
	if got != facts[0] {
		t.Errorf("guaranteedFact() = %q, want the first fact %q when nothing stands out", got, facts[0])
	}
}

// TestGuaranteedFactFallsBackToProseWithNoFactsAtAll confirms a summary
// with nothing grounded still guarantees *something* (a short prose
// snippet) rather than an empty guarantee.
func TestGuaranteedFactFallsBackToProseWithNoFactsAtAll(t *testing.T) {
	prose := "Melanie's hobbies and travels."
	got := guaranteedFact(prose, nil, []string{"melanie"})
	if got != prose {
		t.Errorf("guaranteedFact() = %q, want the prose itself %q (short enough not to need truncating)", got, prose)
	}
}

// TestGuaranteedFactTruncatesLongProseFallback confirms the no-facts
// fallback is itself bounded — a guarantee that's allowed to be
// unbounded would defeat summaryDepthCap's whole purpose.
func TestGuaranteedFactTruncatesLongProseFallback(t *testing.T) {
	longProse := strings.Repeat("x", guaranteedProseFallbackChars+500)
	got := guaranteedFact(longProse, nil, nil)
	if len(got) <= guaranteedProseFallbackChars || !strings.Contains(got, "truncated") {
		t.Errorf("guaranteedFact() with no facts and long prose wasn't truncated, len=%d", len(got))
	}
}

// TestDepthTextIncludesProseAndAllFacts confirms the bounded "extra
// depth" section still carries the full picture (prose + every fact,
// most-relevant marked) — summaryDepthCap bounds its *length*, not its
// content; depthText itself should still build the complete picture for
// the caller to truncate.
func TestDepthTextIncludesProseAndAllFacts(t *testing.T) {
	facts := []string{"fact one", "fact two"}
	got := depthText("the prose", facts, nil)
	if !strings.Contains(got, "the prose") || !strings.Contains(got, "fact one") || !strings.Contains(got, "fact two") {
		t.Errorf("depthText() = %q, want it to contain the prose and every fact", got)
	}
}
