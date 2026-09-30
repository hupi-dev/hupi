package store

import "testing"

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
