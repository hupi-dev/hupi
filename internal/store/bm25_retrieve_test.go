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

// insertEntityWithEmbeddingIndex is insertEntityWithEmbedding's sibling
// with a controllable embedding dimension — see
// insertSummaryWithEmbeddingIndex's doc comment for why.
func insertEntityWithEmbeddingIndex(t *testing.T, s *Store, scope identity.Scope, id, kind, name, attrsJSON string, dim int) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(attrsJSON)
	if err != nil {
		t.Fatalf("encrypt test entity attrs: %v", err)
	}
	vec := make([]float32, 1536)
	vec[dim] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner, key_version, embedding)
			values ($1, $2, $3, $4, $5, $6, $7, $8::vector)
		`, id, kind, name, attrsCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity %s: %v", id, err)
	}
}

// TestRetrieve_KeywordSearchFindsEntityByAttributeContentVectorSearchMisses
// mirrors the summary/episode BM25 tests, but proves the entity path
// specifically: the query shares no substring with the entity's own name
// (so stage1EntityMatches can't find it either), and the entity's
// embedding is deliberately orthogonal to the query's (so vector search
// can't find it) — only BM25 scoring the *decrypted attributes* against
// the query finds it, via a distinctive token that appears in neither
// the entity's name nor stage1's substring check at all.
func TestRetrieve_KeywordSearchFindsEntityByAttributeContentVectorSearchMisses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-entity"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithEmbeddingIndex(t, s, scope,
		"tool:job-queue-note", "skill", "Job processing note",
		`{"detail":"uses zephyrbatch for background job processing"}`,
		1, // orthogonal to fakeEmbedder's query vector
	)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "what is zephyrbatch used for?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "entity tool:job-queue-note") || !strings.Contains(result.ContextMessage, "keyword match") {
		t.Errorf("context message missing the entity keyword match, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "zephyrbatch") {
		t.Errorf("context message missing the entity's actual attribute content, got: %q", result.ContextMessage)
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

// insertEpisodeWithoutEmbedding seeds a plain interaction episode with no
// embedding at all (the column stays NULL) — the case
// vectorSearchEpisodes' own "embedding is not null" filter means it can
// never even be a candidate for vector search, regardless of similarity.
func insertEpisodeWithoutEmbedding(t *testing.T, s *Store, scope identity.Scope, id, input, output string) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, err := enc.Encrypt(input)
	if err != nil {
		t.Fatalf("encrypt test episode input: %v", err)
	}
	outputCT, err := enc.Encrypt(output)
	if err != nil {
		t.Fatalf("encrypt test episode output: %v", err)
	}

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into episodes (id, type, input_text, output_text, hash, scope_kind, scope_owner, key_version)
			values ($1, 'interaction', $2, $3, $4, $5, $6, $7)
		`, id, inputCT, outputCT, hash(id), scope.Kind, scope.Owner, keyVersion)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode %s: %v", id, err)
	}
}

// TestRetrieve_KeywordSearchFindsEpisodeWithNoEmbeddingAtAll is the real
// reason keywordSearchEpisodes was widened beyond vectorSearchEpisodes'
// own "embedding is not null" restriction: a low-importance episode
// consolidation never deemed worth embedding was never even a candidate
// for vector search — no similarity score, however low, could ever
// surface it, since it isn't in that query's result set at all. BM25
// needs no embedding, so it's still reachable via exact-term overlap.
func TestRetrieve_KeywordSearchFindsEpisodeWithNoEmbeddingAtAll(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-bm25-no-embedding"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEpisodeWithoutEmbedding(t, s, scope,
		"ep_test-bm25-no-embedding_1",
		"quick note: Meridian's scheduler now uses tokio for its async runtime",
		"got it, noted",
	)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "what async runtime does Meridian use?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q) — an unembedded episode should still be reachable via keyword search", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "ep_test-bm25-no-embedding_1") {
		t.Errorf("context message missing the unembedded episode, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "keyword match") {
		t.Errorf("context message doesn't show this as a keyword match, got: %q", result.ContextMessage)
	}
}
