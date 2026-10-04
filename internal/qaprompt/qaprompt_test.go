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

// TestConciseIncludesNonPersonEntityAttributionGuidance is a real
// regression test for 6ae235be: a LongMemEval single-session-assistant
// question about one of three CITGO refineries' process lists, where the
// predicted answer correctly matched the named refinery's first three
// processes but substituted the fourth with a different, similarly-
// structured refinery's extra process. The original "double-check WHO"
// paragraph already covered this exact mechanism for two people; this
// confirms it was generalized to any similar, enumerated entity, not
// just people.
func TestConciseIncludesNonPersonEntityAttributionGuidance(t *testing.T) {
	for _, want := range []string{
		"double-check WHO the retrieved information is actually about",
		"The same risk applies to any set of similar, closely-related things, not just two people",
		"use only the exact list that belongs to the one actually named",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected non-person entity-attribution guidance: %q", want)
		}
	}
}

// TestConciseIncludesSimilarLivesAttributionEmphasis is a real
// regression test for two LoCoMo adversarial misses traced to their
// actual retrieved context (not just the final answer): a question
// about Melanie answered with Caroline's own quote, and a question
// about Caroline answered with Melanie's own quote, both between two
// close friends with heavily overlapping interests (running, mental
// health). In both cases the retrieved context was unambiguous — every
// fact was explicitly labeled with the correct name — confirming this
// isn't a retrieval gap, it's the answer step not reliably applying the
// already-correct WHO guidance when two people's lives are similar
// enough that a fact "sounds like it could belong to either."
func TestConciseIncludesSimilarLivesAttributionEmphasis(t *testing.T) {
	for _, want := range []string{
		"This risk is HIGHEST, not lower, when two people's lives are similar",
		"check the exact name actually attached to the specific fact you're using, every time",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected similar-lives attribution emphasis: %q", want)
		}
	}
}

// TestConciseDoesNotCountAMisattributedFactAsSomethingRelevant is a real
// regression test for a full-scale LoCoMo finding (docs/BENCHMARKS.md):
// rescoring the full 10-conversation run against the v6 baseline found
// the adversarial (abstention) category's "confident wrong" rate (a
// specific answer given with no hedge at all) rose from 23.1% to 35.4%
// — the dominant share of a real ~17-point regression, not just a
// wording/scoring-technicality. The "make your best specific attempt"
// paragraph already existed and already encouraged guessing over
// abstention for genuinely under-specified questions; this connects it
// explicitly to the WHO-check so a fact confirmed to belong to a
// different person/thing doesn't count as "something relevant" to
// guess from, without weakening the paragraph's original purpose for
// questions that really are answerable from what's retrieved.
func TestConciseDoesNotCountAMisattributedFactAsSomethingRelevant(t *testing.T) {
	for _, want := range []string{
		"does not count as something relevant to work with",
		"that is the same as having nothing",
	} {
		if !strings.Contains(Concise, want) {
			t.Errorf("Concise missing expected misattributed-fact-is-not-relevant guidance: %q", want)
		}
	}
}

// TestConciseAbstentionExamplesUseTheExactScoredPhrase is a real
// regression test for a bug this file's own doc comment already
// documented once (gap 3, found on the original LoCoMo run) but PR
// #101/#103 silently reintroduced: LoCoMo's adversarial category is
// scored by a literal, case-insensitive substring check for "not
// mentioned" or "no information available" — see
// bench/data/locomo/task_eval/evaluation.py's own category-5 branch.
// "isn't mentioned" does not contain that substring and scores as
// wrong even though it is a correct abstention in meaning. Every
// abstention example in this prompt must use the literal phrase, not
// a contraction or synonym, so the model's own output is more likely
// to match it too.
func TestConciseAbstentionExamplesUseTheExactScoredPhrase(t *testing.T) {
	if strings.Contains(strings.ToLower(Concise), "isn't mentioned") {
		t.Error(`Concise contains "isn't mentioned" in an example — this does not match LoCoMo's literal "not mentioned" substring check; use "is not mentioned" instead`)
	}
	for _, want := range []string{
		"own necklace is not mentioned",
		"purchasing three cows from peter is not mentioned",
	} {
		if !strings.Contains(strings.ToLower(Concise), want) {
			t.Errorf("Concise missing expected exact-phrase abstention example: %q", want)
		}
	}
}
