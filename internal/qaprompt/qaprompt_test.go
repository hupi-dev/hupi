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
