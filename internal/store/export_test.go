package store

import (
	"context"
	"testing"

	"hupi/internal/identity"
)

// TestExportMemory_ReturnsEntitiesSummariesKeyFactsAndRelationships is
// the first real test for docs/EVALMEM_INTEGRATION_PLAN.md's export
// tool — confirms a scope's full memory content round-trips through
// ExportMemory decrypted and complete, the same real data an EvalMem
// adapter's export_full_memory would hand back.
func TestExportMemory_ReturnsEntitiesSummariesKeyFactsAndRelationships(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-export-memory"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithEmbedding(t, s, scope, "person:alex", "person", "Alex", `{"role":"engineer"}`)
	insertEntityWithEmbedding(t, s, scope, "place:beach", "place", "Beach", `{}`)

	period := "2026-01-01"
	summaryID := "sum_test-export-memory_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period, "Alex went to the beach and relaxed.", "")
	insertKeyFact(t, s, scope, summaryID, "Alex camped at the beach.", true)
	insertKeyFact(t, s, scope, summaryID, "Alex camped on the moon.", false)

	insertRelationship(t, s, scope, "rel_test1", "person:alex", "visited", "place:beach")

	export, err := s.ExportMemory(ctx, scope, "test-actor")
	if err != nil {
		t.Fatalf("ExportMemory: %v", err)
	}

	if len(export.Entities) != 2 {
		t.Errorf("len(Entities) = %d, want 2", len(export.Entities))
	}
	if len(export.Summaries) != 1 {
		t.Fatalf("len(Summaries) = %d, want 1", len(export.Summaries))
	}
	sum := export.Summaries[0]
	if sum.Text != "Alex went to the beach and relaxed." {
		t.Errorf("summary text = %q", sum.Text)
	}
	if len(sum.KeyFacts) != 2 {
		t.Fatalf("len(KeyFacts) = %d, want 2", len(sum.KeyFacts))
	}
	foundGrounded, foundUngrounded := false, false
	for _, kf := range sum.KeyFacts {
		if kf.Fact == "Alex camped at the beach." && kf.Grounded {
			foundGrounded = true
		}
		if kf.Fact == "Alex camped on the moon." && !kf.Grounded {
			foundUngrounded = true
		}
	}
	if !foundGrounded {
		t.Error("missing the grounded key fact")
	}
	if !foundUngrounded {
		t.Error("missing the ungrounded key fact (export should include it, unlike Retrieve — a diagnostic export needs the full store, not just what's safe to inject as context)")
	}

	if len(export.Relationships) != 1 {
		t.Fatalf("len(Relationships) = %d, want 1", len(export.Relationships))
	}
	rel := export.Relationships[0]
	if rel.SubjectID != "person:alex" || rel.Predicate != "visited" || rel.ObjectID != "place:beach" {
		t.Errorf("relationship = %+v", rel)
	}
}
