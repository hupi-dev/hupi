package qaprompt

import (
	"strings"
	"testing"
)

// TestConciseRetainsExistingInstructions guards the move of this prompt
// out of cmd/hupi-bench/replay.go (and the removal of its exact duplicate
// from cmd/hupi-answer-question/main.go) against accidentally dropping
// any of the existing, real-measured instructions.
func TestConciseRetainsExistingInstructions(t *testing.T) {
	for _, want := range []string{
		"a short phrase rather than a full sentence",
		"Always give dates as an absolute date",
		"double-check WHO the retrieved information is actually about",
		"give ONLY the items or the yes/no verdict itself",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected existing instruction: %q", want)
		}
	}
}

// TestConciseIncludesConflictingValuesGuidance is a real regression test
// for 852ce960 (docs/LONGMEMEVAL_ACCURACY_PLAN.md Category 3 Phase 1):
// confirms the new recency-preference instruction, and its guards against
// the real false-positive shapes found investigating that case, are
// present.
func TestConciseIncludesConflictingValuesGuidance(t *testing.T) {
	for _, want := range []string{
		"most recently dated memory's value",
		"in passing or as a recollection",
		"last updated",
		"original, first, or previous value",
		"past tense alone",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected conflicting-values guidance: %q", want)
		}
	}
}

// TestConciseIncludesDerivedComparisonGuidance is a real regression test
// for 09ba9854_abs: a LongMemEval multi-session question where the
// predicted answer paired a genuine Narita bus fare with a genuine but
// wrong-airport (Haneda) taxi estimate and computed a specific-looking
// but ungrounded combined figure. Distinct from
// TestConciseIncludesConflictingValuesGuidance above: that guards picking
// between two competing values of ONE fact; this guards against
// combining two different real figures from two different scenarios that
// were never stated together.
func TestConciseIncludesDerivedComparisonGuidance(t *testing.T) {
	for _, want := range []string{
		"only combine figures that were actually stated about the exact same specific scenario",
		"a different scenario",
		"labeled with which scenario it belongs to",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected derived-comparison guidance: %q", want)
		}
	}
}

// TestConciseIncludesFalsePremiseOrderingGuidance is a real regression
// test for gpt4_70e84552_abs: a LongMemEval temporal-reasoning question
// ("which did I complete first, fixing the fence or purchasing three
// cows from Peter?") where only one of the two named things was ever
// actually mentioned, and the predicted answer named the one real item
// as though it had won a real comparison. Distinct from
// TestConciseIncludesDerivedComparisonGuidance above: that guards against
// combining two real figures that were never stated together; this
// guards against ordering two named things when only one of them is
// real at all.
func TestConciseIncludesFalsePremiseOrderingGuidance(t *testing.T) {
	for _, want := range []string{
		"which of two specific named things happened first",
		"confirm that BOTH named things actually appear",
		"finding one of them is not evidence about the other",
		"say so explicitly and name which one is missing",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected false-premise-ordering guidance: %q", want)
		}
	}
}
