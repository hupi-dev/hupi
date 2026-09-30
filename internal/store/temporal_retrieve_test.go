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

// insertEntityWithLastUpdated is insertEntity's own technique
// (scope_isolation_test.go) plus an explicit last_updated, needed to test
// stage1EntityMatches' timeframe filter — the plain insertEntity helper
// relies on last_updated's own current_date default, which can't
// simulate an entity dated in the past relative to a test's own fixed
// `now`.
func insertEntityWithLastUpdated(t *testing.T, s *Store, scope identity.Scope, id, kind, name string, lastUpdated time.Time) {
	t.Helper()
	enc, _, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt test entity attrs: %v", err)
	}
	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, first_seen, last_updated, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5, $5, $6, $7)
		`, id, kind, name, attrsCT, lastUpdated, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity: %v", err)
	}
}

// insertEntityWithEmbeddingAndLastUpdated is insertEntityWithLastUpdated
// plus a content-independent fake embedding (same technique
// insertEntityWithEmbedding in retrieve_test.go uses), needed to exercise
// vectorSearchEntities specifically — that path requires embedding is
// not null, unlike stage1EntityMatches/keywordSearchEntities.
func insertEntityWithEmbeddingAndLastUpdated(t *testing.T, s *Store, scope identity.Scope, id, kind, name string, lastUpdated time.Time) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt test entity attrs: %v", err)
	}
	vec := make([]float32, 1536)
	vec[0] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)
	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, first_seen, last_updated, scope_kind, scope_owner, key_version, embedding)
			values ($1, $2, $3, $4, $5, $5, $6, $7, $8, $9::vector)
		`, id, kind, name, attrsCT, lastUpdated, scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity: %v", err)
	}
}

// TestFusedSearchSummaries_ExcludesOutOfTimeframeCandidateEntirely is the
// answer-time reasoning follow-up to Phase E's own temporal-relevance
// boost (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): real-verified against
// the adversarial "AI conference" case that a ranking boost alone wasn't
// enough — even after it correctly raised the right candidate's fused
// score, and even after mmrSelect's ordering fix correctly put it first
// in context, the answering model still picked the temporally-wrong
// summary because it sat right there in context with a closer lexical
// match. This test mirrors that exact scenario and confirms the
// temporally-wrong one is now excluded from context entirely, not merely
// deprioritized.
func TestFusedSearchSummaries_ExcludesOutOfTimeframeCandidateEntirely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-timeframe-filter"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummary(t, s, scope, "sum_test-timeframe-filter_2024-01-15_daily_v1", "2024-01-15",
		"The user attended an AI conference on 2024-01-15. Neural networks and deep learning were covered in depth.", "")
	insertSummary(t, s, scope, "sum_test-timeframe-filter_2024-05-15_daily_v1", "2024-05-15",
		"The user attended a robotics-focused event downtown on 2024-05-15. Actuators and control systems were highlighted.", "")

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "Neural networks") {
		t.Errorf("context includes the January (out-of-timeframe) summary's content — it should have been excluded entirely, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Actuators and control systems") {
		t.Errorf("context missing the May (in-timeframe, correct) summary's content, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "dated 2024-05-15") || !strings.Contains(result.ContextMessage, "3 weeks before now") {
		t.Errorf("context missing the computed relative-date label for the May summary, got: %q", result.ContextMessage)
	}
}

// TestFusedSearchSummaries_TimeframeFilterBacksOffWhenNothingSurvives
// confirms the safe-degrade path: if a confidently-resolved timeframe
// would exclude every candidate (e.g. consolidation never ran for the
// implied period), the filter must not return an empty context — it
// backs off to the unfiltered, boost-only behavior instead. Same
// direction as groundingCheck's own count-mismatch handling elsewhere in
// this codebase: never destroy information on an unclear signal.
func TestFusedSearchSummaries_TimeframeFilterBacksOffWhenNothingSurvives(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-timeframe-filter-backoff"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// Neither summary's period overlaps "last month" relative to `now`
	// below (June) — January and May are both out of scope.
	insertSummary(t, s, scope, "sum_test-timeframe-filter-backoff_2024-01-15_daily_v1", "2024-01-15",
		"The user attended an AI conference on 2024-01-15. Neural networks and deep learning were covered in depth.", "")
	insertSummary(t, s, scope, "sum_test-timeframe-filter-backoff_2024-05-15_daily_v1", "2024-05-15",
		"The user attended a robotics-focused event downtown on 2024-05-15. Actuators and control systems were highlighted.", "")

	now := time.Date(2024, 7, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to June — neither summary overlaps
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q — the filter backing off should still surface something (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Neural networks") && !strings.Contains(result.ContextMessage, "Actuators and control systems") {
		t.Errorf("context has neither summary's content — the timeframe filter should have backed off rather than excluding everything, got: %q", result.ContextMessage)
	}
}

// TestStage1EntityMatches_ExcludesOutOfTimeframeEntityEntirely is the
// third, distinct retrieval path this same investigation found still
// needed the timeframe hard filter after the summary and episode paths
// were already fixed: a directly name-matched entity
// (stage1EntityMatches) was rendered unconditionally, with no date
// awareness at all, and was exactly what kept asserting a
// temporally-wrong date in the real Phase E adversarial case even after
// both other paths correctly excluded their own equivalent content. This
// mirrors that case directly: an entity literally named "AI conference,"
// dated January, against a query implying "last month" (May).
//
// Includes a second, May-dated entity as an in-timeframe control —
// unlike the other two paths, this one deliberately does NOT back off
// when excluding would leave nothing (see this test's own sibling below
// for why), so this control is what confirms real exclusion happened,
// not just an empty result for an unrelated reason.
func TestStage1EntityMatches_ExcludesOutOfTimeframeEntityEntirely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entity-timeframe-filter"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithLastUpdated(t, s, scope, "project:ai-conference-2024-01-15", "project", "AI conference",
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))
	insertEntityWithLastUpdated(t, s, scope, "project:robotics-event-2024-05-15", "project", "robotics event",
		time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC))

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference or the robotics event I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if strings.Contains(result.ContextMessage, "AI conference") {
		t.Errorf("context includes the January (out-of-timeframe) entity — it should have been excluded entirely, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "robotics event") {
		t.Errorf("context missing the May (in-timeframe, control) entity, got: %q", result.ContextMessage)
	}
}

// TestStage1EntityMatches_TimeframeFilterExcludesEvenTheOnlyMatch
// confirms the entity path's deliberate asymmetry with the other two
// (summary/episode) paths: it does NOT back off to the unfiltered set
// when excluding would leave nothing. This is the real, motivating case
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md) — the Phase E adversarial
// scenario's entity match was exactly one candidate whose only date was
// wrong, so a backoff here would restore precisely the match this fix
// exists to remove. Losing it doesn't lose all context: stage 1 entity
// matches are a coarse pre-check, not this call's main retrieval
// surface, and every other mechanism (fusedSearchSummaries,
// keywordSearchEpisodes, vectorSearchEntities) still runs afterward
// regardless.
func TestStage1EntityMatches_TimeframeFilterExcludesEvenTheOnlyMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entity-timeframe-filter-exclude-only"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithLastUpdated(t, s, scope, "project:ai-conference-2024-01-15", "project", "AI conference",
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May — the entity doesn't overlap
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about at the AI conference I attended last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// GatePartial, not GateFull: with no other content in this scope at
	// all (this test's whole point — the only thing that superficially
	// matched was correctly excluded), there's genuinely nothing left to
	// call a strong hit. That's the honest, correct outcome here, not a
	// bug — stage 2 still ran (this isn't GateSkipped either).
	if result.Gate != gateway.GatePartial {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GatePartial, result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "AI conference") {
		t.Errorf("context includes the entity — it's the only match and out of timeframe, so it should have been excluded entirely, not backed off, got: %q", result.ContextMessage)
	}
}

// TestVectorSearchEntities_ExcludesOutOfTimeframeCandidateEntirely is the
// real, fifth retrieval path this same investigation found needed the
// same fix, after entities.last_updated itself was fixed to reflect the
// consolidated date instead of real current_date
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): re-verifying the Phase E
// adversarial case with that fix in place found vectorSearchEntities —
// not stage1EntityMatches or keywordSearchEntities, both already fixed —
// was the one actually still surfacing the wrong entity, because a
// semantic (embedding) match needs no literal substring/keyword hit at
// all. The query here deliberately names neither entity literally, so
// only vectorSearchEntities' own fake-but-uniform embedding similarity
// can find them — isolating this path from the other two already-tested
// ones.
func TestVectorSearchEntities_ExcludesOutOfTimeframeCandidateEntirely(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-vector-entity-timeframe-filter"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithEmbeddingAndLastUpdated(t, s, scope, "project:ai-conference-2024-01-15", "project", "AI conference",
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))
	insertEntityWithEmbeddingAndLastUpdated(t, s, scope, "project:robotics-event-2024-05-15", "project", "robotics event",
		time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC))

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if strings.Contains(result.ContextMessage, "AI conference") {
		t.Errorf("context includes the January (out-of-timeframe) entity — it should have been excluded entirely, got: %q", result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "robotics event") {
		t.Errorf("context missing the May (in-timeframe, control) entity, got: %q", result.ContextMessage)
	}
}

// TestVectorSearchEntities_ExcludesEvenTheOnlyMatch is this path's own
// version of the same no-backoff confirmation stage1EntityMatches/
// keywordSearchEntities already have: excluding must not back off to the
// unfiltered set even when it's the only candidate, since the real
// motivating case is exactly that shape.
func TestVectorSearchEntities_ExcludesEvenTheOnlyMatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-vector-entity-timeframe-filter-exclude-only"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithEmbeddingAndLastUpdated(t, s, scope, "project:ai-conference-2024-01-15", "project", "AI conference",
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC))

	now := time.Date(2024, 6, 5, 10, 0, 0, 0, time.UTC) // "last month" resolves to May — the entity doesn't overlap
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What did I learn about last month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, now)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if strings.Contains(result.ContextMessage, "AI conference") {
		t.Errorf("context includes the entity — it's the only match and out of timeframe, so it should have been excluded entirely, not backed off, got: %q", result.ContextMessage)
	}
}
