package consolidation

import (
	"context"
	"strings"
	"testing"
)

func factsN(n int) []KeyFactOutput {
	facts := make([]KeyFactOutput, n)
	for i := range facts {
		facts[i] = KeyFactOutput{Fact: "fact"}
	}
	return facts
}

// TestGroundingCheckBatchesLargeFactListsPreservingOrder is the real,
// measured fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md, per-episode fact
// extraction verification): a single grounding call covering 25 facts
// mismatched its own count on every real attempt once per-episode
// extraction pushed a busy day's fact total past ~70. Splitting into
// groundingCheckBatchSize-sized calls keeps each individual list short.
// This test only exercises the split/reassemble bookkeeping (a fake
// provider can't reproduce the real model's own count-mismatch behavior),
// confirming batch boundaries and result order are correct.
func TestGroundingCheckBatchesLargeFactListsPreservingOrder(t *testing.T) {
	facts := factsN(25) // groundingCheckBatchSize=20 -> batches of 20, then 5
	fake := &fakeSequentialProvider{responses: []string{
		`{"grounded": [true,true,true,true,true,true,true,true,true,true,false,false,false,false,false,false,false,false,false,false]}`,
		`{"grounded": [true,true,true,true,true]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if len(got) != 25 {
		t.Fatalf("groundingCheck() returned %d results, want 25", len(got))
	}
	if fake.calls != 2 {
		t.Fatalf("provider called %d times, want 2 (one per batch)", fake.calls)
	}
	for i := 0; i < 10; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (first half of batch 1)", i)
		}
	}
	for i := 10; i < 20; i++ {
		if got[i] {
			t.Errorf("got[%d] = true, want false (second half of batch 1)", i)
		}
	}
	for i := 20; i < 25; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (batch 2)", i)
		}
	}
}

// TestGroundingCheckMismatchOnOneBatchDoesNotDegradeOthers is the actual
// benefit the batching split provides: a count mismatch on one batch
// (safe-degraded to all-ungrounded, per groundingCheckOne's own existing
// behavior) must not also discard the other, correctly-matched batch's
// real results — the exact all-or-nothing blast radius that made this a
// real problem on busy days in the first place.
func TestGroundingCheckMismatchOnOneBatchDoesNotDegradeOthers(t *testing.T) {
	facts := factsN(25)
	fake := &fakeSequentialProvider{responses: []string{
		// batch 1: 20 facts claimed, only 18 booleans returned -> mismatch, degrades to all-false
		`{"grounded": [true,true,true,true,true,true,true,true,true,true,true,true,true,true,true,true,true,true]}`,
		// batch 2: 5 facts, 5 booleans -> matches, all true preserved
		`{"grounded": [true,true,true,true,true]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	for i := 0; i < 20; i++ {
		if got[i] {
			t.Errorf("got[%d] = true, want false (batch 1's mismatch should degrade only batch 1)", i)
		}
	}
	for i := 20; i < 25; i++ {
		if !got[i] {
			t.Errorf("got[%d] = false, want true (batch 2 matched and must be unaffected by batch 1's mismatch)", i)
		}
	}
}

func TestGroundingCheckSmallListSkipsBatching(t *testing.T) {
	facts := factsN(3)
	fake := &fakeSequentialProvider{responses: []string{
		`{"grounded": [true,false,true]}`,
	}}
	runner := New(nil, nil, nil, fake, nil)

	got, err := runner.groundingCheck(context.Background(), "source", facts)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if fake.calls != 1 {
		t.Errorf("provider called %d times, want exactly 1 for a list under the batch size", fake.calls)
	}
	if len(got) != 3 || got[0] != true || got[1] != false || got[2] != true {
		t.Errorf("groundingCheck() = %v, want [true false true]", got)
	}
}

func TestGroundingCheckEmptyInput(t *testing.T) {
	runner := New(nil, nil, nil, &fakeSequentialProvider{}, nil)
	got, err := runner.groundingCheck(context.Background(), "source", nil)
	if err != nil {
		t.Fatalf("groundingCheck: %v", err)
	}
	if got != nil {
		t.Errorf("groundingCheck(nil facts) = %v, want nil", got)
	}
}

func TestBuildGroundingPromptIncludesSourceAndNumberedFacts(t *testing.T) {
	prompt := buildGroundingPrompt("the source text", []KeyFactOutput{
		{Fact: "fact one"}, {Fact: "fact two"},
	})
	if !strings.Contains(prompt, "the source text") {
		t.Error("buildGroundingPrompt() missing source text")
	}
	if !strings.Contains(prompt, "1. fact one") || !strings.Contains(prompt, "2. fact two") {
		t.Errorf("buildGroundingPrompt() = %q, want numbered facts", prompt)
	}
}
