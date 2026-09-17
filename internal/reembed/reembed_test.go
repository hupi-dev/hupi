package reembed

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/consolidation"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// these against a real Postgres instance.

func testDB(t *testing.T) (*sql.DB, *crypto.KeyStore) {
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
	return db, crypto.NewKeyStore(db, make([]byte, 32)) // all-zero test KEK, never used for real data
}

func cleanup(t *testing.T, db *sql.DB, scope identity.Scope) {
	t.Helper()
	_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
		tx.Exec(`delete from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
	db.Exec(`delete from audit_log where workspace_scope_kind = $1 and workspace_scope_owner = $2`, scope.Kind, scope.Owner)
}

var errFakeNotImplemented = errors.New("fakeEmbedder: not implemented, not needed by these tests")

// fakeEmbedder records every text it's asked to embed — so a test can
// assert reembed actually decrypted and passed the right plaintext, not
// just that it wrote *some* vector — and returns a vector derived from a
// running call counter, so successive embeds are distinguishable from
// each other too.
type fakeEmbedder struct {
	vendor, model string
	calls         *[]string
}

func newFakeEmbedder(vendor, model string) fakeEmbedder {
	return fakeEmbedder{vendor: vendor, model: model, calls: &[]string{}}
}

func (f fakeEmbedder) Name() string   { return f.vendor + "-" + f.model }
func (f fakeEmbedder) Vendor() string { return f.vendor }
func (f fakeEmbedder) Model() string  { return f.model }

func (fakeEmbedder) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errFakeNotImplemented
}

func (fakeEmbedder) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (f fakeEmbedder) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	vecs := make([][]float32, len(req.Input))
	for i, text := range req.Input {
		*f.calls = append(*f.calls, text)
		v := make([]float32, 1536)
		v[0] = float32(len(*f.calls))
		vecs[i] = v
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

// dummyVector is a syntactically valid vector(1536) literal used to seed
// rows that already have "some" embedding — its exact values don't
// matter, only that it's non-null and gets overwritten (or doesn't) as
// each test expects.
func dummyVector() string {
	v := make([]float32, 1536)
	v[0] = -1
	return pgfmt.VectorLiteral(v)
}

func seedSummary(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id, text, embeddingModel string, withEmbedding bool) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	summaryCT, err := enc.Encrypt(text)
	if err != nil {
		t.Fatalf("encrypt summary: %v", err)
	}
	var embeddingModelArg any
	if embeddingModel != "" {
		embeddingModelArg = embeddingModel
	}
	var embeddingArg any
	if withEmbedding {
		embeddingArg = dummyVector()
	}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, embedding, embedding_model)
			values ($1, $2, 'daily', $3, $4, $5, $6, $7)
		`, id, "2026-09-0"+id[len(id)-1:], summaryCT, scope.Kind, scope.Owner, embeddingArg, embeddingModelArg)
		return err
	})
	if err != nil {
		t.Fatalf("seed summary %s: %v", id, err)
	}
}

func seedEpisode(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id, epType string, importance float64, input, output, embeddingModel string, withEmbedding bool) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	inputCT, err := enc.Encrypt(input)
	if err != nil {
		t.Fatalf("encrypt input: %v", err)
	}
	outputCT, err := enc.Encrypt(output)
	if err != nil {
		t.Fatalf("encrypt output: %v", err)
	}
	var embeddingModelArg any
	if embeddingModel != "" {
		embeddingModelArg = embeddingModel
	}
	var embeddingArg any
	if withEmbedding {
		embeddingArg = dummyVector()
	}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (id, ts, type, input_text, output_text, hash, importance, scope_kind, scope_owner, embedding, embedding_model)
			values ($1, now(), $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`, id, epType, inputCT, outputCT, "sha256:"+id, importance, scope.Kind, scope.Owner, embeddingArg, embeddingModelArg)
		return err
	})
	if err != nil {
		t.Fatalf("seed episode %s: %v", id, err)
	}
}

func seedEntity(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id, kind, name string, attrsJSON, embeddingModel string, withEmbedding bool) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(attrsJSON)
	if err != nil {
		t.Fatalf("encrypt attributes: %v", err)
	}
	var embeddingModelArg any
	if embeddingModel != "" {
		embeddingModelArg = embeddingModel
	}
	var embeddingArg any
	if withEmbedding {
		embeddingArg = dummyVector()
	}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner, embedding, embedding_model)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
		`, id, kind, name, attrsCT, scope.Kind, scope.Owner, embeddingArg, embeddingModelArg)
		return err
	})
	if err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

func runToCompletion(t *testing.T, ctx context.Context, r *Runner, scope identity.Scope, batchSize int) int {
	t.Helper()
	total := 0
	for i := 0; i < 1000; i++ { // hard cap so a bug can't hang the test suite
		processed, _, done, err := r.Continue(ctx, scope, batchSize)
		if err != nil {
			t.Fatalf("Continue: %v", err)
		}
		total += processed
		if done {
			return total
		}
	}
	t.Fatal("reembed did not complete within 1000 Continue calls — likely stuck")
	return total
}

func TestReembed_StatusCountsNullAndMismatchedRows(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-reembed-status"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	current := newFakeEmbedder("openai", "text-embed-3")

	// Summaries: one never embedded, one stale, one already current.
	seedSummary(t, ctx, db, keys, scope, "sum_status-null", "summary null", "", false)
	seedSummary(t, ctx, db, keys, scope, "sum_status-stale", "summary stale", "openai:text-embed-2", true)
	seedSummary(t, ctx, db, keys, scope, "sum_status-current", "summary current", "openai:text-embed-3", true)

	// Episodes: above-threshold + stale (pending), above-threshold +
	// current (not pending), below-threshold + null (never meant to be
	// embedded — out of scope, not "pending"), and a non-interaction type
	// that's above threshold and stale (also out of scope — only
	// interaction episodes get individually embedded).
	seedEpisode(t, ctx, db, keys, scope, "ep_status-pending", "interaction", 0.9, "in1", "out1", "openai:text-embed-2", true)
	seedEpisode(t, ctx, db, keys, scope, "ep_status-current", "interaction", 0.9, "in2", "out2", "openai:text-embed-3", true)
	seedEpisode(t, ctx, db, keys, scope, "ep_status-low-importance", "interaction", 0.2, "in3", "out3", "", false)
	seedEpisode(t, ctx, db, keys, scope, "ep_status-feedback", "feedback", 0.9, "in4", "out4", "openai:text-embed-2", true)

	// Entities: one stale, one current.
	seedEntity(t, ctx, db, keys, scope, "project:status-stale", "project", "Stale Project", `{"k":"v"}`, "openai:text-embed-2", true)
	seedEntity(t, ctx, db, keys, scope, "project:status-current", "project", "Current Project", `{"k":"v"}`, "openai:text-embed-3", true)

	r := New(db, keys, current)
	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Model != "openai:text-embed-3" {
		t.Errorf("Model = %q, want %q", st.Model, "openai:text-embed-3")
	}
	if st.SummariesPending != 2 {
		t.Errorf("SummariesPending = %d, want 2 (null + stale)", st.SummariesPending)
	}
	if st.EpisodesPending != 1 {
		t.Errorf("EpisodesPending = %d, want 1 (only the above-threshold interaction episode with a stale model)", st.EpisodesPending)
	}
	if st.EntitiesPending != 1 {
		t.Errorf("EntitiesPending = %d, want 1 (only the stale one)", st.EntitiesPending)
	}
}

func TestReembed_ContinueReembedsUntilDoneAndSkipsCurrentRows(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-reembed-continue"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	current := newFakeEmbedder("openai", "text-embed-3")

	seedSummary(t, ctx, db, keys, scope, "sum_continue-stale", "summary stale text", "openai:text-embed-2", true)
	seedSummary(t, ctx, db, keys, scope, "sum_continue-current", "summary current text", "openai:text-embed-3", true)

	seedEpisode(t, ctx, db, keys, scope, "ep_continue-stale", "interaction", 0.9, "stale in", "stale out", "openai:text-embed-2", true)
	seedEpisode(t, ctx, db, keys, scope, "ep_continue-current", "interaction", 0.9, "current in", "current out", "openai:text-embed-3", true)
	seedEpisode(t, ctx, db, keys, scope, "ep_continue-below-threshold", "interaction", 0.1, "low in", "low out", "", false)

	seedEntity(t, ctx, db, keys, scope, "project:continue-stale", "project", "Stale", `{"a":"b"}`, "openai:text-embed-2", true)
	seedEntity(t, ctx, db, keys, scope, "project:continue-current", "project", "Current", `{"a":"b"}`, "openai:text-embed-3", true)

	r := New(db, keys, current)
	total := runToCompletion(t, ctx, r, scope, 1) // batch size 1 forces many small batches
	if total != 3 {                               // 1 stale summary + 1 stale episode + 1 stale entity
		t.Errorf("re-embedded %d rows, want 3 (only the stale ones)", total)
	}

	// The already-current rows must never have been sent to the embedder.
	for _, text := range *current.calls {
		if text == "summary current text" {
			t.Error("embedder was called with the already-current summary's text — it should have been skipped entirely")
		}
	}

	// Content actually embedded must be the *decrypted* plaintext, proving
	// reembed round-trips through the same encryption key the row was
	// written under, not just some placeholder.
	foundStaleSummary := false
	foundStaleEpisode := false
	wantEpisodeText := consolidation.EpisodeEmbedText("stale in", "stale out")
	for _, text := range *current.calls {
		if text == "summary stale text" {
			foundStaleSummary = true
		}
		if text == wantEpisodeText {
			foundStaleEpisode = true
		}
	}
	if !foundStaleSummary {
		t.Error("expected the embedder to have been called with the stale summary's decrypted text")
	}
	if !foundStaleEpisode {
		t.Errorf("expected the embedder to have been called with %q (the stale episode's canonical embed text)", wantEpisodeText)
	}

	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status after completion: %v", err)
	}
	if st.Total() != 0 {
		t.Errorf("Status after completion = %+v, want everything at 0", st)
	}

	assertModel := func(table, id, want string) {
		var got sql.NullString
		if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, "select embedding_model from "+table+" where id = $1", id).Scan(&got)
		}); err != nil {
			t.Fatalf("load %s.%s embedding_model: %v", table, id, err)
		}
		if !got.Valid || got.String != want {
			t.Errorf("%s.%s embedding_model = %v, want %q", table, id, got, want)
		}
	}
	assertModel("summaries", "sum_continue-stale", "openai:text-embed-3")
	assertModel("episodes", "ep_continue-stale", "openai:text-embed-3")
	assertModel("entities", "project:continue-stale", "openai:text-embed-3")

	// The below-threshold episode was never supposed to be embedded and
	// must remain untouched — reembed extending its reach there would be
	// a real behavior change, not just a fix.
	var belowThresholdModel sql.NullString
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "select embedding_model from episodes where id = $1", "ep_continue-below-threshold").Scan(&belowThresholdModel)
	}); err != nil {
		t.Fatalf("load below-threshold episode: %v", err)
	}
	if belowThresholdModel.Valid {
		t.Errorf("below-threshold episode got embedding_model = %q, want it to remain untouched (null)", belowThresholdModel.String)
	}
}

func TestReembed_LogRunWritesAuditEntry(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-reembed-audit"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	current := newFakeEmbedder("openai", "text-embed-3")
	r := New(db, keys, current)

	if err := r.LogRun(ctx, scope, "test-actor", 7); err != nil {
		t.Fatalf("LogRun: %v", err)
	}

	var actor, eventType string
	var detail []byte
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select actor, event_type, detail from audit_log
			where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'reembed'
			order by id desc limit 1
		`, scope.Kind, scope.Owner).Scan(&actor, &eventType, &detail)
	})
	if err != nil {
		t.Fatalf("load audit_log row: %v", err)
	}
	if actor != "test-actor" {
		t.Errorf("actor = %q, want %q", actor, "test-actor")
	}
	if eventType != "reembed" {
		t.Errorf("event_type = %q, want %q", eventType, "reembed")
	}
	var parsed map[string]any
	if err := json.Unmarshal(detail, &parsed); err != nil {
		t.Fatalf("parse detail json %s: %v", detail, err)
	}
	if got, ok := parsed["rows_reembedded"].(float64); !ok || got != 7 {
		t.Errorf("detail[rows_reembedded] = %v, want 7", parsed["rows_reembedded"])
	}
}
