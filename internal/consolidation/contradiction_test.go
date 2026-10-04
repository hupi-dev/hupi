package consolidation

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/metrics"
)

func TestBuildContradictionCheckPromptIncludesBothPeriodsAndEntities(t *testing.T) {
	got := buildContradictionCheckPrompt(
		"2023-03-15", []string{"Pre-approval is now $300,000"},
		"daily", "2023-03-01", "The user was pre-approved for $250,000.", []string{"Pre-approval is $250,000"},
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

// TestFindRelatedSummaries_PrefersSpecificSharedEntityOverHub is a real
// regression test for a live failure (852ce960): two genuinely
// conflicting Wells Fargo pre-approval amounts, in summaries months
// apart, never got compared. The new summary's entitiesTouched included
// person:user — present in nearly every summary in a real scope — and
// ranking candidates by recency alone meant the
// maxRelatedSummariesForContradictionCheck most-recent summaries sharing
// *any* entity were all ones that only shared that hub, crowding out the
// one genuinely related summary (sharing the rare, specific
// organization:wells-fargo). This fails without the specificity-ranking
// fix: with recency-only ordering, 6 newer hub-only summaries fill every
// slot before the older, specific one is ever reached.
func TestFindRelatedSummaries_PrefersSpecificSharedEntityOverHub(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, `{"summary": "unused", "key_facts": [], "entities_touched": []}`, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-find-related-specificity"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	store := func(period string, entities []EntityUpdate) {
		t.Helper()
		if err := runner.storeSummary(ctx, storeSummaryInput{
			scope:  scope,
			level:  "daily",
			period: period,
			output: ConsolidationOutput{
				Summary:         "period " + period,
				EntitiesTouched: entities,
			},
			actor: systemActor,
		}); err != nil {
			t.Fatalf("store %s: %v", period, err)
		}
	}

	user := EntityUpdate{ID: "person:user", Kind: "person", Name: "User"}
	wellsFargo := EntityUpdate{ID: "organization:wells-fargo", Kind: "organization", Name: "Wells Fargo"}

	// The one genuinely related summary — oldest by far, sharing the rare
	// entity (organization:wells-fargo) with the triggering one below.
	store("2023-01-01", []EntityUpdate{user, wellsFargo})
	// Six newer summaries, all sharing only the hub entity (person:user) —
	// enough to fill maxRelatedSummariesForContradictionCheck (5) on
	// recency alone and crowd out the real match above.
	for i, period := range []string{"2023-06-01", "2023-06-02", "2023-06-03", "2023-06-04", "2023-06-05", "2023-06-06"} {
		store(period, []EntityUpdate{user})
		_ = i
	}
	// The triggering ("new") summary.
	store("2023-06-10", []EntityUpdate{user, wellsFargo})

	var newID, relatedID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where period = '2023-06-10' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-01-01' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&relatedID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	found, err := runner.findRelatedSummaries(ctx, scope, newID, []string{"person:user", "organization:wells-fargo"})
	if err != nil {
		t.Fatalf("findRelatedSummaries: %v", err)
	}
	for _, f := range found {
		if f.id == relatedID {
			return // found it — specificity ranking worked
		}
	}
	t.Errorf("findRelatedSummaries() = %+v, want it to include the old, specific-entity-sharing summary %q despite 6 newer hub-only summaries", found, relatedID)
}

// TestFindRelatedSummaries_ExcludesSupersededIncludesCorrection is a real
// regression test for the "current" filter bug found alongside the
// specificity one above: "supersedes is null" is backwards (see
// findRelatedSummaries' own doc comment) — it returns exactly the
// stale, corrected-away summary and excludes its own correction. This
// fails without the fix: the pre-fix query would return the superseded
// v1, not the current v2.
func TestFindRelatedSummaries_ExcludesSupersededIncludesCorrection(t *testing.T) {
	contradictionJSON := `{"contradictions": [{"old_fact": "irrelevant", "replacement": "irrelevant"}], "corrected_prose": "corrected"}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, contradictionJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-find-related-supersession"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summary_key_facts where summary_id in (select id from summaries where scope_kind = $1 and scope_owner = $2)`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	wellsFargo := EntityUpdate{ID: "organization:wells-fargo", Kind: "organization", Name: "Wells Fargo"}
	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-01-01",
		output: ConsolidationOutput{
			Summary:         "original",
			KeyFacts:        []KeyFactOutput{{Fact: "irrelevant"}},
			EntitiesTouched: []EntityUpdate{wellsFargo},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	var v1ID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-01-01' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&v1ID)
	}); err != nil {
		t.Fatalf("load v1 id: %v", err)
	}

	// Correct v1 into v2 — the same mechanism checkCrossPeriodContradictions
	// itself uses, so v1 ends up genuinely superseded.
	if err := runner.Correct(ctx, scope, v1ID, ConsolidationOutput{
		Summary:         "corrected",
		KeyFacts:        []KeyFactOutput{{Fact: "irrelevant"}},
		EntitiesTouched: []EntityUpdate{wellsFargo},
	}, "test correction", systemActor, ""); err != nil {
		t.Fatalf("Correct: %v", err)
	}
	var v2ID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where supersedes = $1 and scope_kind = $2 and scope_owner = $3`, v1ID, scope.Kind, scope.Owner).Scan(&v2ID)
	}); err != nil {
		t.Fatalf("load v2 id: %v", err)
	}

	// A later, unrelated-period triggering summary sharing the same entity.
	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2023-02-01",
		output: ConsolidationOutput{
			Summary:         "new",
			EntitiesTouched: []EntityUpdate{wellsFargo},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed triggering summary: %v", err)
	}
	var newID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2023-02-01' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load triggering id: %v", err)
	}

	found, err := runner.findRelatedSummaries(ctx, scope, newID, []string{"organization:wells-fargo"})
	if err != nil {
		t.Fatalf("findRelatedSummaries: %v", err)
	}
	if len(found) != 1 || found[0].id != v2ID {
		t.Errorf("findRelatedSummaries() = %+v, want exactly the correction (v2, %q), not the superseded original (v1, %q)", found, v2ID, v1ID)
	}
}

// TestCheckCrossPeriodContradictions_AppliesCorrection is Phase C
// sub-problem 2's core real behavior: a new period's fact that the LLM
// identifies as contradicting an older, different period's fact about a
// shared entity gets applied as a real Correct — the old summary becomes
// superseded, and its corrected content reflects the new value, not a
// stale duplicate sitting alongside it.
func TestCheckCrossPeriodContradictions_AppliesCorrection(t *testing.T) {
	contradictionJSON := `{"contradictions": [{"old_fact": "Wells Fargo pre-approval amount is $250,000", "replacement": "Wells Fargo pre-approval amount is $300,000"}], "corrected_prose": "Discussed mortgage pre-approval, now at $300,000."}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
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
	if corrected.Summary != "Discussed mortgage pre-approval, now at $300,000." {
		t.Errorf("corrected.Summary = %q, want the model's corrected_prose to have replaced the stale prose too", corrected.Summary)
	}
}

// TestContradictionCheckPrompt_IncludesRedundancyParagraphOnlyWhenEnabled
// is the real regression test for redundancyDedupEnabled's own gating:
// the prompt sent to the model must only teach the second ("restated,
// not contradicted") removal reason when the feature is explicitly
// turned on — this mutates stored consolidation data, so it stays off by
// default the same way queryExpansionEnabled does for its own real
// cost/behavior change.
func TestContradictionCheckPrompt_IncludesRedundancyParagraphOnlyWhenEnabled(t *testing.T) {
	off := contradictionCheckPrompt()
	if strings.Contains(off, "restates the EXACT same specific occurrence") {
		t.Error("contradictionCheckPrompt() with the flag unset includes the redundancy paragraph, want it absent by default")
	}

	t.Setenv("HUPI_ENABLE_REDUNDANCY_DEDUP", "true")
	on := contradictionCheckPrompt()
	for _, want := range []string{
		"restates the EXACT same specific occurrence",
		"a repeating TYPE of event",
		"only report a removal this way when you are confident it is the literal same single occurrence",
	} {
		if !strings.Contains(on, want) {
			t.Errorf("contradictionCheckPrompt() with the flag set missing expected redundancy guidance: %q", want)
		}
	}
}

// TestCheckCrossPeriodContradictions_AppliesRedundancyRemoval is the
// real regression test for docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md's
// follow-up work: a NEW fact that pure-restates an OLD fact (no value
// conflict, "reason": "redundant") must remove the old fact via the
// exact same supersede-and-splice mechanism contradiction correction
// already uses — not a new write path, same Correct call, same
// candidate-selection plumbing.
func TestCheckCrossPeriodContradictions_AppliesRedundancyRemoval(t *testing.T) {
	redundancyJSON := `{"contradictions": [{"old_fact": "Nate did not make it to the finals in his last game tournament", "replacement": "", "reason": "redundant"}], "corrected_prose": "Nate mentioned his gaming hobby again."}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, redundancyJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-cross-period-redundancy"}
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
		period: "2022-11-04",
		output: ConsolidationOutput{
			Summary:         "Nate did not make it to the finals in his last game tournament.",
			KeyFacts:        []KeyFactOutput{{Fact: "Nate did not make it to the finals in his last game tournament"}},
			EntitiesTouched: []EntityUpdate{{ID: "person:nate", Kind: "person", Name: "Nate"}},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed old summary: %v", err)
	}

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2022-11-09",
		output: ConsolidationOutput{
			Summary:         "Nate mentioned his gaming hobby again.",
			KeyFacts:        []KeyFactOutput{{Fact: "Nate did not make it to the finals in his last game tournament"}},
			EntitiesTouched: []EntityUpdate{{ID: "person:nate", Kind: "person", Name: "Nate"}},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed new summary: %v", err)
	}

	var oldID, newID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where period = '2022-11-04' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&oldID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2022-11-09' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	metricBefore := testutil.ToFloat64(metrics.RedundancyFactsRemovedTotal)

	runner.checkCrossPeriodContradictions(ctx, scope, newID, "2022-11-09", []string{"person:nate"})

	if got := testutil.ToFloat64(metrics.RedundancyFactsRemovedTotal); got != metricBefore+1 {
		t.Errorf("hupi_redundancy_facts_removed_total went from %v to %v, want exactly +1", metricBefore, got)
	}

	var supersededBy sql.NullString
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where supersedes = $1 and scope_kind = $2 and scope_owner = $3`, oldID, scope.Kind, scope.Owner).Scan(&supersededBy)
	}); err != nil {
		t.Fatalf("check supersession: %v", err)
	}
	if !supersededBy.Valid {
		t.Fatal("old summary was not superseded — redundancy removal should have applied a correction")
	}

	corrected, err := runner.CurrentContent(ctx, scope, supersededBy.String)
	if err != nil {
		t.Fatalf("load corrected content: %v", err)
	}
	if len(corrected.KeyFacts) != 0 {
		t.Errorf("corrected.KeyFacts = %+v, want the redundant fact removed with nothing replacing it", corrected.KeyFacts)
	}
}

// TestCheckCrossPeriodContradictions_EmptyResponseLeavesBothFactsIntact
// simulates the model correctly declining to merge a recurring-event
// pair (docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md's own explicit "when in
// doubt, do not report it" guard) — this doesn't test the model's actual
// judgment (that needs real-LLM verification against real conversation
// text, not a stub), only that the Go code correctly does nothing when
// told there's nothing to apply: neither summary should be superseded.
func TestCheckCrossPeriodContradictions_EmptyResponseLeavesBothFactsIntact(t *testing.T) {
	emptyJSON := `{"contradictions": [], "corrected_prose": ""}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, emptyJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-cross-period-no-merge"}
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
		period: "2022-04-25",
		output: ConsolidationOutput{
			Summary:         "Nate won a local Street Fighter tournament.",
			KeyFacts:        []KeyFactOutput{{Fact: "Nate won a local Street Fighter tournament"}},
			EntitiesTouched: []EntityUpdate{{ID: "person:nate", Kind: "person", Name: "Nate"}},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed old summary: %v", err)
	}

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2022-09-29",
		output: ConsolidationOutput{
			Summary:         "Nate won a big video game tournament and earned money.",
			KeyFacts:        []KeyFactOutput{{Fact: "Nate won a big video game tournament and earned money"}},
			EntitiesTouched: []EntityUpdate{{ID: "person:nate", Kind: "person", Name: "Nate"}},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed new summary: %v", err)
	}

	var oldID, newID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `select id from summaries where period = '2022-04-25' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&oldID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `select id from summaries where period = '2022-09-29' and scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&newID)
	}); err != nil {
		t.Fatalf("load seeded ids: %v", err)
	}

	runner.checkCrossPeriodContradictions(ctx, scope, newID, "2022-09-29", []string{"person:nate"})

	var supersededBy sql.NullString
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select id from summaries where supersedes = $1 and scope_kind = $2 and scope_owner = $3`, oldID, scope.Kind, scope.Owner).Scan(&supersededBy)
	})
	if err != sql.ErrNoRows {
		t.Errorf("expected no supersession row (err=sql.ErrNoRows), got err=%v supersededBy=%v — the two distinct tournament wins should not have been merged", err, supersededBy)
	}
}
