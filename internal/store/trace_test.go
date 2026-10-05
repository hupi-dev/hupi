package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
)

// See scope_isolation_test.go's doc comment for how to run these
// against a real instance.

// TestTrace_ResolvesRefKindMemoryAttributeAndKeyFact is the real
// regression this session's citation-model change needs: before
// internal/store/trace.go gained a RefKindMemory case, Store.Trace's
// own ref-resolution switch hard-errored ("unknown ref kind") the
// moment a real episode's retrieved_refs contained one — which, after
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's citation model shipped,
// is every single turn that cites a fact or an entity attribute. This
// confirms both memories shapes (a static attribute row and an event
// key-fact row) resolve correctly, not just that the switch doesn't
// error.
func TestTrace_ResolvesRefKindMemoryAttributeAndKeyFact(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scope := identity.Scope{Kind: identity.ScopeKindPrivate, Owner: "user:test-trace-memory"}
	t.Cleanup(func() { cleanupScope(t, s, scope) })

	insertEntity(t, s, scope, "person:dana", "person", "Dana")
	enc, keyVersion, err := s.keys.GetOrCreate(ctx, scope)
	if err != nil {
		t.Fatalf("resolve test encryption key: %v", err)
	}
	attrCT, err := enc.Encrypt("engineer")
	if err != nil {
		t.Fatalf("encrypt test attribute: %v", err)
	}
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
			values ('mem_test_dana_role', $1, $2, 'person:dana', 'role', $3, $4, true, true)
		`, scope.Kind, scope.Owner, attrCT, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("seed attribute memory: %v", err)
	}

	insertSummary(t, s, scope, "sum_test-trace-memory_2026-01-01_daily_v1", "2026-01-01", "Dana started a new role.", "")
	factCT, err := enc.Encrypt("Dana started as an engineer.")
	if err != nil {
		t.Fatalf("encrypt test fact: %v", err)
	}
	if err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into memories (id, scope_kind, scope_owner, summary_id, content, key_version, is_static, grounded)
			values ('mem_test_fact_1', $1, $2, 'sum_test-trace-memory_2026-01-01_daily_v1', $3, $4, false, true)
		`, scope.Kind, scope.Owner, factCT, keyVersion)
		return err
	}); err != nil {
		t.Fatalf("seed key-fact memory: %v", err)
	}

	ep := gateway.Episode{
		ID:         "ep_test_trace_memory",
		TS:         time.Now(),
		Type:       "interaction",
		InputText:  "what's Dana's new role?",
		OutputText: "Dana started as an engineer.",
		Hash:       "sha256:test-trace-memory",
		RetrievedRefs: []identity.Ref{
			{Kind: identity.RefKindMemory, Scope: scope, ID: "mem_test_dana_role"},
			{Kind: identity.RefKindMemory, Scope: scope, ID: "mem_test_fact_1"},
		},
	}
	if err := s.Capture(ctx, scope, ep); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	trace, err := s.Trace(ctx, scope, ep.ID, "test-operator")
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(trace.RetrievedMemories) != 2 {
		t.Fatalf("got %d retrieved memories, want 2: %+v", len(trace.RetrievedMemories), trace.RetrievedMemories)
	}

	var sawAttr, sawFact bool
	for _, m := range trace.RetrievedMemories {
		switch m.Ref.ID {
		case "mem_test_dana_role":
			sawAttr = true
			if !m.IsStatic || m.EntityID != "person:dana" || m.AttributeKey != "role" || m.Content != "engineer" {
				t.Errorf("attribute memory = %+v, want IsStatic=true EntityID=person:dana AttributeKey=role Content=engineer", m)
			}
		case "mem_test_fact_1":
			sawFact = true
			if m.IsStatic || m.SummaryID != "sum_test-trace-memory_2026-01-01_daily_v1" || m.Content != "Dana started as an engineer." {
				t.Errorf("key-fact memory = %+v, want IsStatic=false SummaryID=sum_test-trace-memory_2026-01-01_daily_v1 Content=\"Dana started as an engineer.\"", m)
			}
		}
	}
	if !sawAttr {
		t.Error("expected the attribute memory (mem_test_dana_role) to be resolved")
	}
	if !sawFact {
		t.Error("expected the key-fact memory (mem_test_fact_1) to be resolved")
	}
}
