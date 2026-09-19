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

// insertSummary seeds a summary row directly (bypassing the full
// consolidation/grounding pipeline, same technique insertEntity uses) —
// real encryption, real embedding, so retrieve()'s actual queries exercise
// it exactly as they would a genuinely consolidated summary.
func insertSummary(t *testing.T, s *Store, scope identity.Scope, id, period, text, supersedes string) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	summaryCT, err := enc.Encrypt(text)
	if err != nil {
		t.Fatalf("encrypt test summary: %v", err)
	}
	// fakeEmbedder always returns the same vector regardless of input, so
	// every summary in this test is equally "similar" to any query — good
	// enough here, since what's under test is whether a superseded row
	// gets filtered out at all, not similarity ranking between rows.
	vec := make([]float32, 1536)
	vec[0] = 1
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (
				id, period, level, status, summary, grounding_checked,
				supersedes, scope_kind, scope_owner, key_version, embedding
			) values (
				$1, $2, 'daily', 'draft', $3, true,
				$4, $5, $6, $7, $8::vector
			)
		`, id, period, summaryCT, pgfmt.Nullable(supersedes), scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

// TestRetrieve_UsesCorrectedSummaryNotSupersededOne is a regression test
// for a real bug found via manual end-to-end testing (not code review):
// vectorSearchSummaries and buildAnchor originally filtered on
// "supersedes is null" to mean "current version", but that's backwards —
// a correction (hupi-correct) writes a *new* row whose own supersedes
// points at the *old* row it replaces; the old row's supersedes stays
// null forever, since corrections never update rows in place. Filtering
// on "supersedes is null" therefore returned exactly the stale,
// corrected-away summary and silently excluded every correction ever
// made — the opposite of the intended behavior. "Current" must mean "no
// other row's supersedes points at this id".
func TestRetrieve_UsesCorrectedSummaryNotSupersededOne(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-retrieve-correction"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	insertSummary(t, s, scope, "sum_test-correction_2026-01-01_daily_v1", period,
		"the stale, since-corrected fact: the answer is 500", "")
	insertSummary(t, s, scope, "sum_test-correction_2026-01-01_daily_v2", period,
		"the corrected, current fact: the answer is 2000", "sum_test-correction_2026-01-01_daily_v1")

	// "remember" is one of stage1SignalKeywords — pushes retrieve() past
	// the cheap stage-1 skip and into the actual vector search being
	// tested here, with no entity needed to force that.
	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember the answer?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "the corrected, current fact: the answer is 2000") {
		t.Errorf("context message missing the corrected (v2) fact, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "the stale, since-corrected fact: the answer is 500") {
		t.Errorf("context message includes the superseded (v1) fact — it should have been filtered out, got: %q", result.ContextMessage)
	}
}

// insertEntityWithEmbedding seeds an entity row with an embedding set —
// scope_isolation_test.go's insertEntity doesn't, since none of its own
// tests need vector search. Real encryption, like insertSummary.
func insertEntityWithEmbedding(t *testing.T, s *Store, scope identity.Scope, id, kind, name, attrsJSON string) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(attrsJSON)
	if err != nil {
		t.Fatalf("encrypt test entity attrs: %v", err)
	}
	// Same content-independent fake vector insertSummary uses — this
	// suite tests the SQL/wiring (does vector search fire, does it
	// exclude what it should), not real semantic discrimination, which
	// needs an actual embedding model to verify meaningfully.
	vec := make([]float32, 1536)
	vec[0] = 1
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

// TestRetrieve_FindsEntityByVectorSearchWhenSubstringMatchMisses is a
// regression test for a real gap found via live end-to-end testing:
// stage1EntityMatches only ever finds an entity if the query literally
// contains its name or id-slug as a substring. "What language do I
// prefer?" found nothing for an entity named "Favorite programming
// language" (value: Rust) — genuinely on record, but missed outright —
// while "What's my favorite programming language?" only worked because
// that phrase happens to restate the entity's exact name. Measuring real
// embedding similarity for both phrasings against that entity showed
// vector search wasn't the differentiator either (both well below any
// reasonable threshold against the *summary* text) — the actual fix is
// giving entities their own embedding and vector search, independent of
// summaries, which is what this test exercises directly.
func TestRetrieve_FindsEntityByVectorSearchWhenSubstringMatchMisses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entity-vector-search"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	// Name deliberately doesn't share a substring with the query below —
	// isolates this test to the vector-search path, since stage1EntityMatches
	// would otherwise find this one by substring alone and the test
	// wouldn't prove anything about the new code.
	insertEntityWithEmbedding(t, s, scope, "preference:favorite-language", "preference", "Favorite programming language", `{"value":"Rust"}`)

	// A second entity that WOULD be found by stage1EntityMatches (its
	// name literally appears in the query) — proves vectorSearchEntities
	// correctly excludes what stage 1 already matched, not just that it
	// finds new things.
	insertEntityWithEmbedding(t, s, scope, "skill:rust", "skill", "Rust", `{"role":"favorite"}`)

	// A self_model entity — must never surface via vector search
	// regardless of similarity, since it's unconditionally handled by
	// buildAnchor instead (see stage1EntityMatches' same exclusion).
	insertEntityWithEmbedding(t, s, scope, "self_model:primary", "self_model", "Self model", `{"tone":"terse"}`)

	// "prefer" is a stage1SignalKeywords entry (pushes past the cheap
	// skip into stage 2) and "Rust" makes the second entity a literal
	// stage-1 substring match — neither word is anywhere in the first
	// entity's name, so that one can only be found by vector search.
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What language do I prefer? (mentioning Rust here)"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Favorite programming language") {
		t.Errorf("context message missing the vector-search-only entity, got: %q", result.ContextMessage)
	}
	if got := strings.Count(result.ContextMessage, "skill:rust"); got != 1 {
		t.Errorf("stage-1-matched entity appeared %d times in context, want exactly 1 (vectorSearchEntities should have excluded it as a duplicate), got: %q", got, result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "self_model:primary") || strings.Contains(result.ContextMessage, "Self model") {
		t.Errorf("self_model entity leaked into vector-searched context, got: %q", result.ContextMessage)
	}
}

// TestStage1QuestionSignal is a pure unit test for the helper added
// alongside stage1KeywordSignal after live testing found real recall
// questions ("What is my project's codename?") that contain none of
// stage1SignalKeywords' fixed phrases and were silently skipped instead
// of searched — see stage1QuestionSignal's own doc comment.
func TestStage1QuestionSignal(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"trailing question mark", "What is my project's codename?", true},
		{"leading what, no question mark", "whats my project codename", true},
		{"leading is, mixed case", "Is there a preferred language for this", true},
		{"leading can", "Can you tell me the ship date", true},
		{"leading auxiliary did", "Did we decide on a name for this", true},
		{"plain statement", "The project ships in March.", false},
		{"imperative, not a question", "Write me a poem about cats", false},
		{"empty string", "", false},
		{"whitespace only", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stage1QuestionSignal(tc.query); got != tc.want {
				t.Errorf("stage1QuestionSignal(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestRetrieve_QuestionWithoutKeywordPhraseStillSearches is a regression
// test for the same live-testing gap TestStage1QuestionSignal covers,
// but proves it end to end through Retrieve rather than just the helper
// in isolation: a query that's clearly a question but shares no
// stage1SignalKeywords phrase and no entity substring match must still
// reach stage 2 and find a real, on-record summary — before
// stage1QuestionSignal existed, this exact shape of query returned
// GateSkipped regardless of what was in memory.
func TestRetrieve_QuestionWithoutKeywordPhraseStillSearches(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-question-signal"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertSummary(t, s, scope, "sum_test-question-signal_2026-01-01_daily_v1", "2026-01-01",
		"the project's codename is Aurora and it ships in March", "")

	// Deliberately contains none of stage1SignalKeywords' fixed phrases
	// (no "remember", "recall", "what did", etc.) and no entity to match
	// by substring — the only reason this should reach stage 2 at all is
	// stage1QuestionSignal recognizing it as a question.
	messages := []provider.Message{{Role: provider.RoleUser, Content: "What is my project's codename?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if result.Gate != gateway.GateFull {
		t.Fatalf("gate = %q, want %q — a question with no stage1SignalKeywords phrase should still reach stage 2 (context: %q)", result.Gate, gateway.GateFull, result.ContextMessage)
	}
	if !strings.Contains(result.ContextMessage, "Aurora") {
		t.Errorf("context message missing the on-record fact, got: %q", result.ContextMessage)
	}
}
