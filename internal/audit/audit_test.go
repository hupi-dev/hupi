package audit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

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

	// A unique actor per run, not a fixed literal: audit_log is
	// deliberately insert-only for hupi_app (schema/0007 — no delete
	// grant, matching production's append-only guarantee), so this test
	// cannot clean up its own row afterward. A fixed actor id would let a
	// leftover row from a previous run collide with this run's query and
	// fail it with "want 1, got 2" — a real, previously-recurring
	// flake, not hypothetical.
	actor := fmt.Sprintf("user:audit-test-%d", time.Now().UnixNano())
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: actor}

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
