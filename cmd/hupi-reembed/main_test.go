package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
	"hupi/internal/reembed"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// this against a real Postgres instance.

type fakeEmbedder struct{}

func (fakeEmbedder) Name() string   { return "test" }
func (fakeEmbedder) Vendor() string { return "test" }
func (fakeEmbedder) Model() string  { return "test-model" }
func (fakeEmbedder) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	panic("not used")
}
func (fakeEmbedder) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	panic("not used")
}
func (fakeEmbedder) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
	vecs := make([][]float32, len(req.Input))
	for i := range req.Input {
		v := make([]float32, 1536)
		v[0] = 1
		vecs[i] = v
	}
	return provider.EmbedResponse{Vectors: vecs}, nil
}

// TestReembedAll_AuditsEveryBatchSeparately is a real regression test
// (docs/CODEBASE_SURVEY_AND_REVIEW.md finding A9): LogRun used to be
// called once, after the whole run completed, with the accumulated
// total — so a process killed between a batch committing and that final
// call left real, committed embedding writes with zero audit trail,
// permanently (the next invocation finds nothing pending and never logs
// it either). reembedAll now calls LogRun once per non-empty batch. With
// batch size 1 and 3 rows needing re-embedding, this must produce 3
// separate reembed audit_log entries, not 1 combined one — proving
// there's no window, even within a single successful run, where
// completed work waits until the very end to be audited.
func TestReembedAll_AuditsEveryBatchSeparately(t *testing.T) {
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

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-reembed-all-audit-per-batch"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
		// audit_log is deliberately append-only — the hupi_app role has no
		// DELETE grant on it at all (schema/0007_audit_log.sql), confirmed
		// by this test's own earlier attempt to clean it up silently
		// failing with a permission error. Querying by a high-water mark
		// captured before this test's own run (below), rather than
		// depending on the table starting empty, is what actually makes
		// this test correct across repeated runs — not cleanup.
	})

	var beforeID int64
	if err := db.QueryRowContext(ctx, `select coalesce(max(id), 0) from audit_log`).Scan(&beforeID); err != nil {
		t.Fatalf("capture audit_log high-water mark: %v", err)
	}

	enc, keyVersion, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	for i, id := range []string{"sum_batch_1", "sum_batch_2", "sum_batch_3"} {
		summaryCT, err := enc.Encrypt("summary text")
		if err != nil {
			t.Fatalf("encrypt test summary: %v", err)
		}
		vec := make([]float32, 1536)
		vec[0] = 1
		if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.Exec(`
				insert into summaries (id, period, level, summary, scope_kind, scope_owner, key_version, embedding, embedding_model)
				values ($1, $2, 'daily', $3, $4, $5, $6, $7::vector, 'old-vendor:old-model')
			`, id, "2026-01-0"+string(rune('1'+i)), summaryCT, scope.Kind, scope.Owner, keyVersion, pgfmt.VectorLiteral(vec))
			return err
		}); err != nil {
			t.Fatalf("seed stale summary %s: %v", id, err)
		}
	}

	runner := reembed.New(db, keys, fakeEmbedder{})
	total, err := reembedAll(ctx, runner, scope, "test-actor", 1, nil) // batch size 1 forces 3 separate batches
	if err != nil {
		t.Fatalf("reembedAll: %v", err)
	}
	if total != 3 {
		t.Fatalf("reembedAll processed %d rows, want 3", total)
	}

	rows, err := db.QueryContext(ctx, `
		select detail from audit_log
		where workspace_scope_kind = $1 and workspace_scope_owner = $2 and event_type = 'reembed' and id > $3
		order by id
	`, scope.Kind, scope.Owner, beforeID)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	defer rows.Close()

	var entries []map[string]any
	for rows.Next() {
		var detailJSON []byte
		if err := rows.Scan(&detailJSON); err != nil {
			t.Fatalf("scan audit_log row: %v", err)
		}
		var detail map[string]any
		if err := json.Unmarshal(detailJSON, &detail); err != nil {
			t.Fatalf("unmarshal audit_log detail: %v", err)
		}
		entries = append(entries, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit_log rows: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d reembed audit_log entries, want 3 (one per batch) — auditing must not be deferred to a single end-of-run entry", len(entries))
	}
	for _, e := range entries {
		got, ok := e["rows_reembedded"].(float64)
		if !ok || got != 1 {
			t.Errorf("audit entry detail[rows_reembedded] = %v, want 1 (this batch's own count, not the running total)", e["rows_reembedded"])
		}
	}
}
