package store

import "testing"

// TestLooksLikeOrderingRequestDetectsRealFailingQuestions uses the
// actual real LongMemEval questions that failed this way
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase D item 2) — HUPI needs
// several different summaries recalled and combined for these, not just
// the single best match, and the specific events involved often share no
// vocabulary with the question's own generic phrasing.
func TestLooksLikeOrderingRequestDetectsRealFailingQuestions(t *testing.T) {
	real := []string{
		"What is the order of the sports events I watched in January?",
		"What is the order of the three sports events I participated in during the past month, from earliest to latest?",
		"How many months have passed since I participated in two charity events in a row, on consecutive days?",
		"Which task did I complete first, fixing the fence or purchasing three cows from Peter?",
		"Which item did I purchase first, the dog bed for Max or the training pads for Luna?",
	}
	for _, q := range real {
		if !looksLikeOrderingRequest(q) {
			t.Errorf("looksLikeOrderingRequest(%q) = false, want true (a real failing LongMemEval question)", q)
		}
	}
}

func TestLooksLikeOrderingRequestIgnoresOrdinaryQuestions(t *testing.T) {
	ordinary := []string{
		"What database does Meridian use for its job queue?",
		"When did I start using Ibotta?",
		"What's the weather like today?",
		"Can you recommend a good book?",
	}
	for _, q := range ordinary {
		if looksLikeOrderingRequest(q) {
			t.Errorf("looksLikeOrderingRequest(%q) = true, want false (not a multi-event/ordering request)", q)
		}
	}
}

func TestLooksLikeOrderingRequestIsCaseInsensitive(t *testing.T) {
	if !looksLikeOrderingRequest("WHAT IS THE ORDER OF the events?") {
		t.Error("looksLikeOrderingRequest should match regardless of case")
	}
}
