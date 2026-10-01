package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"hupi/internal/identity"
	"hupi/internal/provider"
)

// TestTemporalRelevanceBoost_MatchesReciprocalRankZero is a direct,
// deterministic regression test for review finding B8: the constant's
// own doc comment has always said it's "set to a full reciprocal-rank-0
// contribution," but the literal value (1.0) was numerically double
// reciprocalRank(0) (0.5, given rrfK's own real value) — a stated
// design intent the code never actually matched. Anchors that relationship
// going forward (e.g. against rrfK drifting without this updating too),
// not just at the moment of the fix.
func TestTemporalRelevanceBoost_MatchesReciprocalRankZero(t *testing.T) {
	if temporalRelevanceBoost != reciprocalRank(0) {
		t.Errorf("temporalRelevanceBoost = %v, want exactly reciprocalRank(0) = %v (its own doc comment's stated design intent)",
			temporalRelevanceBoost, reciprocalRank(0))
	}
}

// TestFusedSearchSummaries_TemporalBoostMagnitudeAffectsSelectionOrder is
// the real, end-to-end proof that B8's magnitude bug had an actual
// behavioral consequence, not just a cosmetic doc/code mismatch. The
// additive boost only ever changes *relative* ranking in one narrow,
// real case: when a candidate with an unparseable period (kept, per
// this file's own "never destroy information on an unclear signal"
// policy — see parsePeriodRange's callers) survives alongside a
// candidate whose period does overlap the resolved timeframe and so
// receives the boost. Constructed so the two candidates' own fused
// scores straddle exactly the 0.5-vs-1.0 gap the bug was in:
//   - "clear-period" overlaps "last month," ranked second by keyword
//     (reciprocalRank(1) = 1/3 ≈ 0.333 before any boost).
//   - "fuzzy-period" has an empty, unparseable period, ranked first by
//     both vector and keyword (reciprocalRank(0)*2 = 1.0).
//
// At the old, buggy boost (1.0): clear-period totals 1.333, winning.
// At the correct boost (0.5): clear-period totals 0.833, losing to
// fuzzy-period's unboosted 1.0 — the fix must flip which one is
// selected first.
func TestFusedSearchSummaries_TemporalBoostMagnitudeAffectsSelectionOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-temporal-boost-magnitude"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// fakeEmbedder (testStore's embedder) always embeds a query to
	// dimension 0 — insertSummaryWithEmbeddingIndex at dim 1 is
	// therefore orthogonal to every real query, forcing vectorRank=-1
	// deterministically (same technique bm25_retrieve_test.go's own
	// doc comment describes).
	insertSummaryWithEmbeddingIndex(t, s, scope,
		"sum_test-temporal-boost-magnitude_clear-period", "2024-05-15",
		"There was a quantum thing, somewhat related to computing I think, mentioned in passing.", 1)
	insertSummary(t, s, scope,
		"sum_test-temporal-boost-magnitude_fuzzy-period", "",
		"The quantum computing quantum computing workshop covered qubits and superposition in depth.", "")

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May — clear-period overlaps
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about the quantum computing workshop last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	clearIdx := strings.Index(result.ContextMessage, "somewhat related to computing")
	fuzzyIdx := strings.Index(result.ContextMessage, "covered qubits and superposition")
	if clearIdx == -1 || fuzzyIdx == -1 {
		t.Fatalf("expected both summaries in context, got: %q", result.ContextMessage)
	}
	if clearIdx < fuzzyIdx {
		t.Errorf("clear-period (fused=0.333+boost: overlaps \"last month\") appeared before fuzzy-period (fused=1.0, no boost: unparseable period) — at the old, buggy boost (1.0) clear-period totals 1.333 and wrongly outranks fuzzy-period's 1.0; with the correct boost (0.5) clear-period only totals 0.833 and should lose, so fuzzy-period should be selected/written first. context: %q", result.ContextMessage)
	}
}
