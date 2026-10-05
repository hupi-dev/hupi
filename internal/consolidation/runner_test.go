package consolidation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// against a real instance.

var errFakeNotImplemented = errors.New("fake provider: not implemented, not needed by these tests")

// fakeConsolidationProvider returns a fixed, valid ConsolidationOutput
// JSON payload — enough to exercise storeSummary's real transaction path
// (the thing this file's test cares about) without a real LLM.
// capturedRequests, when non-nil, records every ChatRequest this provider
// receives — used to assert on prompt *content* (e.g. that RunDaily
// actually threaded the established-record text into the prompt) without
// needing a real LLM to react to it correctly.
type fakeConsolidationProvider struct {
	response              string
	capturedRequests      *[]provider.ChatRequest
	capturedEmbedRequests *[]provider.EmbedRequest // records every batch embedKeyFacts (and embedSummary/embedEntities) sends, for asserting on batching/content
}

func (fakeConsolidationProvider) Name() string   { return "fake" }
func (fakeConsolidationProvider) Vendor() string { return "fake" }
func (fakeConsolidationProvider) Model() string  { return "fake-model" }

func (f fakeConsolidationProvider) ChatCompletion(_ context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	if f.capturedRequests != nil {
		*f.capturedRequests = append(*f.capturedRequests, req)
	}
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: f.response}}, nil
}

func (fakeConsolidationProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (f fakeConsolidationProvider) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	if f.capturedEmbedRequests != nil {
		*f.capturedEmbedRequests = append(*f.capturedEmbedRequests, req)
	}
	vecs := make([][]float32, len(req.Input))
	for i := range req.Input {
		v := make([]float32, 1536)
		v[0] = 1
		vecs[i] = v
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

func testRunner(t *testing.T, consolidationResponse, groundingResponse string) (*Runner, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	keys := crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data

	consolidationProvider := fakeConsolidationProvider{response: consolidationResponse}
	groundingProvider := fakeConsolidationProvider{response: groundingResponse}
	return New(db, keys, consolidationProvider, groundingProvider, consolidationProvider), db
}

// TestStoreSummary_WritesExpirationIntoBothKeyFactsAndMemories is the
// real-infra regression for Phase 1 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: a key fact's ExpiresAt/
// ExpireReason (from the consolidation LLM's own JSON output) must land
// in both summary_key_facts (what loadKeyFacts actually filters on) and
// its live memories mirror (what a RefKindMemory citation resolves
// back to) — and a fact with no ExpiresAt at all must leave both
// columns null, not some zero-value placeholder.
func TestStoreSummary_WritesExpirationIntoBothKeyFactsAndMemories(t *testing.T) {
	groundingJSON := `{"grounded": [{"i":1,"ok":true},{"i":2,"ok":true}]}`
	runner, db := testRunner(t, "", groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-fact-expiration"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-01-15",
		output: ConsolidationOutput{
			Summary: "Dana has an appointment coming up.",
			KeyFacts: []KeyFactOutput{
				{Fact: "Dana has a dentist appointment on 2026-01-20.", ExpiresAt: "2026-01-20", ExpireReason: "one-time appointment"},
				{Fact: "Dana prefers morning appointments."},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("storeSummary: %v", err)
	}

	var summaryID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-01-15' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryID)
	}); err != nil {
		t.Fatalf("load stored summary id: %v", err)
	}

	type factRow struct {
		id                      int64
		expiresAt, expireReason sql.NullString
	}
	var facts []factRow
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, expires_at::text, expire_reason from summary_key_facts where summary_id = $1 order by id
		`, summaryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var fr factRow
			if err := rows.Scan(&fr.id, &fr.expiresAt, &fr.expireReason); err != nil {
				return err
			}
			facts = append(facts, fr)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load key facts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("got %d key facts, want 2", len(facts))
	}

	expiring, plain := facts[0], facts[1]
	if !expiring.expiresAt.Valid || expiring.expiresAt.String != "2026-01-20" {
		t.Errorf("summary_key_facts expires_at = %v, want 2026-01-20", expiring.expiresAt)
	}
	if !expiring.expireReason.Valid || expiring.expireReason.String != "one-time appointment" {
		t.Errorf("summary_key_facts expire_reason = %v, want \"one-time appointment\"", expiring.expireReason)
	}
	if plain.expiresAt.Valid || plain.expireReason.Valid {
		t.Errorf("fact with no stated expiration has expires_at=%v expire_reason=%v, want both null", plain.expiresAt, plain.expireReason)
	}

	// The live memories mirror must carry the exact same expiration —
	// this is what a RefKindMemory citation (and, in a later phase,
	// retrieval filtering) actually reads.
	var memExpiresAt, memExpireReason sql.NullString
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select expires_at::text, expire_reason from memories where id = $1
		`, "mem_fact_"+strconv.FormatInt(expiring.id, 10)).Scan(&memExpiresAt, &memExpireReason)
	}); err != nil {
		t.Fatalf("load memories mirror for expiring fact: %v", err)
	}
	if !memExpiresAt.Valid || memExpiresAt.String != "2026-01-20" {
		t.Errorf("memories expires_at = %v, want 2026-01-20", memExpiresAt)
	}
	if !memExpireReason.Valid || memExpireReason.String != "one-time appointment" {
		t.Errorf("memories expire_reason = %v, want \"one-time appointment\"", memExpireReason)
	}
}

// TestRunDaily_WritesGroundedSummary exercises storeSummary's real
// transaction path end to end (insert summary, insert key facts, upsert
// the touched entity, then embed) — the part of this package's
// dbscope/hardening refactor no existing test otherwise touched.
func TestRunDaily_WritesGroundedSummary(t *testing.T) {
	consolidationJSON := `{
		"summary": "Discussed the HUPI project.",
		"key_facts": [{"fact": "Decided to use pgvector", "source_episode_ids": ["ep_test_run_daily"]}],
		"entities_touched": [{"id": "project:hupi", "kind": "project", "name": "HUPI", "attributes": {"vector_index": "pgvector"}}]
	}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-daily"}
	ctx := context.Background()
	// Cleanup, the seed insert, and every verification read below must run
	// inside a scoped transaction (dbscope.Run) — under RLS
	// (docs/HARDENING_PLAN.md D2), an unscoped write is rejected outright
	// and an unscoped read silently sees zero rows, either of which would
	// make this test meaningless rather than just fail loudly.
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	// Use the same KeyStore-resolved encryptor the Runner itself will use
	// for this scope — a standalone Encryptor built from a different key
	// would encrypt the seed data under a key nothing else can decrypt.
	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what should we use for the vector index?")
	outputCT, _ := enc.Encrypt("let's use pgvector")
	date := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_run_daily', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var summaryID string
	var groundingChecked bool
	var summaryCT []byte
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id, grounding_checked, summary from summaries
			where level = 'daily' and period = '2026-09-09' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryID, &groundingChecked, &summaryCT)
	})
	if err != nil {
		t.Fatalf("expected a daily summary to be written: %v", err)
	}
	if !groundingChecked {
		t.Error("expected grounding_checked = true")
	}
	text, err := enc.Decrypt(summaryCT)
	if err != nil {
		t.Fatalf("decrypt summary: %v", err)
	}
	if text != "Discussed the HUPI project." {
		t.Errorf("summary text = %q, want the seeded consolidation output", text)
	}

	// summary_key_facts gained its own scope columns and RLS in
	// schema/0018 (docs/CODEBASE_SURVEY_AND_REVIEW.md B18) — a scoped
	// query is required now, not optional.
	var grounded bool
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select grounded from summary_key_facts where summary_id = $1`, summaryID).Scan(&grounded)
	})
	if err != nil {
		t.Fatalf("expected a key_facts row: %v", err)
	}
	if !grounded {
		t.Error("expected the key fact to be marked grounded")
	}

	var entityName string
	var attrsCT []byte
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select name, attributes from entities where id = 'project:hupi' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&entityName, &attrsCT)
	})
	if err != nil {
		t.Fatalf("expected the touched entity to be upserted: %v", err)
	}
	if entityName != "HUPI" {
		t.Errorf("entity name = %q, want HUPI", entityName)
	}

	var embeddingCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from summaries where id = $1 and embedding is not null`, summaryID).Scan(&embeddingCount)
	})
	if err != nil {
		t.Fatalf("check embedding: %v", err)
	}
	if embeddingCount != 1 {
		t.Error("expected the summary to have been embedded")
	}
}

// TestRunDaily_EmbedsOnlyGroundedKeyFactsInOneBatchedCall is the
// consolidation-side counterpart of the semantic fact-ranking tests in
// internal/store — confirms embedKeyFacts (schema/0021) actually runs as
// part of a normal RunDaily: exactly one batched Embed call carrying only
// the grounded facts' text (ungrounded facts are never retrieved, so
// embedding them would be pure waste — see embedKeyFacts' own doc
// comment), and the resulting summary_key_facts rows have an
// embedding/embedding_model recorded only for the grounded ones.
func TestRunDaily_EmbedsOnlyGroundedKeyFactsInOneBatchedCall(t *testing.T) {
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	keys := crypto.NewKeyStore(db, make([]byte, 32))

	consolidationJSON := `{
		"summary": "Discussed two topics.",
		"key_facts": [
			{"fact": "The user decided to use pgvector.", "source_episode_ids": ["ep_test_embed_facts"]},
			{"fact": "A hallucinated, ungrounded fact.", "source_episode_ids": ["ep_test_embed_facts"]}
		],
		"entities_touched": []
	}`
	// Only the first fact is grounded — grounding.go's real groundingCheck
	// is bypassed by this fake, but storeSummary's own handling of the
	// grounded[] result is exactly what's under test here.
	groundingJSON := `{"grounded": [{"i":1,"ok":true}, {"i":2,"ok":false}]}`
	var embedRequests []provider.EmbedRequest
	fake := fakeConsolidationProvider{response: consolidationJSON, capturedEmbedRequests: &embedRequests}
	groundingFake := fakeConsolidationProvider{response: groundingJSON}
	runner := New(db, keys, fake, groundingFake, fake)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-key-facts"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what should we use for the vector index?")
	outputCT, _ := enc.Encrypt("let's use pgvector")
	date := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_embed_facts', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	// embedSummary makes its own, separate Embed call — only the *second*
	// captured request (summary first, per storeSummary's call order) is
	// embedKeyFacts' own batch.
	var factEmbedReq *provider.EmbedRequest
	for i := range embedRequests {
		if len(embedRequests[i].Input) == 1 && embedRequests[i].Input[0] == "The user decided to use pgvector." {
			factEmbedReq = &embedRequests[i]
		}
	}
	if factEmbedReq == nil {
		t.Fatalf("no Embed call carried the grounded fact's text; captured requests: %+v", embedRequests)
	}
	if len(factEmbedReq.Input) != 1 {
		t.Errorf("key-facts Embed request had %d inputs, want exactly 1 (only the grounded fact — the ungrounded one must never be embedded)", len(factEmbedReq.Input))
	}

	var summaryID string
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-09-10' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryID)
	})
	if err != nil {
		t.Fatalf("expected a daily summary: %v", err)
	}

	type factRow struct {
		grounded       bool
		hasEmbedding   bool
		embeddingModel sql.NullString
	}
	var rows []factRow
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		r, err := tx.QueryContext(ctx, `select grounded, embedding is not null, embedding_model from summary_key_facts where summary_id = $1`, summaryID)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var fr factRow
			if err := r.Scan(&fr.grounded, &fr.hasEmbedding, &fr.embeddingModel); err != nil {
				return err
			}
			rows = append(rows, fr)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("load key fact rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d key fact rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.grounded && !r.hasEmbedding {
			t.Errorf("grounded fact row has no embedding, want one written by embedKeyFacts")
		}
		if !r.grounded && r.hasEmbedding {
			t.Errorf("ungrounded fact row has an embedding, want none (never retrieved, so never worth embedding)")
		}
	}
}

// failingEmbedder wraps fakeConsolidationProvider but always fails Embed
// — used by TestRunDaily_SucceedsDespiteEmbeddingFailure to simulate a
// transient embedding-provider outage while consolidation and grounding
// still succeed normally.
type failingEmbedder struct {
	fakeConsolidationProvider
}

func (failingEmbedder) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResponse, error) {
	return provider.EmbedResponse{}, errors.New("simulated embedding provider outage")
}

// TestRunDaily_SucceedsDespiteEmbeddingFailure is a real regression test
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A6): embedSummary,
// embedEntities, embedHighImportanceEpisodes, and the entity-embedding
// backfill are all documented as best-effort — a failure shouldn't fail
// consolidation itself, since the summary/key-facts/entities are already
// durably stored by the time any of them run. The code used to return
// these errors anyway, making a transient embedding outage register as a
// failed RunDaily (a false alarm) and, because RunDaily returned
// immediately, silently skip real downstream work (the entity-embedding
// backfill, reachable only after embedHighImportanceEpisodes). This test
// uses an embedder that fails on every call and confirms RunDaily still
// returns nil — if any of the now-fixed call sites still propagated its
// error, this would fail.
func TestRunDaily_SucceedsDespiteEmbeddingFailure(t *testing.T) {
	consolidationJSON := `{
		"summary": "Discussed the HUPI project.",
		"key_facts": [{"fact": "Decided to use pgvector", "source_episode_ids": ["ep_test_embed_fail"]}],
		"entities_touched": [{"id": "project:hupi-embed-fail", "kind": "project", "name": "HUPI", "attributes": {"vector_index": "pgvector"}}]
	}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`

	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	keys := crypto.NewKeyStore(db, make([]byte, 32))

	consolidationProvider := fakeConsolidationProvider{response: consolidationJSON}
	groundingProvider := fakeConsolidationProvider{response: groundingJSON}
	runner := New(db, keys, consolidationProvider, groundingProvider, failingEmbedder{})

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-daily-embed-fail"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what should we use for the vector index?")
	outputCT, _ := enc.Encrypt("let's use pgvector")
	date := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_embed_fail', $1, 'interaction', $2, $3, 'sha256:test', 0.9, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v, want nil — a failed embed must not fail consolidation (finding A6)", err)
	}

	// The summary itself must still be durably stored despite every
	// embed call failing.
	var summaryCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where level = 'daily' and period = '2026-09-09' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&summaryCount)
	})
	if err != nil {
		t.Fatalf("check summary: %v", err)
	}
	if summaryCount != 1 {
		t.Error("expected the daily summary to be durably stored even though embedding failed")
	}
}

// TestRunDaily_SetsEntityDatesToTheSimulatedDateNotRealNow is the real,
// confirmed fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): upsertEntities
// previously stamped every touched entity's first_seen/last_updated with
// Postgres's own current_date — the real wall-clock day this test
// actually runs on, not the (deliberately backdated, far in the past)
// date RunDaily is asked to consolidate. Uses a date years in the past
// specifically so a real regression back to current_date can't pass by
// coincidence.
func TestRunDaily_SetsEntityDatesToTheSimulatedDateNotRealNow(t *testing.T) {
	consolidationJSON := `{
		"summary": "Attended an AI conference.",
		"key_facts": [{"fact": "The user attended an AI conference.", "source_episode_ids": ["ep_test_entity_date"]}],
		"entities_touched": [{"id": "project:ai-conference", "kind": "project", "name": "AI conference", "attributes": {"topic": "neural networks"}}]
	}`
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entity-date"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("I learned so much at the AI conference today")
	outputCT, _ := enc.Encrypt("That sounds fantastic!")
	simulatedDate := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_entity_date', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, simulatedDate, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, simulatedDate); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var firstSeen, lastUpdated time.Time
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select first_seen, last_updated from entities where id = 'project:ai-conference' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&firstSeen, &lastUpdated)
	})
	if err != nil {
		t.Fatalf("expected the touched entity to be upserted: %v", err)
	}
	wantDate := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if !firstSeen.Equal(wantDate) {
		t.Errorf("entity first_seen = %v, want %v (the simulated date, not real today)", firstSeen, wantDate)
	}
	if !lastUpdated.Equal(wantDate) {
		t.Errorf("entity last_updated = %v, want %v (the simulated date, not real today)", lastUpdated, wantDate)
	}
}

// TestRunDaily_EmbedsHighImportanceEpisode is a real regression test for
// a bug docs/BENCHMARK_IMPROVEMENT_PLAN.md step 5's textSource.date
// change introduced but no existing test caught: embedHighImportanceEpisodes
// has its own SQL query (separate from loadDailyEpisodes/loadEpisodesByID)
// that also feeds scanEpisodeSources, and step 5 added a 5th scanned
// column (ts) to that shared helper without updating this third,
// easy-to-miss call site's own SELECT to also fetch it — a real
// "sql: expected 4 destination arguments in Scan, not 5" failure on any
// episode actually meeting the importance threshold, silently swallowed
// by RunDaily's own "continue past a failed scope" resilience (found via
// a real full-scale benchmark re-run, not code review). Every existing
// episode fixture in this file hardcodes importance=0.5, below
// EpisodeEmbedImportanceThreshold (0.6) -- so embedHighImportanceEpisodes'
// query always matched zero rows in every other test here, never
// actually exercising this path. This one seeds importance=0.8
// specifically so it does.
func TestRunDaily_EmbedsHighImportanceEpisode(t *testing.T) {
	consolidationJSON := `{"summary": "Unrelated daily content.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-high-importance-embed"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	inputCT, _ := enc.Encrypt("what's the most important decision we made today?")
	outputCT, _ := enc.Encrypt("choosing pgvector for the vector index")
	date := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_high_importance', $1, 'interaction', $2, $3, 'sha256:test', 0.8, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed high-importance episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var embeddingCount int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from episodes where id = 'ep_test_high_importance' and embedding is not null`).Scan(&embeddingCount)
	})
	if err != nil {
		t.Fatalf("check episode embedding: %v", err)
	}
	if embeddingCount != 1 {
		t.Error("expected the high-importance episode to have been embedded — embedHighImportanceEpisodes either errored (the real regression this test catches) or silently skipped it")
	}
}

// TestRunRollup_IdempotentAcrossReruns exercises the guard
// docs/GAP_CLOSURE_PLAN.md §4.1 added ahead of a cron scheduler calling
// this on a fixed calendar boundary: a second call for a period that
// already rolled up must not create a second, undifferentiated version.
func TestRunRollup_IdempotentAcrossReruns(t *testing.T) {
	consolidationJSON := `{
		"summary": "Rolled up the week.",
		"key_facts": [],
		"entities_touched": []
	}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-rollup"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	dailyCT, _ := enc.Encrypt("daily summary text")
	sourcePeriods := []string{"2026-09-07", "2026-09-08"}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, p := range sourcePeriods {
			if _, err := tx.ExecContext(ctx, `
				insert into summaries (id, period, level, summary, scope_kind, scope_owner)
				values ($1, $2, 'daily', $3, $4, $5)
			`, "sum_test_"+p, p, dailyCT, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed daily summaries: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
			t.Fatalf("RunRollup call %d: %v", i+1, err)
		}
	}

	var count int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count weekly summaries: %v", err)
	}
	if count != 1 {
		t.Errorf("got %d weekly summaries for 2026-W37 after two RunRollup calls, want exactly 1", count)
	}
}

// TestRunRollup_RegeneratesWhenSourceChangesAfterward is Phase D item 4's
// real fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a source day's
// current summary can legitimately change after its own weekly rollup
// was already generated — a later, artificially-timestamped daily
// summary version (simulating RunDaily's own re-consolidation, or a
// Phase C correction) must make the existing rollup stale, and a second
// RunRollup call must regenerate and supersede it, not silently stay a
// no-op the way TestRunRollup_IdempotentAcrossReruns confirms it should
// when nothing actually changed.
func TestRunRollup_RegeneratesWhenSourceChangesAfterward(t *testing.T) {
	consolidationJSON := `{"summary": "Rolled up the week.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-rollup-stale"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	dailyCT, _ := enc.Encrypt("daily summary text")
	sourcePeriods := []string{"2026-09-07", "2026-09-08"}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, p := range sourcePeriods {
			if _, err := tx.ExecContext(ctx, `
				insert into summaries (id, period, level, summary, scope_kind, scope_owner)
				values ($1, $2, 'daily', $3, $4, $5)
			`, "sum_stale_test_"+p, p, dailyCT, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daily summaries: %v", err)
	}

	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("first RunRollup: %v", err)
	}

	// Simulate a later re-consolidation of one source day: a fresh
	// current row, artificially timestamped well after the rollup above,
	// so this test is deterministic rather than racing real wall-clock
	// granularity.
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, created_at)
			values ($1, $2, 'daily', $3, $4, $5, now() + interval '1 hour')
		`, "sum_stale_test_2026-09-07_v2", "2026-09-07", dailyCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed later daily summary version: %v", err)
	}

	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("second RunRollup (should regenerate, source changed): %v", err)
	}

	var current, total int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from summaries where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&total); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries s where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		`, scope.Kind, scope.Owner).Scan(&current)
	}); err != nil {
		t.Fatalf("count weekly summaries: %v", err)
	}
	if total != 2 {
		t.Errorf("got %d total weekly summaries for 2026-W37, want 2 (original + regenerated)", total)
	}
	if current != 1 {
		t.Errorf("got %d current weekly summaries for 2026-W37, want exactly 1 (the regenerated one superseding the original)", current)
	}
}

// TestRunDaily_FirstSummaryForGapDayRefreshesExistingRollup is a real
// regression test (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A7):
// RunDaily used to only call refreshRollupsCovering when
// existingCurrentID != "" — reasoning that a day's first-ever summary
// couldn't leave an existing rollup stale, since nothing existed to be
// stale before. That misses exactly the scenario here: a week's rollup
// already ran with "2026-09-08" as a genuine gap day (no episodes, no
// daily summary yet) — weeklyRollup's own fixed 7-day calendar template
// means the rollup's source_summary_periods already lists 2026-09-08
// even though it contributed nothing. When that day's episodes are
// later captured (an out-of-order import, a manual re-run, a late
// sync) and RunDaily gives it its first-ever summary, the existing
// rollup is now provably stale (its sources changed) but the old guard
// never even tried to check.
func TestRunDaily_FirstSummaryForGapDayRefreshesExistingRollup(t *testing.T) {
	consolidationJSON := `{"summary": "Rolled up the week.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-gap-day-refresh"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}

	// Only 2026-09-07 has a daily summary when the week first rolls up —
	// 2026-09-08 is a genuine gap day (no episodes at all yet).
	dailyCT, _ := enc.Encrypt("daily summary text")
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ($1, $2, 'daily', $3, $4, $5)
		`, "sum_gap_test_2026-09-07", "2026-09-07", dailyCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed the one real daily summary: %v", err)
	}

	sourcePeriods := []string{"2026-09-07", "2026-09-08"} // weeklyRollup's own fixed 7-day template, truncated here to the two days this test cares about
	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("seed weekly rollup over the gap week: %v", err)
	}

	// Now the gap day's episodes arrive (out-of-order capture, backfill,
	// whatever the real cause) and RunDaily gives it its first-ever
	// summary — existingCurrentID is "" going into this call.
	inputCT, _ := enc.Encrypt("what happened on the gap day?")
	outputCT, _ := enc.Encrypt("here's what happened")
	gapDate := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_gap_day', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, gapDate, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed gap day episode: %v", err)
	}
	if err := runner.RunDaily(ctx, scope, gapDate); err != nil {
		t.Fatalf("RunDaily for the gap day: %v", err)
	}

	// The existing weekly rollup must have been regenerated: two total
	// rows (original + regenerated), exactly one current.
	var total, current int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from summaries where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&total); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries s where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		`, scope.Kind, scope.Owner).Scan(&current)
	}); err != nil {
		t.Fatalf("count weekly summaries: %v", err)
	}
	if total != 2 {
		t.Errorf("got %d total weekly summaries for 2026-W37, want 2 (original + regenerated after the gap day's first summary) — the rollup was never refreshed", total)
	}
	if current != 1 {
		t.Errorf("got %d current weekly summaries for 2026-W37, want exactly 1", current)
	}
}

// TestRefreshRollupsCovering_FindsAndRegeneratesExistingRollup is Phase D
// item 4's other real half: the query that finds *which* already-existing
// rollups cover a corrected period (RunDaily and checkOneRelatedSummary
// both trigger this after a correction, since nothing in the natural cron
// cadence ever revisits a past calendar period on its own).
func TestRefreshRollupsCovering_FindsAndRegeneratesExistingRollup(t *testing.T) {
	consolidationJSON := `{"summary": "Rolled up the week.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-refresh-rollups-covering"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	dailyCT, _ := enc.Encrypt("daily summary text")
	sourcePeriods := []string{"2026-09-07", "2026-09-08"}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, p := range sourcePeriods {
			if _, err := tx.ExecContext(ctx, `
				insert into summaries (id, period, level, summary, scope_kind, scope_owner)
				values ($1, $2, 'daily', $3, $4, $5)
			`, "sum_refresh_test_"+p, p, dailyCT, scope.Kind, scope.Owner); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed daily summaries: %v", err)
	}
	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("seed weekly rollup: %v", err)
	}

	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, created_at)
			values ($1, $2, 'daily', $3, $4, $5, now() + interval '1 hour')
		`, "sum_refresh_test_2026-09-07_v2", "2026-09-07", dailyCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed later daily summary version: %v", err)
	}

	// The real trigger under test: given only the corrected level+period,
	// this must find the existing weekly rollup on its own (via
	// entities_touched-style source_summary_periods overlap) and
	// regenerate it — the caller never names "weekly" or "2026-W37"
	// itself.
	runner.refreshRollupsCovering(ctx, scope, "daily", "2026-09-07")

	var current int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries s where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		`, scope.Kind, scope.Owner).Scan(&current)
	}); err != nil {
		t.Fatalf("count current weekly summaries: %v", err)
	}
	if current != 1 {
		t.Errorf("got %d current weekly summaries after refreshRollupsCovering, want exactly 1", current)
	}

	var regeneratedSummary []byte
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select summary from summaries s where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		`, scope.Kind, scope.Owner).Scan(&regeneratedSummary)
	}); err != nil {
		t.Fatalf("load regenerated weekly summary: %v", err)
	}
	dec, err := enc.Decrypt(regeneratedSummary)
	if err != nil {
		t.Fatalf("decrypt regenerated summary: %v", err)
	}
	if dec != "Rolled up the week." {
		t.Errorf("regenerated weekly summary = %q, want the fresh consolidation output, not a stale copy", dec)
	}
}

// TestCorrect_WritesAuditLogWithGivenActor checks the one place actor
// isn't systemActor: a correction is always a deliberate human action
// (docs/GAP_CLOSURE_PLAN.md §4.3), so it must be attributed to whoever
// cmd/hupi-correct's -actor flag says, not "system:consolidation".
func TestCorrect_WritesAuditLogWithGivenActor(t *testing.T) {
	consolidationJSON := `{"summary": "Original weekly summary.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-correct-audit"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
		_, _ = db.Exec(`delete from audit_log where workspace_scope_owner = $1`, scope.Owner)
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	dailyCT, _ := enc.Encrypt("daily summary text")
	sourcePeriods := []string{"2026-09-07"}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ('sum_test_daily_2026-09-07', '2026-09-07', 'daily', $1, $2, $3)
		`, dailyCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed daily summary: %v", err)
	}
	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("seed weekly summary via RunRollup: %v", err)
	}
	var originalID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'weekly' and period = '2026-W37' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&originalID)
	}); err != nil {
		t.Fatalf("load seeded weekly summary id: %v", err)
	}

	const actor = "test-operator-erin"
	correction := ConsolidationOutput{Summary: "Corrected weekly summary."}
	if err := runner.Correct(ctx, scope, originalID, correction, "the original missed a decision", actor, ""); err != nil {
		t.Fatalf("Correct: %v", err)
	}

	var loggedActor, eventType string
	err = db.QueryRowContext(ctx, `
		select actor, event_type from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'correct'
	`, scope.Kind, scope.Owner).Scan(&loggedActor, &eventType)
	if err != nil {
		t.Fatalf("expected a correct audit_log row: %v", err)
	}
	if loggedActor != actor {
		t.Errorf("actor = %q, want %q", loggedActor, actor)
	}
}

// TestRunDaily_SupersedesExistingDraftOnReconsolidation is a regression
// test for a real gap found via manual end-to-end testing: a second
// RunDaily call for a day that already had a current draft used to insert
// a second, completely unrelated summary row — not chained via
// supersedes — leaving two rows simultaneously "current" for the same
// day (see internal/store/retrieve.go's doc comment on what that means).
// RunDaily now looks up the existing current draft via currentSummaryID
// and supersedes it instead.
func TestRunDaily_SupersedesExistingDraftOnReconsolidation(t *testing.T) {
	groundingJSON := `{"grounded": [{"i":1,"ok":true}]}`
	firstJSON := `{
		"summary": "First draft: only the morning conversation.",
		"key_facts": [{"fact": "morning fact", "source_episode_ids": ["ep_recon_morning"]}],
		"entities_touched": []
	}`
	runner1, db1 := testRunner(t, firstJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-run-daily-reconsolidate"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db1, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner1.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	date := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	seedEpisode := func(id, input, output string) {
		t.Helper()
		inputCT, _ := enc.Encrypt(input)
		outputCT, _ := enc.Encrypt(output)
		if err := dbscope.Run(ctx, db1, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
				values ($1, $2, 'interaction', $3, $4, 'sha256:test', 0.5, $5, $6)
			`, id, date, inputCT, outputCT, scope.Kind, scope.Owner)
			return err
		}); err != nil {
			t.Fatalf("seed episode %s: %v", id, err)
		}
	}
	seedEpisode("ep_recon_morning", "morning question", "morning answer")

	if err := runner1.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("first RunDaily: %v", err)
	}

	var firstID string
	if err := dbscope.Run(ctx, db1, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-09-20' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&firstID)
	}); err != nil {
		t.Fatalf("load first draft id: %v", err)
	}

	// More of the day arrives, then consolidation re-runs — the realistic
	// trigger for this bug, not just an operator blindly re-running it.
	seedEpisode("ep_recon_afternoon", "afternoon question", "afternoon answer")

	secondJSON := `{
		"summary": "Regenerated draft: the full day, morning and afternoon.",
		"key_facts": [],
		"entities_touched": []
	}`
	runner2 := New(db1, runner1.keys, fakeConsolidationProvider{response: secondJSON}, fakeConsolidationProvider{response: groundingJSON}, fakeConsolidationProvider{response: secondJSON})
	if err := runner2.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("second RunDaily: %v", err)
	}

	var count int
	if err := dbscope.Run(ctx, db1, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where level = 'daily' and period = '2026-09-20' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	}); err != nil {
		t.Fatalf("count daily summaries: %v", err)
	}
	if count != 2 {
		t.Fatalf("got %d daily summaries after re-consolidation, want exactly 2 (first draft + the regenerated one)", count)
	}

	var secondSupersedes sql.NullString
	if err := dbscope.Run(ctx, db1, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select supersedes from summaries
			where level = 'daily' and period = '2026-09-20' and scope_kind = $1 and scope_owner = $2 and id != $3
		`, scope.Kind, scope.Owner, firstID).Scan(&secondSupersedes)
	}); err != nil {
		t.Fatalf("load second draft's supersedes: %v", err)
	}
	if !secondSupersedes.Valid || secondSupersedes.String != firstID {
		t.Errorf("second draft's supersedes = %v, want %q (the first draft) — a fork would leave this null", secondSupersedes, firstID)
	}

	current, err := runner1.currentSummaryID(ctx, db1, scope, "daily", "2026-09-20")
	if err != nil {
		t.Fatalf("currentSummaryID: %v", err)
	}
	if current == firstID {
		t.Error("currentSummaryID still resolves to the superseded first draft")
	}
}

// TestRunDaily_PassesEstablishedRecordOnReconsolidation is a regression
// test for a real failure mode found via live end-to-end testing, one
// level deeper than the supersede-not-fork fix above: once RunDaily
// started regenerating and superseding an existing draft, the regenerated
// draft had no way to know a fact in it had already been corrected.
// Regenerating purely from raw episode transcripts, the model saw the
// user's original raw statement (e.g. "500 concurrent jobs") and a later,
// already-corrected answer (e.g. "5,000", grounded in a real
// hupi-correct) as two conflicting claims with no signal that the
// correction was deliberate — and walked it back, treating its own
// correctly-corrected past answer as the less trustworthy one. This
// confirms RunDaily now threads the existing draft's text into the
// consolidation prompt as an established record on a re-consolidation,
// and omits it on a first run where there's nothing to establish yet.
func TestRunDaily_PassesEstablishedRecordOnReconsolidation(t *testing.T) {
	dsn := os.Getenv("HUPI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HUPI_TEST_DATABASE_URL not set; skipping integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	keys := crypto.NewKeyStore(db, make([]byte, 32))

	var captured []provider.ChatRequest
	firstJSON := `{"summary": "First draft.", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	consolidationProvider := fakeConsolidationProvider{response: firstJSON, capturedRequests: &captured}
	groundingProvider := fakeConsolidationProvider{response: groundingJSON}
	runner := New(db, keys, consolidationProvider, groundingProvider, consolidationProvider)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-established-record"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	date := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	seedEpisode := func(id, input, output string) {
		t.Helper()
		inputCT, _ := enc.Encrypt(input)
		outputCT, _ := enc.Encrypt(output)
		if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
				values ($1, $2, 'interaction', $3, $4, 'sha256:test', 0.5, $5, $6)
			`, id, date, inputCT, outputCT, scope.Kind, scope.Owner)
			return err
		}); err != nil {
			t.Fatalf("seed episode %s: %v", id, err)
		}
	}
	seedEpisode("ep_established_1", "first question", "first answer")

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("first RunDaily: %v", err)
	}
	// 2 calls, not 1: the whole-day summary call, plus one per-episode
	// insurance-pass call for the single episode — generateDailySummary
	// now runs that pass on every day, not just busy ones (see its own
	// doc comment, 89527b6b/852ce960). The summary call always goes
	// first (singlePassWithInsurance's own order), so captured[0] is
	// still the one to check for the established-record block.
	if len(captured) != 2 {
		t.Fatalf("got %d consolidation LLM calls after first RunDaily, want 2 (1 summary + 1 per-episode)", len(captured))
	}
	firstPrompt := captured[0].Messages[len(captured[0].Messages)-1].Content
	if strings.Contains(firstPrompt, "ALREADY-ESTABLISHED RECORD") {
		t.Error("first RunDaily's summary call included an established-record block — there was nothing to establish yet")
	}

	seedEpisode("ep_established_2", "second question", "second answer")
	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("second RunDaily: %v", err)
	}
	// +3 calls: the second day's summary call (now covering both
	// episodes) plus one per-episode call each for ep_established_1 and
	// ep_established_2 — total 5. The second summary call is captured[2]
	// (index 0-1 were the first RunDaily's summary + single per-episode
	// call).
	if len(captured) != 5 {
		t.Fatalf("got %d consolidation LLM calls after second RunDaily, want 5 (2 from the first run + 1 summary + 2 per-episode from the second)", len(captured))
	}
	secondPrompt := captured[2].Messages[len(captured[2].Messages)-1].Content
	if !strings.Contains(secondPrompt, "ALREADY-ESTABLISHED RECORD") {
		t.Error("second RunDaily call (a re-consolidation) did not include the established-record block")
	}
	if !strings.Contains(secondPrompt, "First draft.") {
		t.Error("established-record block did not contain the prior draft's actual text")
	}
}

// TestCorrect_ReplacesEntityAttributesWholesale is a regression test for a
// real gap found by inspecting a live correction's effect on the touched
// entity: upsertEntities always merged new attributes over existing ones,
// so a correction that re-describes a fact under a different attribute
// key than the original consolidation used (e.g. concurrency_limit vs.
// concurrent_jobs_per_node) left the stale key sitting right next to the
// corrected one, both visible to retrieval. A human correction is
// expected to state an entity's full corrected set of touched attributes,
// so Correct now replaces rather than merges.
func TestCorrect_ReplacesEntityAttributesWholesale(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	initialJSON := `{
		"summary": "Initial daily summary.",
		"key_facts": [],
		"entities_touched": [{"id": "project:widget", "kind": "project", "name": "Widget", "attributes": {"concurrency_limit": "500", "language": "Go"}}]
	}`
	runner, db := testRunner(t, initialJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-correct-replace-attrs"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-09-20",
		output: ConsolidationOutput{
			Summary: "Initial daily summary.",
			EntitiesTouched: []EntityUpdate{
				{ID: "project:widget", Kind: "project", Name: "Widget", Attributes: map[string]string{"concurrency_limit": "500", "language": "Go"}},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed initial summary+entity: %v", err)
	}

	var originalID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-09-20' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&originalID)
	}); err != nil {
		t.Fatalf("load seeded summary id: %v", err)
	}

	correction := ConsolidationOutput{
		Summary: "Corrected: concurrency raised.",
		EntitiesTouched: []EntityUpdate{
			// Deliberately a *different* key than the original
			// (concurrent_jobs_per_node, not concurrency_limit) — the
			// exact real-world scenario this test guards against — and
			// deliberately omits "language" too: replace is a wholesale
			// overwrite of a touched entity's attributes, so both the
			// stale key and the untouched-but-not-restated one should be
			// gone afterward, not merged forward.
			{ID: "project:widget", Kind: "project", Name: "Widget", Attributes: map[string]string{"concurrent_jobs_per_node": "2000"}},
		},
	}
	if err := runner.Correct(ctx, scope, originalID, correction, "raised the concurrency limit", "test-operator", ""); err != nil {
		t.Fatalf("Correct: %v", err)
	}

	var attrs map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrs, err = entityattrs.Current(ctx, tx, runner.keys, scope, "project:widget")
		return err
	}); err != nil {
		t.Fatalf("load corrected entity attributes: %v", err)
	}

	if got, want := attrs["concurrent_jobs_per_node"], "2000"; got != want {
		t.Errorf("concurrent_jobs_per_node = %q, want %q", got, want)
	}
	if v, exists := attrs["concurrency_limit"]; exists {
		t.Errorf("concurrency_limit = %q still present after correction — replace should have dropped the stale key, not merged over it", v)
	}
	if v, exists := attrs["language"]; exists {
		t.Errorf("language = %q survived — replace means wholesale overwrite of a touched entity's attributes, not a selective merge", v)
	}
}

// TestUpsertEntities_SupersedesKeysDeletesOldKeyBeforeMerging is Phase C
// sub-problem 1's real fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): the
// same concurrency_limit-vs-concurrent_jobs_per_node scenario as
// TestCorrect_ReplacesEntityAttributesWholesale above, but via the
// automatic, non-human daily consolidation path (upsertEntities' default
// merge, not Correct's wholesale replace) — an EntityUpdate naming an old
// key in SupersedesKeys should have that key deleted before the merge,
// not left sitting side by side with the new one forever, the real Wells
// Fargo failure mode this closes.
func TestUpsertEntities_SupersedesKeysDeletesOldKeyBeforeMerging(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, `{"summary": "unused", "key_facts": [], "entities_touched": []}`, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-supersedes-keys"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-09-20",
		output: ConsolidationOutput{
			Summary: "Initial daily summary.",
			EntitiesTouched: []EntityUpdate{
				{ID: "project:widget", Kind: "project", Name: "Widget", Attributes: map[string]string{"concurrency_limit": "500", "language": "Go"}},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed initial summary+entity: %v", err)
	}

	// A later day's own automatic consolidation, not a human correction —
	// storeSummary's default merge path (replaceEntityAttrs left false).
	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-09-21",
		output: ConsolidationOutput{
			Summary: "Later daily summary.",
			EntitiesTouched: []EntityUpdate{
				{
					ID:             "project:widget",
					Kind:           "project",
					Name:           "Widget",
					Attributes:     map[string]string{"concurrent_jobs_per_node": "2000"},
					SupersedesKeys: []string{"concurrency_limit"},
				},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("store later summary+entity update: %v", err)
	}

	var attrs map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrs, err = entityattrs.Current(ctx, tx, runner.keys, scope, "project:widget")
		return err
	}); err != nil {
		t.Fatalf("load entity attributes: %v", err)
	}

	if got, want := attrs["concurrent_jobs_per_node"], "2000"; got != want {
		t.Errorf("concurrent_jobs_per_node = %q, want %q", got, want)
	}
	if v, exists := attrs["concurrency_limit"]; exists {
		t.Errorf("concurrency_limit = %q still present — SupersedesKeys should have deleted it before merging, not left it side by side with the new key", v)
	}
	if got, want := attrs["language"], "Go"; got != want {
		t.Errorf("language = %q, want %q — the default merge path (unlike Correct's replace) should still preserve an untouched attribute", got, want)
	}
}

// TestCorrect_RejectsAlreadySupersededTarget is a regression test for a
// real bug found via manual end-to-end testing: Correct never checked
// whether -summary-id was still the current version before applying a
// correction to it. Pointing a second, independent correction at an
// already-superseded id silently forked history — two rows (the stale
// target's existing corrector, and the new one) both ended up "current"
// (see internal/store/retrieve.go's doc comment on what that means),
// with no principled way for retrieval to pick between them. Correct must
// refuse this rather than let it happen silently.
func TestCorrect_RejectsAlreadySupersededTarget(t *testing.T) {
	consolidationJSON := `{"summary": "unused", "key_facts": [], "entities_touched": []}`
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-correct-stale-target"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, _, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	v1CT, _ := enc.Encrypt("v1: the stale original")
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner)
			values ('sum_test_stale_v1', '2026-09-07', 'daily', $1, $2, $3)
		`, v1CT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}

	// First correction: v2 supersedes v1 — this one is legitimate and
	// must succeed, establishing v2 as current.
	v2 := ConsolidationOutput{Summary: "v2: the first correction"}
	if err := runner.Correct(ctx, scope, "sum_test_stale_v1", v2, "first correction", "operator-a", ""); err != nil {
		t.Fatalf("first Correct (v1 -> v2) should succeed: %v", err)
	}

	// Second correction targets v1 again — v1 is no longer current (v2
	// superseded it), so this must be rejected, not silently accepted.
	v3 := ConsolidationOutput{Summary: "v3: a second correction mistakenly targeting stale v1"}
	err = runner.Correct(ctx, scope, "sum_test_stale_v1", v3, "second correction, wrong target", "operator-b", "")
	if err == nil {
		t.Fatal("Correct against an already-superseded id should have failed, got nil error")
	}

	var count int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from summaries where period = '2026-09-07' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	}); err != nil {
		t.Fatalf("count summaries: %v", err)
	}
	if count != 2 {
		t.Errorf("got %d summaries for the period after the rejected correction, want exactly 2 (v1, v2) — a fork would show 3", count)
	}
}

// TestCurrentContent_DumpTemplateThenCorrectPreservesUntouchedAttributes
// covers the actual workflow -dump-template exists for: since Correct
// replaces a touched entity's attributes wholesale (see
// TestCorrect_ReplacesEntityAttributesWholesale), hand-authoring
// correction JSON from scratch means silently dropping every attribute
// you don't think to restate. This confirms CurrentContent's dump
// includes untouched attributes, and that editing just one field in the
// dump and feeding it back through Correct leaves the rest intact.
func TestCurrentContent_DumpTemplateThenCorrectPreservesUntouchedAttributes(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	initialJSON := `{
		"summary": "Original summary text.",
		"key_facts": [],
		"entities_touched": [{"id": "project:gizmo", "kind": "project", "name": "Gizmo", "attributes": {"a": "1", "b": "2"}}]
	}`
	runner, db := testRunner(t, initialJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-dump-template"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-09-20",
		output: ConsolidationOutput{
			Summary: "Original summary text.",
			EntitiesTouched: []EntityUpdate{
				{ID: "project:gizmo", Kind: "project", Name: "Gizmo", Attributes: map[string]string{"a": "1", "b": "2"}},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("seed initial summary+entity: %v", err)
	}

	var originalID string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id from summaries where level = 'daily' and period = '2026-09-20' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&originalID)
	}); err != nil {
		t.Fatalf("load seeded summary id: %v", err)
	}

	dump, err := runner.CurrentContent(ctx, scope, originalID)
	if err != nil {
		t.Fatalf("CurrentContent: %v", err)
	}
	if dump.Summary != "Original summary text." {
		t.Errorf("dumped summary = %q, want the original prose", dump.Summary)
	}
	if len(dump.EntitiesTouched) != 1 || dump.EntitiesTouched[0].Attributes["a"] != "1" || dump.EntitiesTouched[0].Attributes["b"] != "2" {
		t.Fatalf("dumped entities = %+v, want project:gizmo with a=1, b=2", dump.EntitiesTouched)
	}

	// The only edit a human correcting this would actually make: change
	// the one wrong field. "b" is carried forward untouched because the
	// dump already had it, not because anyone had to remember to restate it.
	dump.EntitiesTouched[0].Attributes["a"] = "99"
	dump.Summary = "Corrected summary text."

	if err := runner.Correct(ctx, scope, originalID, dump, "fixed field a using the dumped template", "test-operator", ""); err != nil {
		t.Fatalf("Correct with edited dump: %v", err)
	}

	var attrs map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrs, err = entityattrs.Current(ctx, tx, runner.keys, scope, "project:gizmo")
		return err
	}); err != nil {
		t.Fatalf("load corrected entity attributes: %v", err)
	}
	if attrs["a"] != "99" {
		t.Errorf("a = %q, want %q (the corrected value)", attrs["a"], "99")
	}
	if attrs["b"] != "2" {
		t.Errorf("b = %q, want %q (untouched, preserved via the dumped template)", attrs["b"], "2")
	}
}

// TestStoreSummary_CanonicalizesEntityIDByKindAndName is a regression test
// for a real gap found via live end-to-end testing: two separate
// consolidation runs generated two different id strings ("project:falcon"
// and "project:project-falcon") for the same real-world entity, since the
// consolidation LLM picks an id slug freely each time instead of reusing
// a stable one. That fragmented one entity into two rows, both of which
// surfaced in retrieval side by side. This seeds two summaries (as if
// from two separate consolidation runs) whose EntitiesTouched name the
// same kind+name but supply two different id strings, and confirms only
// one entities row results — under the id storeSummary actually derives
// from kind+name, not either of the two ids the caller supplied — with
// both runs' attributes merged onto it.
func TestStoreSummary_CanonicalizesEntityIDByKindAndName(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, `{"summary": "unused", "key_facts": [], "entities_touched": []}`, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entity-canonicalization"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	store := func(period, entityID string, attrs map[string]string) {
		t.Helper()
		if err := runner.storeSummary(ctx, storeSummaryInput{
			scope:  scope,
			level:  "daily",
			period: period,
			output: ConsolidationOutput{
				Summary: "period " + period,
				EntitiesTouched: []EntityUpdate{
					{ID: entityID, Kind: "project", Name: "Project Falcon", Attributes: attrs},
				},
			},
			actor: systemActor,
		}); err != nil {
			t.Fatalf("storeSummary for %s: %v", period, err)
		}
	}

	store("2026-09-20", "project:falcon", map[string]string{"launch": "Q3"})
	store("2026-09-21", "project:project-falcon", map[string]string{"launch": "Q2"})

	var count int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select count(*) from entities where kind = 'project' and name = 'Project Falcon' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&count)
	}); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if count != 1 {
		t.Fatalf("got %d entities rows for kind=project name=%q, want exactly 1 — two different ids should have canonicalized to the same row", count, "Project Falcon")
	}

	const canonicalID = "project:project-falcon"
	var attrs map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrs, err = entityattrs.Current(ctx, tx, runner.keys, scope, canonicalID)
		return err
	}); err != nil {
		t.Fatalf("load attributes at canonical id %s: %v", canonicalID, err)
	}
	if attrs["launch"] != "Q2" {
		t.Errorf("launch = %q, want %q (the second run's value, merged onto the same canonicalized row)", attrs["launch"], "Q2")
	}
}

// TestStoreSummary_EmbedsTouchedEntities confirms storeSummary actually
// calls embedEntities and it writes a real embedding — the DB-level
// wiring internal/store's TestRetrieve_FindsEntityByVectorSearchWhenSubstringMatchMisses
// doesn't cover, since that test seeds an entity directly and never goes
// through storeSummary/embedEntities at all. See
// schema/0012_entity_embeddings.sql for why entities need embeddings in
// the first place.
func TestStoreSummary_EmbedsTouchedEntities(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	runner, db := testRunner(t, `{"summary": "unused", "key_facts": [], "entities_touched": []}`, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-embed-entities"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	if err := runner.storeSummary(ctx, storeSummaryInput{
		scope:  scope,
		level:  "daily",
		period: "2026-09-20",
		output: ConsolidationOutput{
			Summary: "Daily summary mentioning the entity.",
			EntitiesTouched: []EntityUpdate{
				{ID: "project:orbit", Kind: "project", Name: "Orbit", Attributes: map[string]string{"status": "active"}},
			},
		},
		actor: systemActor,
	}); err != nil {
		t.Fatalf("storeSummary: %v", err)
	}

	var hasEmbedding bool
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select embedding is not null from entities where id = 'project:orbit' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&hasEmbedding)
	}); err != nil {
		t.Fatalf("check entity embedding: %v", err)
	}
	if !hasEmbedding {
		t.Error("entity project:orbit has no embedding after storeSummary — embedEntities should have written one")
	}
}

// TestRunDaily_BackfillsMissingEntityEmbeddings confirms entitiesMissingEmbeddings
// + embedEntities actually catch up entities that predate
// schema/0012_entity_embeddings.sql (or whose embedding failed to write
// previously) — seeds an entity directly with no embedding, then checks
// a RunDaily call for the same scope backfills it even though that
// entity isn't among the day's own EntitiesTouched.
func TestRunDaily_BackfillsMissingEntityEmbeddings(t *testing.T) {
	groundingJSON := `{"grounded": []}`
	consolidationJSON := `{"summary": "Unrelated daily content.", "key_facts": [], "entities_touched": []}`
	runner, db := testRunner(t, consolidationJSON, groundingJSON)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-embeddings"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, keyVersion, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{"pre_existing":"true"}`)
	if err != nil {
		t.Fatalf("encrypt pre-existing entity attrs: %v", err)
	}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner, key_version)
			values ('project:legacy', 'project', 'Legacy Project', $1, $2, $3, $4)
		`, attrsCT, scope.Kind, scope.Owner, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("seed pre-existing entity with no embedding: %v", err)
	}

	date := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	inputCT, _ := enc.Encrypt("unrelated question")
	outputCT, _ := enc.Encrypt("unrelated answer")
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner)
			values ('ep_test_backfill', $1, 'interaction', $2, $3, 'sha256:test', 0.5, $4, $5)
		`, date, inputCT, outputCT, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed episode: %v", err)
	}

	if err := runner.RunDaily(ctx, scope, date); err != nil {
		t.Fatalf("RunDaily: %v", err)
	}

	var hasEmbedding bool
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select embedding is not null from entities where id = 'project:legacy' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&hasEmbedding)
	}); err != nil {
		t.Fatalf("check backfilled entity embedding: %v", err)
	}
	if !hasEmbedding {
		t.Error("pre-existing entity project:legacy still has no embedding after RunDaily — the backfill should have caught it")
	}
}

// TestRunRollup_ThreadsKnownEntitiesIntoPrompt is the real regression
// test for docs/CONSOLIDATION_ARCHITECTURE_REVIEW_PLAN.md finding 3:
// RunRollup used to call generateSummary with knownEntities always nil
// ("Phase C sub-problem 1 is scoped to raw daily episode text for now"),
// so a rollup regenerating its own entities_touched/attributes straight
// from its source summaries' prose had no visibility into what a
// touched entity's existing attributes already are — the same
// key-fragmentation risk (preapproval_amount vs. pre_approved_amount)
// RunDaily's own findKnownEntities call already prevents at the daily
// level. Asserts on prompt *content* via capturedRequests (the same
// established pattern used elsewhere in this file to check
// established-record threading) rather than relying on a real LLM's
// judgment, which this session repeatedly found to be non-deterministic
// for exactly this kind of attribute-naming decision.
func TestRunRollup_ThreadsKnownEntitiesIntoPrompt(t *testing.T) {
	consolidationJSON := `{
		"summary": "Rolled up the week.",
		"key_facts": [],
		"entities_touched": []
	}`
	groundingJSON := `{"grounded": []}`
	var captured []provider.ChatRequest
	runner, db := testRunner(t, consolidationJSON, groundingJSON)
	runner.consolidation = fakeConsolidationProvider{response: consolidationJSON, capturedRequests: &captured}

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-rollup-known-entities"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, keyVersion, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}

	// A pre-existing entity whose name appears in the rollup's own
	// source summary text below — findKnownEntities' substring match is
	// what's supposed to pick this up now.
	attrCT, err := enc.Encrypt("$400,000")
	if err != nil {
		t.Fatalf("encrypt entity attr: %v", err)
	}
	dailyText := "Jolene mentioned her mortgage pre-approval again this week."
	dailyCT, err := enc.Encrypt(dailyText)
	if err != nil {
		t.Fatalf("encrypt daily summary text: %v", err)
	}

	sourcePeriods := []string{"2026-09-07", "2026-09-08"}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`
			insert into entities (id, kind, name, scope_kind, scope_owner, key_version)
			values ('person:jolene', 'person', 'Jolene', $1, $2, $3)
		`, scope.Kind, scope.Owner, keyVersion); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
			values ('mem_test_jolene_preapproval', $1, $2, 'person:jolene', 'preapproval_amount', $3, $4, true, true)
		`, scope.Kind, scope.Owner, attrCT, keyVersion); err != nil {
			return err
		}
		for _, p := range sourcePeriods {
			if _, err := tx.ExecContext(ctx, `
				insert into summaries (id, period, level, summary, scope_kind, scope_owner, key_version)
				values ($1, $2, 'daily', $3, $4, $5, $6)
			`, "sum_test_"+p, p, dailyCT, scope.Kind, scope.Owner, keyVersion); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed entity and daily summaries: %v", err)
	}

	if err := runner.RunRollup(ctx, scope, "weekly", "daily", "2026-W37", sourcePeriods); err != nil {
		t.Fatalf("RunRollup: %v", err)
	}

	if len(captured) != 1 {
		t.Fatalf("got %d captured consolidation requests, want exactly 1", len(captured))
	}
	prompt := captured[0].Messages[len(captured[0].Messages)-1].Content
	if !strings.Contains(prompt, "KNOWN ENTITIES") {
		t.Errorf("rollup prompt = %q, want it to include a KNOWN ENTITIES section now that findKnownEntities runs for rollups too", prompt)
	}
	if !strings.Contains(prompt, "Jolene") || !strings.Contains(prompt, "preapproval_amount") {
		t.Errorf("rollup prompt = %q, want it to include Jolene's existing preapproval_amount attribute", prompt)
	}
}
