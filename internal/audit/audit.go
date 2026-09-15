// Package audit writes to the single audit_log table
// (schema/0007_audit_log.sql) every other package logs into —
// docs/GAP_CLOSURE_PLAN.md §4.3. One writer, one shape, so a report tool
// never has to reconcile timestamps across separately-logged event
// categories.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

const (
	EventCapture        = "capture"
	EventCorrect        = "correct"
	EventRetrieve       = "retrieve"
	EventTrace          = "trace"
	EventAdminProvision = "admin_provision"
	EventAdminUIView    = "admin_ui_view"
	// EventExport/EventImport were added in schema/0009 — a scope's
	// entire history leaving (or entering) the live store is at least as
	// significant as a single retrieval, see docs/GAP_CLOSURE_PLAN.md §4.2.
	EventExport = "export"
	EventImport = "import"
	// EventKeyRotation was added in schema/0011 — see internal/rotate.
	EventKeyRotation = "key_rotation"
)

// Entry is one audit_log row. ActingScope/WorkspaceScope follow the same
// two-scope convention dbscope.Run does — for a single-scope event, set
// both to the same Scope.
type Entry struct {
	EventType      string
	Actor          string // identity.Identity.UserID, or an operator name (see cmd/hupi-admin's -actor flag / operators.name)
	ActingScope    identity.Scope
	WorkspaceScope identity.Scope
	TargetRef      *identity.Ref  // nil if this event has no single memory-record target
	Detail         map[string]any // nil is fine — marshals to "{}"
}

// Write inserts one row using q, inside whatever transaction the caller
// already has open — its session variables (dbscope.SetSession) must
// already match e.ActingScope/e.WorkspaceScope, since the insert's own
// RLS check (schema/0007) enforces exactly that. Use this from any call
// site that's already inside a dbscope.Run block (internal/store,
// internal/consolidation) so the audit row commits atomically with the
// write it's describing, rather than as a separate round trip that could
// succeed or fail independently.
func Write(ctx context.Context, q dbscope.Querier, e Entry) error {
	detail := e.Detail
	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("audit: marshal detail: %w", err)
	}
	var targetRefJSON []byte
	if e.TargetRef != nil {
		targetRefJSON, err = json.Marshal(e.TargetRef)
		if err != nil {
			return fmt.Errorf("audit: marshal target_ref: %w", err)
		}
	}

	_, err = q.ExecContext(ctx, `
		insert into audit_log (
			event_type, actor,
			acting_scope_kind, acting_scope_owner,
			workspace_scope_kind, workspace_scope_owner,
			target_ref, detail
		) values ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		e.EventType, e.Actor,
		e.ActingScope.Kind, e.ActingScope.Owner,
		e.WorkspaceScope.Kind, e.WorkspaceScope.Owner,
		nullableJSON(targetRefJSON), detailJSON,
	)
	if err != nil {
		return fmt.Errorf("audit: insert %s event: %w", e.EventType, err)
	}
	return nil
}

// LogStandalone opens its own scoped transaction and writes e — for
// callers with no transaction of their own to piggyback on (hupi-admin's
// provisioning commands, which write users/teams/api_keys via plain
// *sql.DB calls, not dbscope, since those tables carry no RLS policy).
func LogStandalone(ctx context.Context, db *sql.DB, e Entry) error {
	return dbscope.Run(ctx, db, e.ActingScope, e.WorkspaceScope, func(tx *sql.Tx) error {
		return Write(ctx, tx, e)
	})
}

func nullableJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
