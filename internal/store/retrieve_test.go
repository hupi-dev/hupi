package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
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

// insertSummaryWithEmbedding is insertSummary with an explicit embedding
// vector instead of the fixed axis(0) vector insertSummary always uses —
// needed whenever a test must make a summary's own overall embedding
// deliberately *dissimilar* to the query (insertSummary's fixed vector
// always matches the fake test embedder's fixed query output, which is
// fine when that's not what's under test, but wrong here).
func insertSummaryWithEmbedding(t *testing.T, s *Store, scope identity.Scope, id, period, text string, vec []float32) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	summaryCT, err := enc.Encrypt(text)
	if err != nil {
		t.Fatalf("encrypt test summary: %v", err)
	}
	embeddingLiteral := pgfmt.VectorLiteral(vec)

	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summaries (
				id, period, level, status, summary, grounding_checked,
				scope_kind, scope_owner, key_version, embedding
			) values (
				$1, $2, 'daily', 'draft', $3, true,
				$4, $5, $6, $7::vector
			)
		`, id, period, summaryCT, scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test summary %s: %v", id, err)
	}
}

// TestFusedSearchSummaries_FindsSummaryByBuriedFactMatch_852ce960 is the
// regression test for a real LongMemEval miss
// (docs/LONGMEMEVAL_ACCURACY_PLAN.md's 852ce960 case): a summary whose
// own overall content is dominated by unrelated topics can bury one
// single, correctly-grounded, highly specific fact so deeply that the
// summary's own embedding/keyword profile never matches a query about
// that fact — and before this fix, fusedSearchSummaries only ever scored
// whole summaries, so the summary never entered the candidate pool at
// all (confirmed via live HUPI_DEBUG_FUSION tracing against the real
// scope, not just reasoned about). Per-fact ranking (rankKeyFacts) only
// ever runs on summaries that already made the pool, so a real, grounded
// fact could be permanently unreachable through no fault of its own
// extraction or grounding.
//
// The fake test embedder (scope_isolation_test.go) returns the same
// fixed vector (axis(0)) for every query regardless of content — this
// fixture gives the summary's own embedding a *different* axis (clearly
// dissimilar, cosine 0) while the one buried fact gets axis(0) (an exact
// match to whatever the query embeds to), and the query text shares no
// vocabulary with the summary's own prose (so BM25 can't find it as a
// side effect, which would defeat the point of this test). Only the new
// per-fact search signal can surface this summary.
func TestFusedSearchSummaries_FindsSummaryByBuriedFactMatch_852ce960(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-fact-level-candidate"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	axis := func(i int) []float32 {
		v := make([]float32, 1536)
		v[i] = 1
		return v
	}

	period := "2023-11-30"
	summaryID := "sum_test-fact-level-candidate_2023-11-30_daily_v1"
	insertSummaryWithEmbedding(t, s, scope, summaryID, period,
		"The user discussed moving logistics, cable providers, and home insurance quotes.",
		axis(7))
	model := s.currentEmbeddingModel()
	insertKeyFactWithEmbedding(t, s, scope, summaryID,
		"The user was pre-approved for $400,000 from Wells Fargo.", model, axis(0))

	messages := []provider.Message{{Role: provider.RoleUser, Content: "What was the mortgage pre-approval amount?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := "The user was pre-approved for $400,000 from Wells Fargo."
	if !strings.Contains(result.ContextMessage, want) {
		t.Errorf("context message = %q, want it to contain the buried fact %q — the summary should have entered the candidate pool via the per-fact match alone", result.ContextMessage, want)
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
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
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

// insertKeyFact seeds a summary_key_facts row directly, the same
// technique insertSummary uses for its own parent row.
func insertKeyFact(t *testing.T, s *Store, scope identity.Scope, summaryID, fact string, grounded bool) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	factCT, err := enc.Encrypt(fact)
	if err != nil {
		t.Fatalf("encrypt test key fact: %v", err)
	}
	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summary_key_facts (summary_id, fact, grounded, key_version, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5, $6)
		`, summaryID, factCT, grounded, keyVersion, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("insert test key fact for %s: %v", summaryID, err)
	}
}

// TestRetrieve_SurfacesGroundedKeyFactsAlongsideSummary is a regression
// test for a real gap found tracing cmd/hupi-bench QA misses against a
// real cloud model at scale: a daily/weekly summary's own prose is
// inherently lossy (a narrative paragraph, not every source detail), and
// consolidation already extracts and grounds specific, checkable facts
// per summary into summary_key_facts (schema/0001_init.sql) -- but
// nothing at retrieval time ever read that table back before this fix.
// A question whose answer was a specific fact the summary's prose had
// paraphrased away (e.g. "family camping trips" instead of the actual
// locations) would fail even though the correct summary was retrieved.
// Only a *grounded* fact should be surfaced -- an ungrounded one already
// failed groundingCheck's own re-verification and shouldn't be presented
// as reliable.
func TestRetrieve_SurfacesGroundedKeyFactsAlongsideSummary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-retrieve-keyfacts"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	summaryID := "sum_test-retrieve-keyfacts_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period,
		"Alex went on a family camping trip and found it relaxing.", "")
	insertKeyFact(t, s, scope, summaryID, "Alex camped at the beach, in the mountains, and in the forest.", true)
	insertKeyFact(t, s, scope, summaryID, "Alex camped on the moon.", false)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember where Alex went camping?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if !strings.Contains(result.ContextMessage, "Alex camped at the beach, in the mountains, and in the forest.") {
		t.Errorf("context message missing the grounded key fact, got: %q", result.ContextMessage)
	}
	if strings.Contains(result.ContextMessage, "Alex camped on the moon.") {
		t.Errorf("context message includes an ungrounded key fact — it should have been filtered out, got: %q", result.ContextMessage)
	}
}

// TestRetrieve_CitesKeyFactsAtFactGranularity is the citation-model
// regression docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's Phase 0 exists
// for: a picked summary must produce its own RefKindSummary prose
// citation *plus* one RefKindMemory citation per grounded key fact
// (ParentSummaryID set to the owning summary) — not a single bundled
// citation that only ever names one "most relevant" fact the old
// summaryCitationSnippet produced.
func TestRetrieve_CitesKeyFactsAtFactGranularity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-retrieve-fact-citations"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	summaryID := "sum_test-retrieve-fact-citations_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period,
		"Alex went on a family camping trip and found it relaxing.", "")
	insertKeyFact(t, s, scope, summaryID, "Alex camped at the beach, in the mountains, and in the forest.", true)
	insertKeyFact(t, s, scope, summaryID, "Alex found camping relaxing.", true)
	insertKeyFact(t, s, scope, summaryID, "Alex camped on the moon.", false)

	var factIDs []int64
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `select id from summary_key_facts where summary_id = $1 and grounded = true order by id`, summaryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			factIDs = append(factIDs, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load seeded key fact ids: %v", err)
	}
	if len(factIDs) != 2 {
		t.Fatalf("seeded %d grounded fact ids, want 2", len(factIDs))
	}

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember where Alex went camping?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	var sawProseCitation bool
	factCitationsSeen := map[string]string{} // memory ref id -> snippet
	for _, c := range result.Citations {
		if c.Ref.Kind == identity.RefKindSummary && c.Ref.ID == summaryID {
			sawProseCitation = true
		}
		if c.Ref.Kind == identity.RefKindMemory && c.ParentSummaryID == summaryID {
			factCitationsSeen[c.Ref.ID] = c.Snippet
		}
	}
	if !sawProseCitation {
		t.Errorf("expected a RefKindSummary prose citation for %s alongside the fact citations, got: %+v", summaryID, result.Citations)
	}
	if len(factCitationsSeen) != 2 {
		t.Fatalf("got %d RefKindMemory fact citations with ParentSummaryID=%s, want 2: %+v", len(factCitationsSeen), summaryID, result.Citations)
	}
	for _, factID := range factIDs {
		wantRefID := memoryIDForKeyFact(factID)
		snippet, ok := factCitationsSeen[wantRefID]
		if !ok {
			t.Errorf("no fact citation with Ref.ID = %q (fact row id %d)", wantRefID, factID)
			continue
		}
		if snippet != "Alex camped at the beach, in the mountains, and in the forest." && snippet != "Alex found camping relaxing." {
			t.Errorf("fact citation %s snippet = %q, want one of the two grounded fact texts", wantRefID, snippet)
		}
	}
	// The ungrounded fact must never get a citation at all.
	for _, snippet := range factCitationsSeen {
		if strings.Contains(snippet, "moon") {
			t.Errorf("ungrounded fact leaked into citations: %q", snippet)
		}
	}
}

// insertKeyFactWithEmbedding seeds a summary_key_facts row with an
// explicit embedding and embedding_model — insertKeyFact doesn't set
// either, since none of its own callers needed semantic fact ranking.
func insertKeyFactWithEmbedding(t *testing.T, s *Store, scope identity.Scope, summaryID, fact string, embeddingModel string, vec []float32) {
	t.Helper()
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	factCT, err := enc.Encrypt(fact)
	if err != nil {
		t.Fatalf("encrypt test key fact: %v", err)
	}
	embeddingLiteral := pgfmt.VectorLiteral(vec)
	err = dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			insert into summary_key_facts (summary_id, fact, grounded, key_version, scope_kind, scope_owner, embedding, embedding_model)
			values ($1, $2, true, $3, $4, $5, $6::vector, $7)
		`, summaryID, factCT, keyVersion, scope.Kind, scope.Owner, embeddingLiteral, embeddingModel)
		return err
	})
	if err != nil {
		t.Fatalf("insert test key fact with embedding for %s: %v", summaryID, err)
	}
}

// TestRetrieve_CitesEntityAttributesAtFactGranularity is the entity-side
// counterpart of TestRetrieve_CitesKeyFactsAtFactGranularity: a stage1
// substring-matched entity must produce one RefKindMemory citation per
// live attribute, not the single bundled RefKindEntity citation the old
// citation model produced for the whole entity at once.
func TestRetrieve_CitesEntityAttributesAtFactGranularity(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-retrieve-entity-fact-citations"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntityWithEmbedding(t, s, scope, "person:zephyrbatch", "person", "Zephyrbatch",
		`{"role":"engineer","team":"platform"}`)

	messages := []provider.Message{{Role: provider.RoleUser, Content: "tell me about Zephyrbatch"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	var byEntityRowIDs map[string][]entityattrs.Attribute
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		byEntityRowIDs, err = entityattrs.CurrentWithIDsForEntities(ctx, tx, s.keys, scope, []string{"person:zephyrbatch"})
		return err
	}); err != nil {
		t.Fatalf("load real attribute row ids: %v", err)
	}
	attrs := byEntityRowIDs["person:zephyrbatch"]
	if len(attrs) != 2 {
		t.Fatalf("seeded entity has %d live attribute rows, want 2", len(attrs))
	}

	wantRefIDs := map[string]bool{}
	for _, a := range attrs {
		wantRefIDs[a.ID] = true
	}

	gotRefIDs := map[string]bool{}
	var sawEntityRef bool
	for _, c := range result.Citations {
		if c.Ref.Kind == identity.RefKindMemory && wantRefIDs[c.Ref.ID] {
			gotRefIDs[c.Ref.ID] = true
		}
	}
	for _, r := range result.Refs {
		if r.Kind == identity.RefKindEntity && r.ID == "person:zephyrbatch" {
			sawEntityRef = true
		}
	}
	if len(gotRefIDs) != 2 {
		t.Errorf("got %d RefKindMemory attribute citations, want 2 (one per live attribute): %+v", len(gotRefIDs), result.Citations)
	}
	if !sawEntityRef {
		t.Error("expected a plain RefKindEntity ref for person:zephyrbatch in Refs (for graph-walk seeding/dedup), independent of the attribute citations")
	}
}

// TestRetrieve_SemanticFactRankingPromotesTheRealAnswerOverALexicalTie is
// the DB-integration counterpart of
// TestRankKeyFacts_SemanticSimilarityBreaksTheRealLexicalTie_gpt4_45189cb4
// — proves the SQL cosine-similarity computation in loadKeyFacts works
// end to end against a real Postgres/pgvector column, not just the pure
// ranking function in isolation. Orthogonal hand-built vectors (query and
// the NFL fact share axis 0; every other fact sits on its own separate
// axis) make the expected ranking unambiguous: cosine(query, NFL) = 1.0,
// cosine(query, anything else) = 0.0 — this is the SQL expression being
// tested, not a claim about what a real embedding model would produce for
// this text.
func TestRetrieve_SemanticFactRankingPromotesTheRealAnswerOverALexicalTie(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-semantic-fact-rank"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	summaryID := "sum_test-semantic-fact-rank_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period, "A busy day covering several unrelated topics.", "")

	model := s.currentEmbeddingModel()
	axis := func(i int) []float32 {
		v := make([]float32, 1536)
		v[i] = 1
		return v
	}
	// More than guaranteedFactMaxCount facts, so guaranteedFact takes the
	// ranking branch rather than joining everything unconditionally.
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user noted rising concern about climate events.", model, axis(1))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user watched the Kansas City Chiefs win an NFL playoff game.", model, axis(0))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user mentioned an interest in gardening.", model, axis(2))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user discussed a painting class.", model, axis(3))

	// The fake test embedder (scope_isolation_test.go) returns the same
	// content-independent vector for every input, which happens to be
	// axis(0) (vec[0]=1) — matching the NFL fact's hand-built embedding
	// exactly and every other fact's not at all, which is what makes this
	// test deterministic without a real embedding model. The query shares
	// "watched" with the NFL fact alone (a clean, unique lexical winner,
	// not a tie) — rankKeyFacts fuses lexical and semantic rank via RRF
	// rather than letting semantic override lexical outright (see its own
	// doc comment for the real regression that caused that design change),
	// so this fixture gives the NFL fact a real edge on *both* signals,
	// which is what should make it win decisively enough to clear
	// factMarkerFusedMargin. Asserting on the "(most relevant)" marker,
	// not just the fact's presence (writeKeyFacts writes every fact
	// regardless of rank at this small a scale), is what actually
	// discriminates semantic ranking from the lexical fallback.
	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember what I watched this month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := "(most relevant) The user watched the Kansas City Chiefs win an NFL playoff game."
	if !strings.Contains(result.ContextMessage, want) {
		t.Errorf("context message = %q, want it to contain %q", result.ContextMessage, want)
	}
}

// TestRetrieve_SemanticFactRankingKillSwitchFallsBackToNoMarker confirms
// HUPI_ENABLE_SEMANTIC_FACT_RANKING=false reproduces the pre-embedding
// behavior end to end: with no shared vocabulary between the query and
// any fact, lexical scoring ties everything at 0 and
// mostRelevantFactIndex refuses to fabricate a "most relevant" marker —
// the same fixture as
// TestRetrieve_SemanticFactRankingPromotesTheRealAnswerOverALexicalTie,
// with the opposite expectation, proving the marker in that test is
// really caused by semantic ranking and not some other incidental factor.
func TestRetrieve_SemanticFactRankingKillSwitchFallsBackToNoMarker(t *testing.T) {
	t.Setenv("HUPI_ENABLE_SEMANTIC_FACT_RANKING", "false")
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-semantic-fact-rank-killswitch"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	summaryID := "sum_test-semantic-fact-rank-killswitch_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period, "A busy day covering several unrelated topics.", "")

	model := s.currentEmbeddingModel()
	axis := func(i int) []float32 {
		v := make([]float32, 1536)
		v[i] = 1
		return v
	}
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user noted rising concern about climate events.", model, axis(1))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user watched the Kansas City Chiefs win an NFL playoff game.", model, axis(0))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user mentioned an interest in gardening.", model, axis(2))
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "The user discussed a painting class.", model, axis(3))

	messages := []provider.Message{{Role: provider.RoleUser, Content: "do you remember what I did this month?"}}
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if strings.Contains(result.ContextMessage, "(most relevant)") {
		t.Errorf("context message = %q, want no fabricated \"(most relevant)\" marker with the kill switch off and no shared lexical vocabulary", result.ContextMessage)
	}
}

// TestRetrieve_FactEmbeddedUnderDifferentModelFallsBackToLexical confirms
// a fact embedded under a since-changed provider doesn't get ranked by a
// meaningless cross-model cosine distance — loadKeyFacts' embedding_model
// check should treat it the same as "not embedded at all."
func TestRetrieve_FactEmbeddedUnderDifferentModelFallsBackToLexical(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-fact-stale-model"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	period := "2026-01-01"
	summaryID := "sum_test-fact-stale-model_2026-01-01_daily_v1"
	insertSummary(t, s, scope, summaryID, period, "A day with an entry from a retired embedding model.", "")

	axis := func(i int) []float32 {
		v := make([]float32, 1536)
		v[i] = 1
		return v
	}
	// Embedded under a model that is no longer active — even though this
	// vector would otherwise win on similarity, it must not be trusted.
	insertKeyFactWithEmbedding(t, s, scope, summaryID, "An irrelevant fact from a stale embedding model.", "retired-vendor:retired-model", axis(0))
	insertKeyFact(t, s, scope, summaryID, "melanie went camping at the beach", true)
	insertKeyFact(t, s, scope, summaryID, "melanie went camping in the mountains", true)
	insertKeyFact(t, s, scope, summaryID, "melanie collects stamps", true)

	enc, _, err := s.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	var facts []keyFact
	err = dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		facts, err = loadKeyFacts(ctx, tx, summaryID, enc, pgfmt.VectorLiteral(axis(0)), s.currentEmbeddingModel())
		return err
	})
	if err != nil {
		t.Fatalf("loadKeyFacts: %v", err)
	}
	for _, f := range facts {
		if strings.Contains(f.text, "stale embedding model") && f.similarity.Valid {
			t.Errorf("fact embedded under a non-active model has a valid similarity = %v, want invalid (NULL)", f.similarity)
		}
	}
}

// insertEntityWithEmbedding seeds an entity row with an embedding set —
// scope_isolation_test.go's insertEntity doesn't, since none of its own
// tests need vector search. Real encryption, like insertSummary.
func insertEntityWithEmbedding(t *testing.T, s *Store, scope identity.Scope, id, kind, name, attrsJSON string) {
	t.Helper()
	_, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
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
			insert into entities (id, kind, name, scope_kind, scope_owner, key_version, embedding)
			values ($1, $2, $3, $4, $5, $6, $7::vector)
		`, id, kind, name, scope.Kind, scope.Owner, keyVersion, embeddingLiteral)
		return err
	})
	if err != nil {
		t.Fatalf("insert test entity %s: %v", id, err)
	}
	seedEntityAttributesAsMemories(t, s, scope, id, attrsJSON)
}

// seedEntityAttributesAsMemories inserts one live memories row per key
// in attrsJSON — the shape internal/consolidation/store.go's
// upsertEntities actually writes post-cutover
// (docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md Phase 0), not the legacy
// entities.attributes blob these test helpers used to populate
// directly. Shared by every test helper in this package that seeds an
// entity with attributes (insertEntityWithEmbedding,
// insertEntityWithEmbeddingIndex, scope_isolation_test.go's insertEntity).
func seedEntityAttributesAsMemories(t *testing.T, s *Store, scope identity.Scope, entityID, attrsJSON string) {
	t.Helper()
	var attrs map[string]string
	if attrsJSON != "" && attrsJSON != "{}" {
		if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
			t.Fatalf("parse test entity attrs JSON for %s: %v", entityID, err)
		}
	}
	enc, keyVersion, err := s.keys.GetOrCreate(context.Background(), scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	if err := dbscope.Run(context.Background(), s.db, scope, scope, func(tx *sql.Tx) error {
		for key, value := range attrs {
			ct, err := enc.Encrypt(value)
			if err != nil {
				return err
			}
			rowID, err := entityattrs.NewID()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`
				insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
				values ($1, $2, $3, $4, $5, $6, $7, true, true)
			`, rowID, scope.Kind, scope.Owner, entityID, key, ct, keyVersion); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed memories for entity %s attributes: %v", entityID, err)
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
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
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
	result, err := s.Retrieve(ctx, scope, scope, messages, time.Now())
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
