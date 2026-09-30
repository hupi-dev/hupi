package store

import "testing"

func TestJaccardOverlapIdenticalTextIsOne(t *testing.T) {
	a := tokenSet("Caroline and Melanie catch up about recent activities")
	b := tokenSet("Caroline and Melanie catch up about recent activities")
	if got := jaccardOverlap(a, b); got != 1.0 {
		t.Errorf("jaccardOverlap(identical) = %v, want 1.0", got)
	}
}

func TestJaccardOverlapUnrelatedTextIsZero(t *testing.T) {
	a := tokenSet("Caroline attended a pride parade last Friday")
	b := tokenSet("Melanie went camping with her family in the mountains")
	if got := jaccardOverlap(a, b); got != 0 {
		t.Errorf("jaccardOverlap(unrelated) = %v, want 0", got)
	}
}

func TestJaccardOverlapPartialOverlap(t *testing.T) {
	a := tokenSet("Caroline and Melanie discussed pottery and painting")
	b := tokenSet("Caroline and Melanie discussed hiking and camping")
	got := jaccardOverlap(a, b)
	if got <= 0 || got >= 1 {
		t.Errorf("jaccardOverlap(partial) = %v, want strictly between 0 and 1", got)
	}
}

// TestMMRSelectPrefersDiversityOverNearDuplicate is the real hypothesis
// this whole feature exists to test (docs/BENCHMARK_IMPROVEMENT_PLAN.md
// step 3): given a highly relevant candidate, a near-duplicate of it
// (slightly lower relevance but almost the same content), and a
// genuinely distinct candidate (lower relevance, no content overlap),
// MMR should pick the distinct one over the near-duplicate once the top
// candidate is already selected -- plain top-K by relevance would pick
// the near-duplicate instead, wasting a slot on redundant content.
func TestMMRSelectPrefersDiversityOverNearDuplicate(t *testing.T) {
	best := "Caroline and Melanie discussed Caroline's new dance studio plans"
	nearDup := "Caroline and Melanie discussed Caroline's dance studio and plans"
	distinct := "Melanie took her kids to the museum to see the dinosaur exhibit"

	pool := []mmrCandidate{
		{relevance: 0.90, tokens: tokenSet(best)},
		{relevance: 0.85, tokens: tokenSet(nearDup)},
		{relevance: 0.60, tokens: tokenSet(distinct)},
	}

	picked := mmrSelect(pool, 2, 0.7)
	if len(picked) != 2 {
		t.Fatalf("mmrSelect returned %d picks, want 2", len(picked))
	}
	if picked[0] != 0 {
		t.Errorf("first pick = index %d, want 0 (the single most relevant candidate)", picked[0])
	}
	if picked[1] != 2 {
		t.Errorf("second pick = index %d, want 2 (the distinct candidate) — got the near-duplicate instead, MMR isn't penalizing redundancy", picked[1])
	}
}

// TestMMRSelectDoesNotDiscardButStillSortsWhenPoolFitsWithinK confirms
// mmrSelect never discards a candidate when there's no actual surplus to
// choose from (asking for k from exactly k must return everything), but
// still orders that full set by relevance — a real, previously untested
// bug (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase E verification): an
// earlier version of this shortcut returned candidates in raw pool
// (insertion) order whenever nothing needed discarding, which silently
// discarded Phase D item 2's widened threshold and Phase E's temporal
// boost's entire effect on context order for exactly the common case —
// few enough candidates that none get cut here. This pool is built with
// the lower-relevance candidate first specifically so a naive "leave
// order unchanged" implementation would fail this test.
func TestMMRSelectDoesNotDiscardButStillSortsWhenPoolFitsWithinK(t *testing.T) {
	pool := []mmrCandidate{
		{relevance: 0.5, tokens: tokenSet("first")},
		{relevance: 0.9, tokens: tokenSet("second")},
	}
	picked := mmrSelect(pool, 2, 0.7)
	if len(picked) != 2 || picked[0] != 1 || picked[1] != 0 {
		t.Errorf("mmrSelect(pool, k=len(pool)) = %v, want [1 0] (sorted by relevance descending, not insertion order)", picked)
	}
}

// TestMMRSelectLambdaOneIgnoresDiversity confirms lambda=1 degenerates to
// plain top-K by relevance (the diversity term is fully zeroed out) —
// pins down the formula's own documented behavior at its boundary.
func TestMMRSelectLambdaOneIgnoresDiversity(t *testing.T) {
	best := "Caroline and Melanie discussed Caroline's new dance studio plans"
	nearDup := "Caroline and Melanie discussed Caroline's dance studio and plans"
	distinct := "Melanie took her kids to the museum to see the dinosaur exhibit"

	pool := []mmrCandidate{
		{relevance: 0.90, tokens: tokenSet(best)},
		{relevance: 0.85, tokens: tokenSet(nearDup)},
		{relevance: 0.60, tokens: tokenSet(distinct)},
	}
	picked := mmrSelect(pool, 2, 1.0)
	if len(picked) != 2 || picked[0] != 0 || picked[1] != 1 {
		t.Errorf("mmrSelect(lambda=1) = %v, want [0 1] (plain top-2 by relevance, diversity ignored)", picked)
	}
}
