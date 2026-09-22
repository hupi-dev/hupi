package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// insertSummaryWithEmbeddingIndex is insertSummary's sibling, but lets
// the caller pick which vector dimension is set to 1 (all others zero)
// instead of always dimension 0 the way insertSummary/insertEntityWithEmbedding
// do. fakeEmbedder always embeds any query to dimension-0-is-1 — a
// summary stored at a *different* dimension is therefore orthogonal to
// every query fakeEmbedder ever produces (cosine similarity exactly 0),
// letting a test force "vector search cannot possibly find this row"
// deterministically, without needing a real embedding model.
func insertSummaryWithEmbeddingIndex(t *testing.T, s *Store, scope identity.Scope, id, period, text string, dim int) {
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
	vec[dim] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (
				id, period, level, status, summary, grounding_checked,
				scope_kind, scope_owner, key_version, embedding
			) values (
				$1, $2, 'daily', 'draft', $3, true,
				$4, $5, $6, $7::vector
			)
		`, id, period, summaryCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

// TestRetrieve_KeywordSearchFindsExactTermVectorSearchMisses is BM25's
// actual reason for existing alongside vector search, exercised
// end-to-end through Retrieve rather than just bm25_test.go's isolated
// scoring unit tests: a summary whose embedding is deliberately
// orthogonal to fakeEmbedder's query vector (similarity exactly 0, an
// order of magnitude below vectorSimilarityThreshold) still gets found,
// because its text contains an exact, rare term the query also uses.
func TestRetrieve_KeywordSearchFindsExactTermVectorSearchMisses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-keyword"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryWithEmbeddingIndex(t, s, scope,
		"sum_test-bm25_2026-01-01_daily_v1", "2026-01-01",
		"Meridian's job scheduler is built in Rust with tokio, using Postgres instead of Redis for its queue.",
		1, // dimension 1, not 0 — orthogonal to every query fakeEmbedder produces
	)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "what async runtime does Meridian use?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q) — keyword search should have found the summary vector search structurally can't", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "keyword match") {
		t.Errorf("context message doesn't show a keyword match, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "tokio") {
		t.Errorf("context message missing the summary's actual content, got: %q", result.ContextMessage)
	}

	var foundSummaryRef bool
	for _, ref := range result.Refs {
		if ref.Kind == identity.RefKindSummary && ref.ID == "sum_test-bm25_2026-01-01_daily_v1" {
			foundSummaryRef = true
		}
	}
	if !foundSummaryRef {
		t.Errorf("Refs missing the matched summary's id, got: %+v", result.Refs)
	}
}

// TestRetrieve_KeywordSearchSkipsWhatVectorSearchAlreadyFound confirms
// keywordSearchSummaries' excludeIDs actually prevents a double-render:
// a summary that clears vectorSimilarityThreshold (default fakeEmbedder
// dimension, so it's a real vector-search hit) and also shares an exact
// term with the query should appear exactly once in the context, not
// twice.
func TestRetrieve_KeywordSearchSkipsWhatVectorSearchAlreadyFound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-dedup"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// dimension 0 matches fakeEmbedder's query vector exactly (similarity
	// 1.0) — a genuine vector-search hit, not a keyword-only one.
	insertSummaryWithEmbeddingIndex(t, s, scope,
		"sum_test-bm25-dedup_2026-01-01_daily_v1", "2026-01-01",
		"Meridian's job scheduler is built in Rust with tokio.",
		0,
	)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "what async runtime does Meridian use?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	count := strings.Count(result.ContextMessage, "sum_test-bm25-dedup_2026-01-01_daily_v1")
	if count != 1 {
		t.Errorf("summary appears %d times in context (want exactly 1) — keyword search should have skipped a summary vector search already found, got: %q", count, result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "keyword match") {
		t.Errorf("context message shows a keyword match for a summary that was actually a vector-search hit, got: %q", result.ContextMessage)
	}
}
