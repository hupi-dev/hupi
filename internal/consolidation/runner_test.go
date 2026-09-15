package consolidation

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// See internal/store/scope_isolation_test.go's doc comment for how to run
// against a real instance.

var errFakeNotImplemented = errors.New("fake provider: not implemented, not needed by these tests")

// fakeConsolidationProvider returns a fixed, valid ConsolidationOutput
// JSON payload — enough to exercise storeSummary's real transaction path
// (the thing this file's test cares about) without a real LLM.
type fakeConsolidationProvider struct{ response string }

func (fakeConsolidationProvider) Name() string   { return "fake" }
func (fakeConsolidationProvider) Vendor() string { return "fake" }
func (fakeConsolidationProvider) Model() string  { return "fake-model" }

func (f fakeConsolidationProvider) ChatCompletion(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{Message: provider.Message{Role: provider.RoleAssistant, Content: f.response}}, nil
}

func (fakeConsolidationProvider) StreamChatCompletion(context.Context, provider.ChatRequest) (<-chan provider.StreamChunk, error) {
	return nil, errFakeNotImplemented
}

func (fakeConsolidationProvider) Embed(_ context.Context, req provider.EmbedRequest) (provider.EmbedResponse, error) {
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
	groundingJSON := `{"grounded": [true]}`
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

	// summary_key_facts has no scope columns of its own and isn't
	// RLS-protected (see schema/0005's doc comment) — a plain query is
	// correct here, not an oversight.
	var grounded bool
	if err := db.QueryRowContext(ctx, `select grounded from summary_key_facts where summary_id = $1`, summaryID).Scan(&grounded); err != nil {
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
	if err := runner.Correct(ctx, scope, originalID, correction, "the original missed a decision", actor); err != nil {
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
