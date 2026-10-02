package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// insertSummaryWithEntitiesTouched is insertSummaryWithEmbeddingIndex's
// sibling, additionally setting entities_touched — the plaintext column
// keywordSearchNarrowed's new "and entities_touched && $N::text[]" SQL
// predicate filters on (internal/store/retrieve.go's fusedSearchSummaries).
// Embedding dimension is fixed at 1 (orthogonal to fakeEmbedder's
// dimension-0 query vector, same technique bm25_retrieve_test.go uses),
// so these tests exercise only the keyword half, never the vector half.
func insertSummaryWithEntitiesTouched(t *testing.T, s *Store, scope identity.Scope, id, period, text string, entitiesTouched []string) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	summaryCT, err := enc.Encrypt(text)
	if err != nil {
		t.Fatalf("encrypt test summary: %v", err)
	}
	vec := make([]float32, 1536)
	vec[1] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (
				id, period, level, status, summary, grounding_checked,
				scope_kind, scope_owner, key_version, embedding, entities_touched
			) values (
				$1, $2, 'daily', 'draft', $3, true,
				$4, $5, $6, $7::vector, $8::text[]
			)
		`, id, period, summaryCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral, pgfmt.TextArray(entitiesTouched))
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

// TestFusedSearchSummaries_NarrowedTierFiltersByEntitiesTouched is the
// real regression test for the narrowing SQL predicate itself: given two
// summaries that both share a BM25-matchable rare term, keywordSearchNarrowed
// with a real matched entity ID must only surface the summary whose
// entities_touched actually overlaps that entity — not just whichever
// scores higher.
func TestFusedSearchSummaries_NarrowedTierFiltersByEntitiesTouched(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-narrow"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	insertSummaryWithEntitiesTouched(t, s, scope, "sum_narrow_match", period,
		"the zorbathon trip went well", []string{"project:zorbathon"})
	insertSummaryWithEntitiesTouched(t, s, scope, "sum_narrow_nomatch", period,
		"the zorbathon detour was long", []string{"project:other"})

	queryVector := pgfmt.VectorLiteral(zeroExceptDim(0))
	var sb strings.Builder
	strongHit := false
	var citations []gateway.Citation
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := s.fusedSearchSummaries(ctx, tx, scope, queryVector, []string{"zorbathon"}, &sb, &strongHit, &citations,
			0.4, 10, "what happened on the zorbathon trip?", time.Now(),
			keywordSearchNarrowed, []string{"project:zorbathon"})
		return err
	})
	if err != nil {
		t.Fatalf("fusedSearchSummaries: %v", err)
	}

	if !strings.Contains(sb.String(), "trip went well") {
		t.Errorf("context missing the entities_touched-overlapping summary, got: %q", sb.String())
	}
	if strings.Contains(sb.String(), "detour was long") {
		t.Errorf("context includes the non-overlapping summary — keywordSearchNarrowed should have filtered it out, got: %q", sb.String())
	}
}

// TestFusedSearchSummaries_NarrowedTierFallsBackToFullScanWithoutEntityMatch
// is keywordSearchNarrowed's documented fallback: with no matched entity
// to narrow by, it must run the exact same unrestricted scan
// keywordSearchFull does, rather than silently dropping recall for a
// query with no entity anchor — BM25's own reason for existing.
func TestFusedSearchSummaries_NarrowedTierFallsBackToFullScanWithoutEntityMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-narrow-fallback"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	insertSummaryWithEntitiesTouched(t, s, scope, "sum_fallback_a", period,
		"the zorbathon trip went well", []string{"project:zorbathon"})
	insertSummaryWithEntitiesTouched(t, s, scope, "sum_fallback_b", period,
		"the zorbathon detour was long", []string{"project:other"})

	queryVector := pgfmt.VectorLiteral(zeroExceptDim(0))
	var sb strings.Builder
	strongHit := false
	var citations []gateway.Citation
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := s.fusedSearchSummaries(ctx, tx, scope, queryVector, []string{"zorbathon"}, &sb, &strongHit, &citations,
			0.4, 10, "what happened on the zorbathon trip?", time.Now(),
			keywordSearchNarrowed, nil)
		return err
	})
	if err != nil {
		t.Fatalf("fusedSearchSummaries: %v", err)
	}

	if !strings.Contains(sb.String(), "trip went well") || !strings.Contains(sb.String(), "detour was long") {
		t.Errorf("narrowed tier with no matched entity should fall back to a full scan, got: %q", sb.String())
	}
}

// TestFusedSearchSummaries_DisabledTierSkipsKeywordSearchEntirely confirms
// keywordSearchDisabled still behaves exactly like today's
// keywordSearchEnabled()==false path: the keyword half never runs at all,
// even when queryTerms is non-empty.
func TestFusedSearchSummaries_DisabledTierSkipsKeywordSearchEntirely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-disabled-tier"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	insertSummaryWithEntitiesTouched(t, s, scope, "sum_disabled", period,
		"the zorbathon trip went well", []string{"project:zorbathon"})

	queryVector := pgfmt.VectorLiteral(zeroExceptDim(0))
	var sb strings.Builder
	strongHit := false
	var citations []gateway.Citation
	refs, err := func() ([]identity.Ref, error) {
		var refs []identity.Ref
		err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			refs, err = s.fusedSearchSummaries(ctx, tx, scope, queryVector, []string{"zorbathon"}, &sb, &strongHit, &citations,
				0.4, 10, "what happened on the zorbathon trip?", time.Now(),
				keywordSearchDisabled, []string{"project:zorbathon"})
			return err
		})
		return refs, err
	}()
	if err != nil {
		t.Fatalf("fusedSearchSummaries: %v", err)
	}

	if len(refs) != 0 {
		t.Errorf("keywordSearchDisabled should find nothing (vector half is also orthogonal here), got refs: %v", refs)
	}
	if strings.Contains(sb.String(), "trip went well") {
		t.Errorf("keywordSearchDisabled should skip the keyword half entirely, got: %q", sb.String())
	}
}

// TestKeywordSearchTierForScope_Boundaries is the direct unit test for
// keywordSearchTierForScope's own decision logic — the global opt-out
// short circuit, the missing-row default, both threshold boundaries, and
// the env var overrides, all independent of the rest of retrieve().
func TestKeywordSearchTierForScope_Boundaries(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-tier-boundaries"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	setCorpusSize := func(t *testing.T, episodeCount, summaryCount int) {
		t.Helper()
		err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				insert into scope_corpus_size (scope_kind, scope_owner, episode_count, summary_count)
				values ($1, $2, $3, $4)
				on conflict (scope_kind, scope_owner) do update
					set episode_count = excluded.episode_count, summary_count = excluded.summary_count
			`, scope.Kind, scope.Owner, episodeCount, summaryCount)
			return err
		})
		if err != nil {
			t.Fatalf("seed scope_corpus_size: %v", err)
		}
	}

	tierOf := func(t *testing.T) keywordSearchTier {
		t.Helper()
		var tier keywordSearchTier
		err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			tier, err = keywordSearchTierForScope(ctx, tx, scope)
			return err
		})
		if err != nil {
			t.Fatalf("keywordSearchTierForScope: %v", err)
		}
		return tier
	}

	t.Run("no row yet defaults to full", func(t *testing.T) {
		if got := tierOf(t); got != keywordSearchFull {
			t.Errorf("tier = %v, want keywordSearchFull for a brand-new scope with no recorded corpus size", got)
		}
	})

	t.Run("below narrow threshold stays full", func(t *testing.T) {
		setCorpusSize(t, 100, 100)
		if got := tierOf(t); got != keywordSearchFull {
			t.Errorf("tier = %v, want keywordSearchFull at 200 total (default narrow threshold is 500)", got)
		}
	})

	t.Run("above narrow threshold, below disable threshold is narrowed", func(t *testing.T) {
		setCorpusSize(t, 400, 400)
		if got := tierOf(t); got != keywordSearchNarrowed {
			t.Errorf("tier = %v, want keywordSearchNarrowed at 800 total (default thresholds are 500/3000)", got)
		}
	})

	t.Run("above disable threshold is disabled", func(t *testing.T) {
		setCorpusSize(t, 2000, 2000)
		if got := tierOf(t); got != keywordSearchDisabled {
			t.Errorf("tier = %v, want keywordSearchDisabled at 4000 total (default disable threshold is 3000)", got)
		}
	})

	t.Run("global opt-out short-circuits regardless of corpus size", func(t *testing.T) {
		setCorpusSize(t, 1, 1)
		t.Setenv("HUPI_ENABLE_KEYWORD_SEARCH", "false")
		if got := tierOf(t); got != keywordSearchDisabled {
			t.Errorf("tier = %v, want keywordSearchDisabled when HUPI_ENABLE_KEYWORD_SEARCH=false, even for a tiny scope", got)
		}
	})

	t.Run("threshold env var overrides apply", func(t *testing.T) {
		setCorpusSize(t, 10, 10)
		t.Setenv("HUPI_KEYWORD_SEARCH_NARROW_THRESHOLD", "15")
		if got := tierOf(t); got != keywordSearchNarrowed {
			t.Errorf("tier = %v, want keywordSearchNarrowed at 20 total with HUPI_KEYWORD_SEARCH_NARROW_THRESHOLD=15", got)
		}
	})
}

func zeroExceptDim(dim int) []float32 {
	vec := make([]float32, 1536)
	vec[dim] = 1
	return vec
}
