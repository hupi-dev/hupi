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
	"hupi/internal/provider"
)

// insertSummaryWithEmbeddingModel is insertSummary's sibling with an
// explicit embedding_model, and always at dimension 0 — a guaranteed top
// vector match under fakeEmbedder (docs/CODEBASE_SURVEY_AND_REVIEW.md
// finding A3's regression tests need this: a row that would clearly win
// on raw cosine distance, so excluding it can only be the embedding_model
// filter at work, not coincidental ranking).
func insertSummaryWithEmbeddingModel(t *testing.T, s *Store, scope identity.Scope, id, period, text string, embeddingModel *string) {
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
	vec[0] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (
				id, period, level, status, summary, grounding_checked,
				scope_kind, scope_owner, key_version, embedding, embedding_model
			) values (
				$1, $2, 'daily', 'draft', $3, true,
				$4, $5, $6, $7::vector, $8
			)
		`, id, period, summaryCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral, embeddingModel)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

// insertEntityWithEmbeddingModel is insertEntityWithEmbedding's sibling
// with an explicit embedding_model — see insertSummaryWithEmbeddingModel's
// doc comment.
func insertEntityWithEmbeddingModel(t *testing.T, s *Store, scope identity.Scope, id, kind, name, attrsJSON string, embeddingModel *string) {
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
	vec[0] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner, key_version, embedding, embedding_model)
			values ($1, $2, $3, $4, $5, $6, $7, $8::vector, $9)
		`, id, kind, name, attrsCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral, embeddingModel)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity %s: %v", id, err)
	}
}

// insertEpisodeWithEmbeddingModel is insertEpisodeWithoutEmbedding's
// sibling, but with a real embedding (dimension 0, guaranteed top vector
// match) and an explicit embedding_model.
func insertEpisodeWithEmbeddingModel(t *testing.T, s *Store, scope identity.Scope, id, input, output string, embeddingModel *string) {
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
	vec := make([]float32, 1536)
	vec[0] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into episodes (id, type, input_text, output_text, hash, scope_kind, scope_owner, key_version, embedding, embedding_model)
			values ($1, 'interaction', $2, $3, $4, $5, $6, $7, $8::vector, $9)
		`, id, inputCT, outputCT, hash(id), scope.Kind, scope.Owner, keyVersion, embeddingLiteral, embeddingModel)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode %s: %v", id, err)
	}
}

func strPtr(s string) *string { return &s }

// TestRetrieve_SummaryVectorSearchExcludesDifferentEmbeddingModel is a
// real regression test (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A3):
// a summary embedded under a *different*, no-longer-active embedding
// model used to still be a valid vector-search candidate — its cosine
// distance to a current query vector is close to noise, not a real
// similarity signal, but nothing filtered it out. This summary is
// deliberately the best possible raw vector match (dimension 0, exactly
// what fakeEmbedder always produces for any query) specifically so
// excluding it can only be the embedding_model filter at work.
func TestRetrieve_SummaryVectorSearchExcludesDifferentEmbeddingModel(t *testing.T) {
	// Keyword search is a separate, embedding-independent path — BM25
	// correctly has nothing to do with embedding_model, so it must be
	// disabled here to isolate what's actually under test (vector search's
	// own filter), not merely avoided via careful word choice.
	t.Setenv("HUPI_ENABLE_KEYWORD_SEARCH", "false")
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-model-summary"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryWithEmbeddingModel(t, s, scope, "sum_test-embed-model_2026-01-01_daily_v1", "2026-01-01",
		"stale-model fact: the answer is 999", strPtr("old-vendor:old-model"))
	insertSummaryWithEmbeddingModel(t, s, scope, "sum_test-embed-model_2026-01-02_daily_v1", "2026-01-02",
		"current-model fact: the answer is 42", strPtr("fake:fake-embed"))

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember the answer?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if !strings.Contains(result.ContextMessage, "current-model fact: the answer is 42") {
		t.Errorf("context message missing the current-embedding-model summary, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "stale-model fact: the answer is 999") {
		t.Errorf("context message includes a summary embedded under a different, no-longer-active model — it should have been filtered out, got: %q", result.ContextMessage)
	}
}

// TestRetrieve_SummaryVectorSearchIncludesNullEmbeddingModel confirms the
// filter doesn't regress pre-migration rows: embedding_model is nullable
// with no backfill (schema/0013_embedding_model_tracking.sql), so a row
// predating that column must still be searchable — "unknown" is not the
// same claim as "known to be a different model."
func TestRetrieve_SummaryVectorSearchIncludesNullEmbeddingModel(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-model-null"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummaryWithEmbeddingModel(t, s, scope, "sum_test-embed-model-null_2026-01-01_daily_v1", "2026-01-01",
		"pre-migration fact: the answer is 7", nil)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember the answer?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "pre-migration fact: the answer is 7") {
		t.Errorf("context message missing the null-embedding_model summary — a legacy pre-migration row must still be searchable, got: %q", result.ContextMessage)
	}
}

// TestRetrieve_EntityVectorSearchExcludesDifferentEmbeddingModel is the
// entity-search counterpart of the summary test above — same real bug,
// same fix, different table.
func TestRetrieve_EntityVectorSearchExcludesDifferentEmbeddingModel(t *testing.T) {
	t.Setenv("HUPI_ENABLE_KEYWORD_SEARCH", "false")
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-model-entity"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// Deliberately nonsense, disjoint names/ids/attrs — sharing no
	// substring with the query also keeps stage1EntityMatches (a plain
	// substring check, independent of embeddings and of the keyword-search
	// toggle above) from surfacing either entity by name, isolating this
	// test to vector search specifically.
	insertEntityWithEmbeddingModel(t, s, scope, "skill:zqlorp-stale", "skill", "Zqlorp Stale",
		`{"note": "quxx"}`, strPtr("old-vendor:old-model"))
	insertEntityWithEmbeddingModel(t, s, scope, "skill:zqlorp-current", "skill", "Zqlorp Current",
		`{"note": "quxx"}`, strPtr("fake:fake-embed"))

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you recall my favorite gadget?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !strings.Contains(result.ContextMessage, "Zqlorp Current") {
		t.Errorf("context message missing the current-embedding-model entity, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "Zqlorp Stale") {
		t.Errorf("context message includes an entity embedded under a different, no-longer-active model — it should have been filtered out, got: %q", result.ContextMessage)
	}
}

// TestRetrieve_EpisodeVectorSearchExcludesDifferentEmbeddingModel is the
// episode-search counterpart of the summary test above.
func TestRetrieve_EpisodeVectorSearchExcludesDifferentEmbeddingModel(t *testing.T) {
	t.Setenv("HUPI_ENABLE_KEYWORD_SEARCH", "false")
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-model-episode"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEpisodeWithEmbeddingModel(t, s, scope,
		"ep_test-embed-model-episode_1",
		"stale-model-episode: zqlorp value banana",
		"ok",
		strPtr("old-vendor:old-model"))
	insertEpisodeWithEmbeddingModel(t, s, scope,
		"ep_test-embed-model-episode_2",
		"current-model-episode: zqlorp value mango",
		"ok",
		strPtr("fake:fake-embed"))

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you recall my favorite gadget?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if !strings.Contains(result.ContextMessage, "current-model-episode") {
		t.Errorf("context message missing the current-embedding-model episode, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "stale-model-episode") {
		t.Errorf("context message includes an episode embedded under a different, no-longer-active model — it should have been filtered out, got: %q", result.ContextMessage)
	}
}
