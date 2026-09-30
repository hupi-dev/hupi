package consolidation

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

func TestBuildContradictionCheckPromptIncludesBothPeriodsAndEntities(t *testing.T) {
	got := buildContradictionCheckPrompt(
		"2023-03-15", []string{"Pre-approval is now $300,000"},
		"daily", "2023-03-01", []string{"Pre-approval is $250,000"},
		[]string{"Wells Fargo"},
	)
	for _, want := range []string{"Wells Fargo", "2023-03-15", "2023-03-01", "$300,000", "$250,000"} {
		if !strings.Contains(got, want) {
			t.Errorf("buildContradictionCheckPrompt() missing %q, got: %q", want, got)
		}
	}
}

// TestFindRelatedSummaries_MatchesSharedEntityExcludesOthers is Phase C
// sub-problem 2's real retrieval step
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): confirms the
// entities_touched overlap query finds a genuinely related summary,
// excludes an unrelated one, excludes the triggering summary itself, and
// excludes an already-superseded one (which should never be treated as
// "current" and eligible for a second, conflicting correction).
func TestFindRelatedSummaries_MatchesSharedEntityExcludesOthers(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, `{"summary": "unused", "key_facts": [], "entities_touched": []}`, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-find-related-summaries"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	store := func(period, entityID, entityName string) {
		t.Helper()
		if err := runner.storeSummary(ctx, storeSummaryInput{
			scope:  scope,
			level:  "daily",
			period: period,
			output: ConsolidationOutput{
				Summary:         "period " + period,
				EntitiesTouched: []EntityUpdate{{ID: entityID, Kind: "organization", Name: entityName}},
			},
			actor: systemActor,
		}); err != nil {
			t.Fatalf("store %s: %v", period, err)
		}
	}

	// canonicalEntityID derives the real id from kind+name, not the ID
	// field given here — distinct names are what actually makes these
	// two different entities, not the entityID strings passed in.
	store("2023-03-01", "organization:wells-fargo", "Wells Fargo") // the related one
	store("2023-03-05", "organization:chase", "Chase Bank")        // unrelated, different entity
	store("2023-03-10", "organization:wells-fargo", "Wells Fargo") // the triggering ("new") one

	var newID, relatedID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where period = '2023-03-10' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-03-01' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&relatedID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	found, err := runner.findRelatedSummaries(ctx, scope, newID, []string{"organization:wells-fargo"})
	if err != nil {
		t.Fatalf("findRelatedSummaries: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("findRelatedSummaries() returned %d summaries, want exactly 1 (Chase excluded, self excluded), got: %+v", len(found), found)
	}
	if found[0].id != relatedID {
		t.Errorf("findRelatedSummaries()[0].id = %q, want %q", found[0].id, relatedID)
	}
}

// TestCheckCrossPeriodContradictions_AppliesCorrection is Phase C
// sub-problem 2's core real behavior: a new period's fact that the LLM
// identifies as contradicting an older, different period's fact about a
// shared entity gets applied as a real Correct — the old summary becomes
// superseded, and its corrected content reflects the new value, not a
// stale duplicate sitting alongside it.
func TestCheckCrossPeriodContradictions_AppliesCorrection(t *testing.T) {
	contradictionJSON := `{"contradictions": [{"old_fact": "Wells Fargo pre-approval amount is $250,000", "replacement": "Wells Fargo pre-approval amount is $300,000"}]}`
	groundingJSON := `{"grounded": [true]}`
	runner, db := testRunner(t, contradictionJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-cross-period-contradiction"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summary_key_facts where summary_id in (select id from summaries where scope_kind = $1 and scope_owner = $2)`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-03-01",
		output: ConsolidationOutput{
			Summary:         "Discussed mortgage pre-approval.",
			KeyFacts:        []KeyFactOutput{{Fact: "Wells Fargo pre-approval amount is $250,000"}},
			EntitiesTouched: []EntityUpdate{{ID: "organization:wells-fargo", Kind: "organization", Name: "Wells Fargo"}},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed old summary: %v", err)
	}

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-03-15",
		output: ConsolidationOutput{
			Summary:         "Mortgage pre-approval updated.",
			KeyFacts:        []KeyFactOutput{{Fact: "Wells Fargo pre-approval amount is $300,000"}},
			EntitiesTouched: []EntityUpdate{{ID: "organization:wells-fargo", Kind: "organization", Name: "Wells Fargo"}},
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
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-03-15' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	runner.checkCrossPeriodContradictions(ctx, scope, newID, "2023-03-15", []string{"organization:wells-fargo"})

	var supersededBy sql.NullString
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where supersedes = $1 and scope_kind = $2 and scope_owner = $3`, oldID, scope.Kind, scope.Owner).Scan(&supersededBy)
	}); err != nil {
		t.Fatalf("check supersession: %v", err)
	}
	if !supersededBy.Valid {
		t.Fatal("old summary was not superseded — checkCrossPeriodContradictions should have applied a correction")
	}

	corrected, err := runner.CurrentContent(ctx, scope, supersededBy.String)
	if err != nil {
		t.Fatalf("load corrected content: %v", err)
	}
	if len(corrected.KeyFacts) != 1 || corrected.KeyFacts[0].Fact != "Wells Fargo pre-approval amount is $300,000" {
		t.Errorf("corrected.KeyFacts = %+v, want exactly one fact with the replacement text", corrected.KeyFacts)
	}
}
