package audit

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// QueryFilter is the set of optional filters cmd/hupi-audit's "query"
// subcommand and hupi-admin-ui's GET /api/audit route both need. Every
// field is optional (zero value means "don't filter on this") except
// Limit, where 0 means "caller must supply a sane default" — Query itself
// applies no default, so a caller that forgets to set one gets an
// unbounded `limit 0` (i.e. no rows) rather than silently pulling the
// whole table; see cmd/hupi-audit and cmd/hupi-admin-ui for the two
// different defaults each picks.
type QueryFilter struct {
	ScopeKind, ScopeOwner, Actor, EventType string
	Since, Until                            *time.Time
	Limit                                   int
	// Ascending selects `order by ts asc` (cmd/hupi-audit's "query"
	// subcommand — oldest match first) vs `order by ts desc` (its "tail"
	// subcommand — most recent N, which the caller then typically reverses
	// for display). Default (false) is descending.
	Ascending bool
}

// LogEntry is one audit_log row, read back out. TargetRef/Detail are left
// as raw JSON text, same as cmd/hupi-audit always printed them — this
// package never decodes or interprets them, since audit_log only ever
// holds provisioning/access metadata, never memory content (episodes,
// summaries, entities); see docs/GAP_CLOSURE_PLAN.md §4.3 and
// docs/ADMIN_UI.md's non-goals.
type LogEntry struct {
	TS                                      time.Time
	EventType, Actor                        string
	ActingScopeKind, ActingScopeOwner       string
	WorkspaceScopeKind, WorkspaceScopeOwner string
	TargetRef, Detail                       string
}

// Query runs a filtered read over audit_log, shared by cmd/hupi-audit
// (tail/query subcommands) and cmd/hupi-admin-ui's GET /api/audit — one
// place building this SQL rather than two copies drifting apart.
func Query(ctx context.Context, db *sql.DB, f QueryFilter) ([]LogEntry, error) {
	var (
		where []string
		vals  []any
	)
	arg := func(v any) string {
		vals = append(vals, v)
		return fmt.Sprintf("$%d", len(vals))
	}
	if f.ScopeKind != "" {
		where = append(where, "workspace_scope_kind = "+arg(f.ScopeKind))
	}
	if f.ScopeOwner != "" {
		where = append(where, "workspace_scope_owner = "+arg(f.ScopeOwner))
	}
	if f.Actor != "" {
		where = append(where, "actor = "+arg(f.Actor))
	}
	if f.EventType != "" {
		where = append(where, "event_type = "+arg(f.EventType))
	}
	if f.Since != nil {
		where = append(where, "ts >= "+arg(*f.Since))
	}
	if f.Until != nil {
		where = append(where, "ts < "+arg(*f.Until))
	}

	order := "desc"
	if f.Ascending {
		order = "asc"
	}

	query := `
		select ts, event_type, actor, acting_scope_kind, acting_scope_owner,
		       workspace_scope_kind, workspace_scope_owner, target_ref, detail
		from audit_log`
	if len(where) > 0 {
		query += " where " + strings.Join(where, " and ")
	}
	query += fmt.Sprintf(" order by ts %s limit %s", order, arg(f.Limit))

	rows, err := db.QueryContext(ctx, query, vals...)
	if err != nil {
		return nil, fmt.Errorf("audit: query audit_log: %w", err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var targetRef sql.NullString
		if err := rows.Scan(
			&e.TS, &e.EventType, &e.Actor,
			&e.ActingScopeKind, &e.ActingScopeOwner,
			&e.WorkspaceScopeKind, &e.WorkspaceScopeOwner,
			&targetRef, &e.Detail,
		); err != nil {
			return nil, fmt.Errorf("audit: scan audit_log row: %w", err)
		}
		e.TargetRef = targetRef.String
		out = append(out, e)
	}
	return out, rows.Err()
}
