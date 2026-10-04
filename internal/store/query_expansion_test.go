package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// contentAwareEmbedder, unlike fakeEmbedder above, returns a different
// vector per exact input string — needed to test query expansion
// meaningfully, since the whole point is proving a *paraphrase's* own
// embedding (not the original query's) is what finds a summary.
// Anything not in vectors gets a fixed fallback vector, dissimilar to
// every hand-assigned one below (axis 9, unused by any test case here).
type contentAwareEmbedder struct {
	vectors map[string][]float32
}

func (contentAwareEmbedder) Name() string   { return "fake" }
func (contentAwareEmbedder) Vendor() string { return "fake" }
func (contentAwareEmbedder) Model() string  { return "fake-embed" }

func (contentAwareEmbedder) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errFakeNotImplemented
}

func (contentAwareEmbedder) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (e contentAwareEmbedder) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	vecs := make([][]float32, len(req.Input))
	for i, in := range req.Input {
		if v, ok := e.vectors[in]; ok {
			vecs[i] = v
			continue
		}
		fallback := make([]float32, 1536)
		fallback[9] = 1
		vecs[i] = fallback
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

// fakeParaphraseProvider is a chat provider that always returns the same
// canned paraphrase JSON, regardless of what's asked — enough to test
// that generateQueryParaphrases' output actually gets embedded and
// searched, without needing a real LLM call.
type fakeParaphraseProvider struct {
	paraphrase string
}

func (fakeParaphraseProvider) Name() string   { return "fake" }
func (fakeParaphraseProvider) Vendor() string { return "fake" }
func (fakeParaphraseProvider) Model() string  { return "fake-chat" }

func (p fakeParaphraseProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{
		Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: `{"paraphrases": ["` + p.paraphrase + `"]}`,
		},
	}, nil
}

func (fakeParaphraseProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (fakeParaphraseProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errFakeNotImplemented
}

func testStoreWithEmbedder(t *testing.T, embedder provider.Provider) *Store {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test (see scope_isolation_test.go's doc comment)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
	return New(db, keys, embedder)
}

// TestFusedSearchSummaries_QueryExpansionFindsSummaryByParaphraseMatch
// confirms query expansion's whole mechanism end to end: a summary whose
// own embedding doesn't match the original query's embedding at all, but
// does match a paraphrase's embedding, is only found once expansion is
// both enabled (HUPI_ENABLE_QUERY_EXPANSION) and wired in
// (EnableQueryExpansion) — otherwise retrieval behaves exactly as before
// query expansion existed.
func TestFusedSearchSummaries_QueryExpansionFindsSummaryByParaphraseMatch(t *testing.T) {
	const originalQuery = "what was the mortgage pre-approval amount"
	const paraphrase = "how much was the Wells Fargo loan approval for"

	axis := func(i int) []float32 {
		v := make([]float32, 1536)
		v[i] = 1
		return v
	}
	embedder := contentAwareEmbedder{vectors: map[string][]float32{
		originalQuery: axis(1), // dissimilar to the summary below
		paraphrase:    axis(0), // matches the summary's own embedding exactly
	}}

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-query-expansion"}
	ctx := context.Background()
	period := "2023-11-30"
	summaryID := "sum_test-query-expansion_2023-11-30_daily_v1"

	t.Run("disabled by default: paraphrase match alone does not surface the summary", func(t *testing.T) {
		s := testStoreWithEmbedder(t, embedder)
		t.Cleanup(func() { cleanupScope(t, s, scope) })
		insertSummaryWithEmbedding(t, s, scope, summaryID, period,
			"The user discussed moving logistics, cable providers, and home insurance quotes.", axis(0))

		result, err := s.Retrieve(ctx, scope, scope, []provider.Message{{Role: provider.RoleUser, Content: originalQuery}}, time.Now())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if strings.Contains(result.ContextMessage, "moving logistics") {
			t.Errorf("summary surfaced without query expansion enabled at all (s.chatProvider is nil) — context: %q", result.ContextMessage)
		}
	})

	t.Run("wired but env var off: still unaffected", func(t *testing.T) {
		s := testStoreWithEmbedder(t, embedder)
		s.EnableQueryExpansion(fakeParaphraseProvider{paraphrase: paraphrase})
		t.Cleanup(func() { cleanupScope(t, s, scope) })
		insertSummaryWithEmbedding(t, s, scope, summaryID, period,
			"The user discussed moving logistics, cable providers, and home insurance quotes.", axis(0))

		result, err := s.Retrieve(ctx, scope, scope, []provider.Message{{Role: provider.RoleUser, Content: originalQuery}}, time.Now())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if strings.Contains(result.ContextMessage, "moving logistics") {
			t.Errorf("summary surfaced with HUPI_ENABLE_QUERY_EXPANSION unset — context: %q", result.ContextMessage)
		}
	})

	t.Run("enabled: paraphrase match surfaces the summary", func(t *testing.T) {
		t.Setenv("HUPI_ENABLE_QUERY_EXPANSION", "true")
		s := testStoreWithEmbedder(t, embedder)
		s.EnableQueryExpansion(fakeParaphraseProvider{paraphrase: paraphrase})
		t.Cleanup(func() { cleanupScope(t, s, scope) })
		insertSummaryWithEmbedding(t, s, scope, summaryID, period,
			"The user discussed moving logistics, cable providers, and home insurance quotes.", axis(0))

		result, err := s.Retrieve(ctx, scope, scope, []provider.Message{{Role: provider.RoleUser, Content: originalQuery}}, time.Now())
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if !strings.Contains(result.ContextMessage, "moving logistics") {
			t.Errorf("expected query expansion's paraphrase match to surface the summary, got context: %q", result.ContextMessage)
		}
	})
}

// TestGenerateQueryParaphrases_ParsesAndCaps confirms the chat-response
// parsing itself: paraphrases beyond queryExpansionMaxParaphrases are
// dropped, not just silently allowed through to balloon fusedSearchSummaries'
// per-variant query loop.
func TestGenerateQueryParaphrases_ParsesAndCaps(t *testing.T) {
	s := &Store{chatProvider: fakeManyParaphrasesProvider{}}
	got, err := s.generateQueryParaphrases(context.Background(), "irrelevant")
	if err != nil {
		t.Fatalf("generateQueryParaphrases: %v", err)
	}
	if len(got) != queryExpansionMaxParaphrases {
		t.Errorf("got %d paraphrases, want capped at %d: %v", len(got), queryExpansionMaxParaphrases, got)
	}
}

type fakeManyParaphrasesProvider struct{}

func (fakeManyParaphrasesProvider) Name() string   { return "fake" }
func (fakeManyParaphrasesProvider) Vendor() string { return "fake" }
func (fakeManyParaphrasesProvider) Model() string  { return "fake-chat" }

func (fakeManyParaphrasesProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{
		Message: provider.Message{
			Role:    provider.RoleAssistant,
			Content: `{"paraphrases": ["one", "two", "three", "four"]}`,
		},
	}, nil
}

func (fakeManyParaphrasesProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errors.New("not needed")
}

func (fakeManyParaphrasesProvider) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errors.New("not needed")
}
