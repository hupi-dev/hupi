package main

import (
	"testing"

	"hupi/internal/identity"
)

// TestLoadProbes_ShippedExampleFileParsesAndDemonstratesEveryField is a
// real regression test for the shipped probes.yaml example (review
// finding B6): confirms the file this repo actually ships still parses
// cleanly, and specifically that its expect_max_gate and scope examples
// decode into the values they look like on the page — yaml.v3's default
// field-name matching (identity.Scope has no yaml tags, only json ones)
// isn't obviously guaranteed to line up with the lowercase "kind"/"owner"
// keys used in the file without a test actually proving it.
func TestLoadProbes_ShippedExampleFileParsesAndDemonstratesEveryField(t *testing.T) {
	probes, err := loadProbes("../../probes.yaml")
	if err != nil {
		t.Fatalf("loadProbes(probes.yaml): %v", err)
	}
	if len(probes) == 0 {
		t.Fatal("probes.yaml parsed to zero probes")
	}

	byID := make(map[string]int)
	for i, p := range probes {
		byID[p.ID] = i
	}

	maxGateIdx, ok := byID["nonsense-query-stays-skipped"]
	if !ok {
		t.Fatal(`probes.yaml: expected a probe with id "nonsense-query-stays-skipped" demonstrating expect_max_gate`)
	}
	if probes[maxGateIdx].ExpectMaxGate != "skipped" {
		t.Errorf("nonsense-query-stays-skipped: ExpectMaxGate = %q, want %q", probes[maxGateIdx].ExpectMaxGate, "skipped")
	}

	scopeIdx, ok := byID["example-team-scope-stays-skipped"]
	if !ok {
		t.Fatal(`probes.yaml: expected a probe with id "example-team-scope-stays-skipped" demonstrating a non-default scope`)
	}
	wantScope := identity.Scope{Kind: identity.ScopeKindShared, Owner: "team:example"}
	if probes[scopeIdx].Scope != wantScope {
		t.Errorf("example-team-scope-stays-skipped: Scope = %+v, want %+v (yaml.v3's default field matching must actually populate this struct, not silently leave it zero-valued)", probes[scopeIdx].Scope, wantScope)
	}
}
