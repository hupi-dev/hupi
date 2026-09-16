package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestRunDaily_SupersedesExistingDraftOnReconsolidation is a regression
// test for a real gap found via manual end-to-end testing: a second
// RunDaily call for a day that already had a current draft used to insert
// a second, completely unrelated summary row — not chained via
// supersedes — leaving two rows simultaneously "current" for the same
// day (see internal/store/retrieve.go's doc comment on what that means).
// RunDaily now looks up the existing current draft via currentSummaryID
// and supersedes it instead.
func TestRunDaily_SupersedesExistingDraftOnReconsolidation(t *testing.T) {
	groundingJSON := `{"grounded": [true]}`
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
	if err := runner.Correct(ctx, scope, originalID, correction, "raised the concurrency limit", "test-operator"); err != nil {
		t.Fatalf("Correct: %v", err)
	}

	var attrsCT []byte
	var keyVersion int
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select attributes, key_version from entities where id = 'project:widget' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&attrsCT, &keyVersion)
	}); err != nil {
		t.Fatalf("load corrected entity: %v", err)
	}
	enc, err := runner.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		t.Fatalf("resolve key version: %v", err)
	}
	attrsJSON, err := enc.Decrypt(attrsCT)
	if err != nil {
		t.Fatalf("decrypt entity attributes: %v", err)
	}
	var attrs map[string]string
	if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
		t.Fatalf("parse entity attributes: %v", err)
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
	if err := runner.Correct(ctx, scope, "sum_test_stale_v1", v2, "first correction", "operator-a"); err != nil {
		t.Fatalf("first Correct (v1 -> v2) should succeed: %v", err)
	}

	// Second correction targets v1 again — v1 is no longer current (v2
	// superseded it), so this must be rejected, not silently accepted.
	v3 := ConsolidationOutput{Summary: "v3: a second correction mistakenly targeting stale v1"}
	err = runner.Correct(ctx, scope, "sum_test_stale_v1", v3, "second correction, wrong target", "operator-b")
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
