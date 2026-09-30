package consolidation

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

func TestFormatKnownEntitiesIncludesNameAndAttributes(t *testing.T) {
	known := []knownEntityContext{
		{id: "organization:wells-fargo", name: "Wells Fargo", attributes: map[string]string{"preapproval_amount": "$250,000"}},
	}
	got := formatKnownEntities(known)
	if !strings.Contains(got, "Wells Fargo") || !strings.Contains(got, "organization:wells-fargo") || !strings.Contains(got, "preapproval_amount") {
		t.Errorf("formatKnownEntities() = %q, want it to contain the entity's name, id, and attributes", got)
	}
}

// TestFormatKnownEntitiesOmitsEntitiesWithNoAttributes confirms an entity
// with nothing on record yet contributes nothing to compare a new
// attribute against — real signal that this doesn't add empty noise to
// the prompt.
func TestFormatKnownEntitiesOmitsEntitiesWithNoAttributes(t *testing.T) {
	known := []knownEntityContext{
		{id: "person:jane", name: "Jane", attributes: nil},
	}
	if got := formatKnownEntities(known); got != "" {
		t.Errorf("formatKnownEntities() = %q, want empty for an entity with no attributes", got)
	}
}

func TestFormatKnownEntitiesEmptyInput(t *testing.T) {
	if got := formatKnownEntities(nil); got != "" {
		t.Errorf("formatKnownEntities(nil) = %q, want empty", got)
	}
}

// TestFindKnownEntitiesMatchesNameSubstring is Phase C sub-problem 1's
// real retrieval step (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): mirrors
// internal/store/retrieve.go's stage1EntityMatches pattern, just in the
// opposite direction (does an existing entity's name appear in today's
// source text, not does the query mention a known entity).
func TestFindKnownEntitiesMatchesNameSubstring(t *testing.T) {
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
	runner := New(db, keys, nil, nil, nil)

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-find-known-entities"}
	ctx := context.Background()
	t.Cleanup(func() {
		_ = dbscope.Run(context.Background(), db, scope, scope, func(tx *sql.Tx) error {
			_, _ = tx.Exec(`delete from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
			return nil
		})
	})

	enc, keyVersion, err := runner.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrsCT, err := enc.Encrypt(`{"preapproval_amount":"$250,000"}`)
	if err != nil {
		t.Fatalf("encrypt attributes: %v", err)
	}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, key_version, scope_kind, scope_owner)
			values ('organization:wells-fargo', 'organization', 'Wells Fargo', $1, $2, $3, $4)
		`, attrsCT, keyVersion, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	// Also seed an unrelated entity to confirm it's correctly excluded —
	// findKnownEntities should be selective, not "return everything".
	unrelatedCT, err := enc.Encrypt(`{}`)
	if err != nil {
		t.Fatalf("encrypt unrelated attributes: %v", err)
	}
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, key_version, scope_kind, scope_owner)
			values ('organization:chase', 'organization', 'Chase', $1, $2, $3, $4)
		`, unrelatedCT, keyVersion, scope.Kind, scope.Owner)
		return err
	}); err != nil {
		t.Fatalf("seed unrelated entity: %v", err)
	}

	var found []knownEntityContext
	if err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var err error
		found, err = runner.findKnownEntities(ctx, tx, scope, "I got a new pre-approval letter from Wells Fargo today.")
		return err
	}); err != nil {
		t.Fatalf("findKnownEntities: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("findKnownEntities() returned %d entities, want exactly 1 (Wells Fargo, not the unrelated Chase entity), got: %+v", len(found), found)
	}
	if found[0].id != "organization:wells-fargo" {
		t.Errorf("findKnownEntities()[0].id = %q, want %q", found[0].id, "organization:wells-fargo")
	}
	if found[0].attributes["preapproval_amount"] != "$250,000" {
		t.Errorf("findKnownEntities()[0].attributes = %+v, want it to include the real, decrypted preapproval_amount", found[0].attributes)
	}
}
