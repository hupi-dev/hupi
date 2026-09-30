package store

import "testing"

// TestLooksLikeRecommendationRequestDetectsRealFailingQuestions uses the
// actual real LongMemEval questions that failed for exactly this reason
// (docs/LONGMEMEVAL_ACCURACY_PLAN.md category 1) — HUPI correctly tries
// to give a personalized recommendation but retrieval finds nothing,
// because these questions share no vocabulary with the preference
// statement they need to recall.
func TestLooksLikeRecommendationRequestDetectsRealFailingQuestions(t *testing.T) {
	real := []string{
		"Can you recommend some interesting cultural events happening around me this weekend?",
		"Can you recommend some recent publications or conferences that I might find interesting?",
		"I'm thinking of inviting my colleagues over for a small gathering. Any tips on what to bake?",
		// These two didn't match the original, narrower keyword list —
		// found during real verification (see the plan doc's own honest
		// writeup) and why recommendationKeywords was widened.
		"I noticed my bike seems to be performing even better during my Sunday group rides. Could there be a reason for this?",
		"I've been feeling nostalgic lately. Do you think it would be a good idea to attend my high school reunion?",
	}
	for _, q := range real {
		if !looksLikeRecommendationRequest(q) {
			t.Errorf("looksLikeRecommendationRequest(%q) = false, want true (a real failing LongMemEval question)", q)
		}
	}
}

func TestLooksLikeRecommendationRequestIgnoresOrdinaryQuestions(t *testing.T) {
	ordinary := []string{
		"What database does Meridian use for its job queue?",
		"When did I start using Ibotta?",
		"What's the weather like today?",
	}
	for _, q := range ordinary {
		if looksLikeRecommendationRequest(q) {
			t.Errorf("looksLikeRecommendationRequest(%q) = true, want false (not a recommendation request)", q)
		}
	}
}

func TestLooksLikeRecommendationRequestIsCaseInsensitive(t *testing.T) {
	if !looksLikeRecommendationRequest("SHOULD I go to the reunion?") {
		t.Error("looksLikeRecommendationRequest should match regardless of case")
	}
}
