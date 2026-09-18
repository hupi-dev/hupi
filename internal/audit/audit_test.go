package audit

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"hupi/internal/identity"
)

func testDB(t *testing.T) *sql.DB {
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
	return db
}

func TestLogStandaloneAndQueryRoundTrip(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:audit-test-a"}
	actor := "user:audit-test-a"
	t.Cleanup(func() {
		db.Exec(`delete from audit_log where actor = $1`, actor)
	})

	entry := Entry{
		EventType:      EventCapture,
		Actor:          actor,
		ActingScope:    scope,
		WorkspaceScope: scope,
		Detail:         map[string]any{"note": "audit_test roundtrip"},
	}
	if err := LogStandalone(ctx, db, entry); err != nil {
		t.Fatalf("LogStandalone: %v", err)
	}

	got, err := Query(ctx, db, QueryFilter{Actor: actor, EventType: EventCapture, Limit: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Query returned %d rows, want 1", len(got))
	}
	row := got[0]
	if row.Actor != actor || row.EventType != EventCapture {
		t.Errorf("got {Actor: %q, EventType: %q}, want {%q, %q}", row.Actor, row.EventType, actor, EventCapture)
	}
	if row.ActingScopeKind != scope.Kind || row.ActingScopeOwner != scope.Owner {
		t.Errorf("acting scope = {%s %s}, want %+v", row.ActingScopeKind, row.ActingScopeOwner, scope)
	}
	if row.Detail == "" || row.Detail == "{}" {
		t.Errorf("Detail = %q, want the marshaled note", row.Detail)
	}
}

func TestQueryLimitZeroReturnsNoRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// QueryFilter's own doc comment: Limit==0 means "no default applied,"
	// which SQL's LIMIT 0 correctly turns into zero rows, not "unbounded."
	got, err := Query(ctx, db, QueryFilter{EventType: EventCapture, Limit: 0})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Query with Limit=0 returned %d rows, want 0", len(got))
	}
}
