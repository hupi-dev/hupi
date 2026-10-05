package consolidation

import (
	"context"
	"database/sql"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// TestCheckExtends_ParsesResponse confirms checkExtends correctly parses a
// real-shaped response from extendsCheckPrompt — the live-verified prompt
// itself (see docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md Phase 5's own
// writeup) isn't re-tested here (that needs a real LLM), just the Go-level
// request/response plumbing around it, the same split
// TestExtractInferences_ParsesResponse already uses for Phase 4.
func TestCheckExtends_ParsesResponse(t *testing.T) {
	responseJSON := `{"extends": [{"existing_fact": "Dana is training for a marathon this spring.", "new_fact": "Dana ran 14 miles along the river trail last Saturday as part of her training."}]}`
	runner, _ := testRunner(t, responseJSON, `{"grounded": []}`)

	got, err := runner.checkExtends(context.Background(), []string{"Dana ran 14 miles along the river trail last Saturday as part of her training."}, []string{"Dana is training for a marathon this spring."})
	if err != nil {
		t.Fatalf("checkExtends: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d pairs, want 1: %+v", len(got), got)
	}
	if got[0].ExistingFact != "Dana is training for a marathon this spring." {
		t.Errorf("ExistingFact = %q, want the marathon fact", got[0].ExistingFact)
	}
	if got[0].NewFact != "Dana ran 14 miles along the river trail last Saturday as part of her training." {
		t.Errorf("NewFact = %q, want the training-run fact", got[0].NewFact)
	}
}

// TestCheckExtends_NoOpWithEmptyFacts confirms checkExtends never makes an
// LLM call when either side has nothing to compare — the same "nothing to
// do, don't pay for a call" posture extractInferences' own
// len(known)==0 short-circuit already takes.
func TestCheckExtends_NoOpWithEmptyFacts(t *testing.T) {
	runner, _ := testRunner(t, `{"extends": []}`, `{"grounded": []}`)
	fake := runner.consolidation.(fakeConsolidationProvider)
	var captured []provider.ChatRequest
	fake.capturedRequests = &captured
	runner.consolidation = fake

	if got, err := runner.checkExtends(context.Background(), nil, []string{"some existing fact"}); err != nil || got != nil {
		t.Errorf("checkExtends(nil, ...) = (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := runner.checkExtends(context.Background(), []string{"some new fact"}, nil); err != nil || got != nil {
		t.Errorf("checkExtends(..., nil) = (%v, %v), want (nil, nil)", got, err)
	}
	if len(captured) != 0 {
		t.Errorf("made %d LLM call(s) with an empty side, want 0", len(captured))
	}
}

// TestCheckOneRelatedSummaryForExtends_RecordsRelation is the real-infra
// regression for Phase 5 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: a
// genuine extends pair must land as a memory_relations row (relation_type
// 'extends') linking the NEW fact's own memories row (from) to the
// EXISTING fact's (to) — the same from-is-newer convention
// TestCheckCrossPeriodContradictions_AppliesCorrection already confirms
// for 'updates'. Unlike that test, no Correct call happens here at all:
// both summaries' own fact text are left exactly as stored.
func TestCheckOneRelatedSummaryForExtends_RecordsRelation(t *testing.T) {
	extendsJSON := `{"extends": [{"existing_fact": "Dana is training for a marathon this spring.", "new_fact": "Dana ran 14 miles along the river trail last Saturday as part of her training."}]}`
	runner, db := testRunner(t, extendsJSON, `{"grounded": [{"i":1,"ok":true}]}`)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-extends-relation"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summary_key_facts where summary_id in (select id from summaries where scope_kind = $1 and scope_owner = $2)`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	dana := EntityUpdate{ID: "person:dana", Kind: "person", Name: "Dana"}
	if _, err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-03-01",
		output: ConsolidationOutput{
			Summary:         "Dana started marathon training.",
			KeyFacts:        []KeyFactOutput{{Fact: "Dana is training for a marathon this spring."}},
			EntitiesTouched: []EntityUpdate{dana},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed old summary: %v", err)
	}
	if _, err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-03-08",
		output: ConsolidationOutput{
			Summary:         "Dana went on a long training run.",
			KeyFacts:        []KeyFactOutput{{Fact: "Dana ran 14 miles along the river trail last Saturday as part of her training."}},
			EntitiesTouched: []EntityUpdate{dana},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed new summary: %v", err)
	}

	var oldID, newID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where period = '2023-03-01' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&oldID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-03-08' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	newFacts, err := runner.loadGroundedKeyFactsByID(ctx, scope, newID)
	if err != nil {
		t.Fatalf("load new facts: %v", err)
	}
	runner.checkOneRelatedSummaryForExtends(ctx, scope, newID, relatedSummary{id: oldID, level: "daily", period: "2023-03-01"}, newFacts)

	oldFactIDs, err := runner.loadKeyFactIDsByText(ctx, scope, oldID)
	if err != nil {
		t.Fatalf("load old fact ids: %v", err)
	}
	newFactIDs, err := runner.loadKeyFactIDsByText(ctx, scope, newID)
	if err != nil {
		t.Fatalf("load new fact ids: %v", err)
	}
	oldMemID := memoryIDForKeyFact(oldFactIDs["Dana is training for a marathon this spring."])
	newMemID := memoryIDForKeyFact(newFactIDs["Dana ran 14 miles along the river trail last Saturday as part of her training."])

	var relationType string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select relation_type from memory_relations
			where from_memory_id = $1 and to_memory_id = $2 and scope_kind = $3 and scope_owner = $4
		`, newMemID, oldMemID, scope.Kind, scope.Owner).Scan(&relationType)
	}); err != nil {
		t.Fatalf("load extends relation: %v", err)
	}
	if relationType != "extends" {
		t.Errorf("relation_type = %q, want %q", relationType, "extends")
	}
}

// TestCheckCrossPeriodContradictions_ExtendsGatedByFlag confirms
// extendsDetectionEnabled() actually gates the extra LLM call
// checkCrossPeriodContradictions now makes per related summary — off by
// default (one call, the existing contradiction check), on only when
// HUPI_ENABLE_EXTENDS_DETECTION=true (a second call, the extends check).
func TestCheckCrossPeriodContradictions_ExtendsGatedByFlag(t *testing.T) {
	// Valid under both contradictionResponse and the extends-shaped
	// unmarshal (unknown/missing fields default to zero values in Go's
	// encoding/json) — this test only cares about call *count*, not
	// content.
	noopJSON := `{"contradictions": [], "corrected_prose": ""}`
	runner, db := testRunner(t, noopJSON, `{"grounded": [{"i":1,"ok":true}]}`)
	var captured []provider.ChatRequest
	fake := runner.consolidation.(fakeConsolidationProvider)
	fake.capturedRequests = &captured
	runner.consolidation = fake

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-extends-flag-gate"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summary_key_facts where summary_id in (select id from summaries where scope_kind = $1 and scope_owner = $2)`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	dana := EntityUpdate{ID: "person:dana", Kind: "person", Name: "Dana"}
	if _, err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-04-01",
		output: ConsolidationOutput{
			Summary:         "old",
			KeyFacts:        []KeyFactOutput{{Fact: "old fact"}},
			EntitiesTouched: []EntityUpdate{dana},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed old summary: %v", err)
	}
	if _, err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-04-08",
		output: ConsolidationOutput{
			Summary:         "new",
			KeyFacts:        []KeyFactOutput{{Fact: "new fact"}},
			EntitiesTouched: []EntityUpdate{dana},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed new summary: %v", err)
	}
	var newID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-04-08' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load new id: %v", err)
	}

	runner.checkCrossPeriodContradictions(ctx, scope, newID, "2023-04-08", []string{"person:dana"})
	if len(captured) != 1 {
		t.Fatalf("flag off: made %d LLM call(s), want 1 (contradiction check only)", len(captured))
	}

	captured = nil
	t.Setenv("HUPI_ENABLE_EXTENDS_DETECTION", "true")
	runner.checkCrossPeriodContradictions(ctx, scope, newID, "2023-04-08", []string{"person:dana"})
	if len(captured) != 2 {
		t.Fatalf("flag on: made %d LLM call(s), want 2 (contradiction check + extends check)", len(captured))
	}
}
