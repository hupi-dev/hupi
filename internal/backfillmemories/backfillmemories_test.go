package backfillmemories

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
		tx.Exec(`delete from memory_relations where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summary_key_facts where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from summaries where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
}

func seedEntity(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id, kind, name string, attrs map[string]string) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	var attrsJSON string
	if attrs != nil {
		b, err := json.Marshal(attrs)
		if err != nil {
			t.Fatalf("marshal attrs: %v", err)
		}
		attrsJSON = string(b)
	}
	attrsCT, err := enc.Encrypt(attrsJSON)
	if err != nil {
		t.Fatalf("encrypt attributes: %v", err)
	}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner)
			values ($1, $2, $3, $4, $5, $6)
		`, id, kind, name, attrsCT, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

func seedSummary(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, id string) {
	t.Helper()
	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	ct, err := enc.Encrypt("summary prose")
	if err != nil {
		t.Fatalf("encrypt summary: %v", err)
	}
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into summaries (id, period, level, summary, scope_kind, scope_owner, grounding_checked)
			values ($1, '2026-01-01', 'daily', $2, $3, $4, true)
		`, id, ct, scope.Kind, scope.Owner)
		return err
	})
	if err != nil {
		t.Fatalf("seed summary %s: %v", id, err)
	}
}

func seedKeyFact(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, summaryID, fact string, grounded bool, sourceEpisodeIDs []string) int64 {
	t.Helper()
	enc, keyVersion, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	ct, err := enc.Encrypt(fact)
	if err != nil {
		t.Fatalf("encrypt fact: %v", err)
	}
	var factID int64
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, key_version, scope_kind, scope_owner)
			values ($1, $2, $3::text[], $4, $5, $6, $7)
			returning id
		`, summaryID, ct, pgfmt.TextArray(sourceEpisodeIDs), grounded, keyVersion, scope.Kind, scope.Owner).Scan(&factID)
	})
	if err != nil {
		t.Fatalf("seed key fact: %v", err)
	}
	return factID
}

func TestStatus_CountsPendingAttributesAndKeyFacts(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-status"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, keys, scope, "person:alice", "person", "Alice", map[string]string{"city": "SF"})
	seedSummary(t, ctx, db, keys, scope, "sum_backfill-status")
	seedKeyFact(t, ctx, db, keys, scope, "sum_backfill-status", "Alice likes coffee", true, nil)

	r := New(db, keys)
	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.AttributeEntitiesPending != 1 {
		t.Errorf("AttributeEntitiesPending = %d, want 1", st.AttributeEntitiesPending)
	}
	if st.KeyFactsPending != 1 {
		t.Errorf("KeyFactsPending = %d, want 1", st.KeyFactsPending)
	}
	if st.Total() != 2 {
		t.Errorf("Total() = %d, want 2", st.Total())
	}
}

func TestContinue_BackfillsAttributesIntoSeparateMemoriesRows(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-attrs"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, keys, scope, "person:bob", "person", "Bob", map[string]string{
		"city":       "NYC",
		"occupation": "engineer",
	})

	r := New(db, keys)
	processed, table, done, err := r.Continue(ctx, scope, 100)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if table != "entities" {
		t.Errorf("table = %q, want %q", table, "entities")
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1 (one entity, regardless of its attribute count)", processed)
	}
	if done {
		t.Error("done = true after the first batch, want false — Continue doesn't re-check after its own batch")
	}

	// Two attribute keys -> two separate memories rows, each independently
	// decryptable back to its own original value, each is_static=true,
	// each pointing at the entity via entity_id (not summary_id, which
	// should be null for an attribute-sourced memory). Queried inside
	// dbscope.Run, same as Continue's own queries — memories has RLS
	// enabled, so a plain, scope-unaware query against the hupi_app role
	// this test connects as would see zero rows, not an error, which
	// would be easy to misread as "nothing was backfilled."
	type memRow struct {
		ct                              []byte
		isStatic, isInference, grounded bool
		sourceCount, keyVersion         int
		summaryID                       sql.NullString
		attributeKey                    sql.NullString
	}
	var memRows []memRow
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select content, is_static, is_inference, source_count, grounded, summary_id, key_version, attribute_key
			from memories where scope_kind = $1 and scope_owner = $2 and entity_id = 'person:bob'
			order by id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mr memRow
			if err := rows.Scan(&mr.ct, &mr.isStatic, &mr.isInference, &mr.sourceCount, &mr.grounded, &mr.summaryID, &mr.keyVersion, &mr.attributeKey); err != nil {
				return err
			}
			memRows = append(memRows, mr)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("query memories: %v", err)
	}

	enc, _, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve key: %v", err)
	}

	wantKeyForValue := map[string]string{"NYC": "city", "engineer": "occupation"}
	gotValues := map[string]bool{}
	for _, mr := range memRows {
		plain, err := enc.Decrypt(mr.ct)
		if err != nil {
			t.Fatalf("decrypt memory content: %v", err)
		}
		gotValues[plain] = true
		if !mr.attributeKey.Valid || mr.attributeKey.String != wantKeyForValue[plain] {
			t.Errorf("attribute_key = %v for value %q, want %q", mr.attributeKey, plain, wantKeyForValue[plain])
		}
		if !mr.isStatic {
			t.Error("is_static = false, want true for an attribute-sourced memory")
		}
		if mr.isInference {
			t.Error("is_inference = true, want false — this backfill never produces inferred facts")
		}
		if mr.sourceCount != 1 {
			t.Errorf("source_count = %d, want 1", mr.sourceCount)
		}
		if !mr.grounded {
			t.Error("grounded = false, want true for an attribute (attributes have no ungrounded concept)")
		}
		if mr.summaryID.Valid {
			t.Errorf("summary_id = %q, want NULL for an attribute-sourced memory", mr.summaryID.String)
		}
		if mr.keyVersion != 1 {
			t.Errorf("key_version = %d, want 1", mr.keyVersion)
		}
	}
	if len(memRows) != 2 {
		t.Fatalf("got %d memories rows for person:bob, want 2", len(memRows))
	}
	for _, want := range []string{"NYC", "engineer"} {
		if !gotValues[want] {
			t.Errorf("no memories row decrypted to %q", want)
		}
	}

	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status after Continue: %v", err)
	}
	if st.AttributeEntitiesPending != 0 {
		t.Errorf("AttributeEntitiesPending after Continue = %d, want 0", st.AttributeEntitiesPending)
	}
}

func TestContinue_BackfillsKeyFactsPreservingCiphertextAndMetadata(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-facts"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedSummary(t, ctx, db, keys, scope, "sum_backfill-facts")
	factID := seedKeyFact(t, ctx, db, keys, scope, "sum_backfill-facts", "Bob moved to NYC in March", true, []string{"ep_1", "ep_2"})

	r := New(db, keys)
	processed, table, _, err := r.Continue(ctx, scope, 100)
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if table != "summary_key_facts" {
		t.Errorf("table = %q, want %q", table, "summary_key_facts")
	}
	if processed != 1 {
		t.Errorf("processed = %d, want 1", processed)
	}

	var ct []byte
	var isStatic, grounded bool
	var summaryID string
	var sourceIDsLit string
	var keyVersion int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select content, is_static, grounded, summary_id, source_episode_ids::text, key_version
			from memories where id = $1
		`, memoryIDForKeyFact(factID)).Scan(&ct, &isStatic, &grounded, &summaryID, &sourceIDsLit, &keyVersion)
	})
	if err != nil {
		t.Fatalf("query backfilled memory: %v", err)
	}
	if isStatic {
		t.Error("is_static = true, want false for a key-fact-sourced memory")
	}
	if !grounded {
		t.Error("grounded = false, want true (seeded fact was grounded)")
	}
	if summaryID != "sum_backfill-facts" {
		t.Errorf("summary_id = %q, want %q", summaryID, "sum_backfill-facts")
	}
	gotSourceIDs := pgfmt.ParseTextArray(sourceIDsLit)
	if len(gotSourceIDs) != 2 || gotSourceIDs[0] != "ep_1" || gotSourceIDs[1] != "ep_2" {
		t.Errorf("source_episode_ids = %v, want [ep_1 ep_2]", gotSourceIDs)
	}
	if keyVersion != 1 {
		t.Errorf("key_version = %d, want 1", keyVersion)
	}

	enc, err := keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		t.Fatalf("resolve key version %d: %v", keyVersion, err)
	}
	plain, err := enc.Decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt backfilled fact content: %v", err)
	}
	if plain != "Bob moved to NYC in March" {
		t.Errorf("decrypted content = %q, want %q", plain, "Bob moved to NYC in March")
	}
}

func TestContinue_IsIdempotentOnRerun(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-idempotent"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, keys, scope, "person:carol", "person", "Carol", map[string]string{"city": "LA"})
	seedSummary(t, ctx, db, keys, scope, "sum_backfill-idempotent")
	seedKeyFact(t, ctx, db, keys, scope, "sum_backfill-idempotent", "Carol likes hiking", true, nil)

	r := New(db, keys)
	for i := 0; i < 2; i++ {
		if _, _, _, err := r.Continue(ctx, scope, 100); err != nil {
			t.Fatalf("Continue call %d: %v", i+1, err)
		}
	}
	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Total() != 0 {
		t.Fatalf("Total() after fully backfilling = %d, want 0", st.Total())
	}

	// Re-running Continue after everything is already backfilled must do
	// nothing — no duplicate rows, done=true.
	processed, _, done, err := r.Continue(ctx, scope, 100)
	if err != nil {
		t.Fatalf("Continue after already-done: %v", err)
	}
	if processed != 0 || !done {
		t.Errorf("Continue after already-done: processed=%d done=%v, want processed=0 done=true", processed, done)
	}

	var count int
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `select count(*) from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count memories: %v", err)
	}
	if count != 2 { // one attribute + one key fact, never duplicated
		t.Errorf("memories row count = %d, want 2 (no duplicates from re-running)", count)
	}
}

func TestContinue_EntityWithNoAttributesIsNotPendingForever(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-backfill-empty-attrs"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	// nil/empty attrs still produces a non-null, "{}"-shaped ciphertext —
	// the real shape upsertEntities can leave behind for an entity that
	// was only ever referenced by entities_touched, never given a real
	// attribute.
	seedEntity(t, ctx, db, keys, scope, "person:dave", "person", "Dave", map[string]string{})

	r := New(db, keys)
	st, err := r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status before Continue: %v", err)
	}
	if st.AttributeEntitiesPending != 1 {
		t.Fatalf("AttributeEntitiesPending before Continue = %d, want 1", st.AttributeEntitiesPending)
	}

	if _, _, _, err := r.Continue(ctx, scope, 100); err != nil {
		t.Fatalf("Continue: %v", err)
	}

	st, err = r.Status(ctx, scope)
	if err != nil {
		t.Fatalf("Status after Continue: %v", err)
	}
	if st.AttributeEntitiesPending != 0 {
		t.Errorf("AttributeEntitiesPending after Continue = %d, want 0 — an empty-attribute entity must not stay pending forever", st.AttributeEntitiesPending)
	}
}
