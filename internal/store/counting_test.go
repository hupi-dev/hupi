package store

import "testing"

// TestLooksLikeCountingRequestDetectsRealFailingQuestions uses the actual
// real LoCoMo questions docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md found
// undercounted — "How many video game tournaments has Nate participated
// in?" (gold nine, predicted "at least four"), "How many tournaments has
// Nate won?" (gold seven, predicted "Six"), "How many letters has Joanna
// received?" (gold two, predicted "Not mentioned").
func TestLooksLikeCountingRequestDetectsRealFailingQuestions(t *testing.T) {
	real := []string{
		"How many video game tournaments has Nate participated in?",
		"How many tournaments has Nate won?",
		"How many letters has Joanna received?",
	}
	for _, q := range real {
		if !looksLikeCountingRequest(q) {
			t.Errorf("looksLikeCountingRequest(%q) = false, want true (a real undercounted LoCoMo question)", q)
		}
	}
}

// TestLooksLikeCountingRequestExcludesElapsedTimePhrasing confirms this
// detector stays out of looksLikeOrderingRequest's own, already-working
// territory — "how many days/weeks/months/years" is an elapsed-time
// question between two known events, not a repeating-event count.
func TestLooksLikeCountingRequestExcludesElapsedTimePhrasing(t *testing.T) {
	elapsed := []string{
		"How many days passed between the two events?",
		"How many weeks ago did I start using Ibotta?",
		"How many months have passed since I joined?",
		"How many years has it been since I moved?",
	}
	for _, q := range elapsed {
		if looksLikeCountingRequest(q) {
			t.Errorf("looksLikeCountingRequest(%q) = true, want false (orderingKeywords' own elapsed-time territory, not this detector's)", q)
		}
	}
}

func TestLooksLikeCountingRequestIgnoresOrdinaryQuestions(t *testing.T) {
	ordinary := []string{
		"What database does Meridian use for its job queue?",
		"When did I start using Ibotta?",
		"What's the weather like today?",
		"Can you recommend a good book?",
	}
	for _, q := range ordinary {
		if looksLikeCountingRequest(q) {
			t.Errorf("looksLikeCountingRequest(%q) = true, want false (not a counting request)", q)
		}
	}
}

func TestLooksLikeCountingRequestIsCaseInsensitive(t *testing.T) {
	if !looksLikeCountingRequest("HOW MANY tournaments has Nate won?") {
		t.Error("looksLikeCountingRequest should match regardless of case")
	}
}
