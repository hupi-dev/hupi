package consolidation

import (
	"context"
	"testing"

	"hupi/internal/provider"
)

// TestExtractInferences_ParsesResponse confirms extractInferences
// correctly parses a real-shaped response from inferenceExtractionPrompt
// — the live-verified prompt itself (see docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md
// Phase 4's own update) isn't re-tested here (that needs a real LLM,
// not a fake), just the Go-level request/response plumbing around it.
func TestExtractInferences_ParsesResponse(t *testing.T) {
	responseJSON := `{"inferences": [{"entity_id": "person:joanna", "fact": "Joanna likely has asthma.", "inferred_from_attribute_keys": ["allergic_to", "allergic_to_cockroaches"]}]}`
	runner, _ := testRunner(t, responseJSON, `{"grounded": []}`)

	known := []knownEntityContext{
		{id: "person:joanna", name: "Joanna", attributes: map[string]string{
			"allergic_to":             "most reptiles and animals with fur",
			"allergic_to_cockroaches": "yes",
		}},
	}
	got, err := runner.extractInferences(context.Background(), known, "USER: Joanna and I went hiking.\nASSISTANT: Nice!")
	if err != nil {
		t.Fatalf("extractInferences: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inferences, want 1: %+v", len(got), got)
	}
	inf := got[0]
	if inf.EntityID != "person:joanna" {
		t.Errorf("EntityID = %q, want person:joanna", inf.EntityID)
	}
	if inf.Fact != "Joanna likely has asthma." {
		t.Errorf("Fact = %q, want the asthma inference text", inf.Fact)
	}
	if len(inf.InferredFromAttributeKeys) != 2 {
		t.Errorf("InferredFromAttributeKeys = %v, want 2 keys", inf.InferredFromAttributeKeys)
	}
}

// TestExtractInferences_NoOpWithoutKnownEntities confirms the function
// never makes an LLM call at all when there's nothing to check against
// — the same "nothing to do, don't pay for a call" posture
// attributionCheck's own zero-citations case already takes.
func TestExtractInferences_NoOpWithoutKnownEntities(t *testing.T) {
	runner, _ := testRunner(t, `{"inferences": []}`, `{"grounded": []}`)
	fake := runner.consolidation.(fakeConsolidationProvider)
	var captured []provider.ChatRequest
	fake.capturedRequests = &captured
	runner.consolidation = fake

	got, err := runner.extractInferences(context.Background(), nil, "some conversation text")
	if err != nil {
		t.Fatalf("extractInferences: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if len(captured) != 0 {
		t.Errorf("made %d LLM call(s) with no known entities, want 0", len(captured))
	}
}
