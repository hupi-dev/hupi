package entityattrs

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
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
		tx.Exec(`delete from memories where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		return nil
	})
}

func seedEntity(t *testing.T, ctx context.Context, db *sql.DB, scope identity.Scope, id string) {
	t.Helper()
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, scope_kind, scope_owner) values ($1, 'project', $2, $3, $4)
		`, id, id, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

// insertAttr inserts one memories row directly (not via upsertEntities,
// which lives in internal/consolidation and would be a circular import)
// — same shape that package's upsertEntities writes.
func insertAttr(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, entityID, id, key, value string, supersedes *string, grounded bool) {
	t.Helper()
	enc, keyVersion, err := keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve encryption key: %v", err)
	}
	ct, err := enc.Encrypt(value)
	if err != nil {
		t.Fatalf("encrypt %s: %v", key, err)
	}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded, supersedes)
			values ($1, $2, $3, $4, $5, $6, $7, true, $8, $9)
		`, id, scope.Kind, scope.Owner, entityID, key, ct, keyVersion, grounded, supersedes)
		return err
	}); err != nil {
		t.Fatalf("insert attribute row %s: %v", id, err)
	}
}

func TestCurrent_ReturnsOnlyNonSupersededRows(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entityattrs-current"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, scope, "project:widget")
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_1", "language", "Go", nil, true)
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_2", "status", "active", nil, true)

	attrs, err := dbscopeCurrent(t, ctx, db, keys, scope, "project:widget")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if attrs["language"] != "Go" || attrs["status"] != "active" {
		t.Errorf("attrs = %+v, want language=Go status=active", attrs)
	}

	// Supersede "language" with a new value — the old row must no longer
	// appear, and the new row's value must replace it.
	id2 := "mem_1"
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_3", "language", "Rust", &id2, true)

	attrs, err = dbscopeCurrent(t, ctx, db, keys, scope, "project:widget")
	if err != nil {
		t.Fatalf("Current after supersede: %v", err)
	}
	if attrs["language"] != "Rust" {
		t.Errorf("language = %q after supersede, want %q", attrs["language"], "Rust")
	}
	if len(attrs) != 2 {
		t.Errorf("attrs = %+v, want exactly 2 keys (language, status) — the superseded row must not also appear", attrs)
	}
}

func TestCurrent_ExcludesTombstonedKeys(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entityattrs-tombstone"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, scope, "project:widget")
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_1", "concurrency_limit", "500", nil, true)

	// A tombstone (grounded=false) superseding the live row — upsertEntities'
	// replace mode / SupersedesKeys deletion path.
	id1 := "mem_1"
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_2", "concurrency_limit", "", &id1, false)

	attrs, err := dbscopeCurrent(t, ctx, db, keys, scope, "project:widget")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if _, exists := attrs["concurrency_limit"]; exists {
		t.Errorf("concurrency_limit = %+v, want the key absent entirely — a tombstoned key must never surface as a real (empty) value", attrs)
	}
}

func TestCurrentRowIDs_IncludesTombstones(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entityattrs-rowids"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, scope, "project:widget")
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_1", "concurrency_limit", "500", nil, true)
	id1 := "mem_1"
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_2", "concurrency_limit", "", &id1, false)

	var rowIDs map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		rowIDs, err = CurrentRowIDs(ctx, tx, scope, "project:widget")
		return err
	}); err != nil {
		t.Fatalf("CurrentRowIDs: %v", err)
	}
	if rowIDs["concurrency_limit"] != "mem_2" {
		t.Errorf("CurrentRowIDs()[concurrency_limit] = %q, want %q (the tombstone, so a later write extends the same chain)", rowIDs["concurrency_limit"], "mem_2")
	}
}

func TestCurrentForEntities_BatchesAcrossMultipleEntities(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entityattrs-batch"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, scope, "project:alpha")
	seedEntity(t, ctx, db, scope, "project:beta")
	insertAttr(t, ctx, db, keys, scope, "project:alpha", "mem_a", "owner", "alice", nil, true)
	insertAttr(t, ctx, db, keys, scope, "project:beta", "mem_b", "owner", "bob", nil, true)

	var attrsByID map[string]map[string]string
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrsByID, err = CurrentForEntities(ctx, tx, keys, scope, []string{"project:alpha", "project:beta", "project:nonexistent"})
		return err
	}); err != nil {
		t.Fatalf("CurrentForEntities: %v", err)
	}
	if attrsByID["project:alpha"]["owner"] != "alice" {
		t.Errorf("project:alpha owner = %q, want %q", attrsByID["project:alpha"]["owner"], "alice")
	}
	if attrsByID["project:beta"]["owner"] != "bob" {
		t.Errorf("project:beta owner = %q, want %q", attrsByID["project:beta"]["owner"], "bob")
	}
	if _, exists := attrsByID["project:nonexistent"]; exists {
		t.Errorf("project:nonexistent unexpectedly present in result: %+v", attrsByID["project:nonexistent"])
	}
}

func TestCurrentWithIDsForEntities_ReturnsRowIDsAlongsideValues(t *testing.T) {
	db, keys := testDB(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-entityattrs-withids"}
	t.Cleanup(func() { cleanup(t, db, scope) })

	seedEntity(t, ctx, db, scope, "project:widget")
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_1", "language", "Go", nil, true)
	id1 := "mem_1"
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_2", "language", "Rust", &id1, true)
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_3", "status", "active", nil, true)
	// A tombstone must not appear at all — same exclusion CurrentForEntities applies.
	id3 := "mem_3"
	insertAttr(t, ctx, db, keys, scope, "project:widget", "mem_4", "status", "", &id3, false)

	var byEntity map[string][]Attribute
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		byEntity, err = CurrentWithIDsForEntities(ctx, tx, keys, scope, []string{"project:widget"})
		return err
	}); err != nil {
		t.Fatalf("CurrentWithIDsForEntities: %v", err)
	}

	attrs := byEntity["project:widget"]
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want exactly 1 (language — status is tombstoned): %+v", len(attrs), attrs)
	}
	if attrs[0].ID != "mem_2" || attrs[0].Key != "language" || attrs[0].Value != "Rust" {
		t.Errorf("attrs[0] = %+v, want {ID: mem_2, Key: language, Value: Rust}", attrs[0])
	}
}

func TestNewID_ProducesDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if seen[id] {
			t.Fatalf("NewID produced a duplicate: %s", id)
		}
		seen[id] = true
	}
}

// dbscopeCurrent wraps Current in its own dbscope.Run — every test above
// needs this same one-line wrapper, so it's factored out here rather
// than repeated five times.
func dbscopeCurrent(t *testing.T, ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, entityID string) (map[string]string, error) {
	t.Helper()
	var attrs map[string]string
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		attrs, err = Current(ctx, tx, keys, scope, entityID)
		return err
	})
	return attrs, err
}
