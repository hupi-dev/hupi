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

// insertEpisodeWithEmbeddingAndTS is insertEpisodeWithEmbeddingModel's
// sibling, but with an explicit ts instead of relying on the schema's
// own now()-at-insert default — needed to simulate an episode dated in
// the past relative to a test's own fixed `now`, the same technique
// insertEntityWithEmbeddingAndLastUpdated (temporal_retrieve_test.go)
// uses for entities.
func insertEpisodeWithEmbeddingAndTS(t *testing.T, s *Store, scope identity.Scope, id, input, output string, ts time.Time) {
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
			insert into episodes (id, type, input_text, output_text, hash, scope_kind, scope_owner, key_version, embedding, ts)
			values ($1, 'interaction', $2, $3, $4, $5, $6, $7, $8::vector, $9)
		`, id, inputCT, outputCT, hash(id), scope.Kind, scope.Owner, keyVersion, embeddingLiteral, ts)
		return err
	})
	if err != nil {
		t.Fatalf("insert test episode %s: %v", id, err)
	}
}

// TestVectorSearchEpisodes_ExcludesOutOfTimeframeCandidateEntirely is the
// real regression test for review finding B7: vectorSearchEpisodes was
// the one retrieval path into episodes (keywordSearchEpisodes' own
// equivalent fix already covers the keyword path onto this same table)
// with no timeframe hard filter at all, so a semantically-matched
// episode (a vector match needs no literal keyword hit) could surface a
// temporally-wrong raw exchange verbatim for a "last month"-shaped
// query. The query here deliberately names neither episode's topic
// literally, so only the fake-but-uniform embedding similarity — not
// BM25/keyword matching — can find them, isolating this path.
func TestVectorSearchEpisodes_ExcludesOutOfTimeframeCandidateEntirely(t *testing.T) {
	// Isolates the vector path from keywordSearchEpisodes' own, separate
	// (and separately-already-correct) backoff behavior: once
	// vectorSearchEpisodes excludes the January episode, it also stops
	// including it in the exclude-list handed to keywordSearchEpisodes
	// (identity.Scope{} refs are only built from timeframe-surviving
	// matches) — without this, keywordSearchEpisodes can independently
	// BM25-match the same episode and, finding itself down to a single
	// candidate once the May episode is separately excluded via the
	// vector path's own exclude-list, legitimately back off and restore
	// it, masking whether vectorSearchEpisodes' own filter did its job.
	// Same technique the A3 embedding-model-filter tests already use to
	// isolate the vector path from this same real interaction.
	t.Setenv("HUPI_ENABLE_KEYWORD_SEARCH", "false")

	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-vector-episode-timeframe-filter"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEpisodeWithEmbeddingAndTS(t, s, scope,
		"ep_test-vector-episode-timeframe-filter_ai-conf",
		"Tell me about the event", "You attended an AI conference. Neural networks and deep learning were covered in depth.",
		time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC))
	insertEpisodeWithEmbeddingAndTS(t, s, scope,
		"ep_test-vector-episode-timeframe-filter_robotics",
		"Tell me about the event", "You attended a robotics-focused event downtown. Actuators and control systems were highlighted.",
		time.Date(2024, 5, 15, 9, 0, 0, 0, time.UTC))

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if strings.Contains(result.ContextMessage, "Neural networks") {
		t.Errorf("context includes the January (out-of-timeframe) episode's content — it should have been excluded entirely, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Actuators and control systems") {
		t.Errorf("context missing the May (in-timeframe, control) episode's content, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "dated 2024-05-15") {
		t.Errorf("context missing the computed relative-date label for the May episode, got: %q", result.ContextMessage)
	}
}

// TestVectorSearchEpisodes_TimeframeFilterBacksOffWhenNothingSurvives
// confirms the same safe-degrade behavior keywordSearchEpisodes/
// fusedSearchSummaries already have on this exact filter shape (unlike
// vectorSearchEntities' own deliberate no-backoff choice): excluding
// every candidate must not return an empty context, it backs off to the
// unfiltered set instead.
func TestVectorSearchEpisodes_TimeframeFilterBacksOffWhenNothingSurvives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-vector-episode-timeframe-filter-backoff"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// Neither episode's ts overlaps "last month" relative to `now` below
	// (July) — January and May are both out of scope.
	insertEpisodeWithEmbeddingAndTS(t, s, scope,
		"ep_test-vector-episode-timeframe-filter-backoff_ai-conf",
		"Tell me about the event", "You attended an AI conference. Neural networks and deep learning were covered in depth.",
		time.Date(2024, 1, 15, 9, 0, 0, 0, time.UTC))

	now := time.Date(2024, 7, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to June — the episode doesn't overlap
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q — the filter backing off should still surface something (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Neural networks") {
		t.Errorf("context missing the only episode's content — the timeframe filter should have backed off rather than excluding everything, got: %q", result.ContextMessage)
	}
}
