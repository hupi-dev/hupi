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
