// Package dbscope wraps a group of related database calls in a
// transaction that carries RLS session variables for the scope(s) that
// group of calls is allowed to touch — see docs/HARDENING_PLAN.md D1-D3.
// Both internal/store and internal/consolidation use this instead of
// issuing independent *sql.DB calls, so that Postgres row-level security
// (added on top in a later migration) has something correct to check.
package dbscope

import (
	"context"
	"database/sql"
	"fmt"

	"hupi/internal/identity"
)

// Querier is satisfied by both *sql.DB and *sql.Tx — query logic written
// against this interface works whether or not it happens to be running
// inside a dbscope-managed transaction, which matters for code paths (like
// migrations, or a future admin tool) that legitimately need to bypass
// scoping.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SetSession sets both scope-variable pairs for the given transaction —
// actingUser and workspace, not just one, because a single logical
// operation (buildAnchor's self_model + workspace search, per
// docs/TIER3_PLAN.md D3) can legitimately need to read from two different
// scopes at once. For a single-scope operation, callers pass the same
// Scope for both — see Run's doc comment.
func SetSession(ctx context.Context, tx *sql.Tx, actingUser, workspace identity.Scope) error {
	_, err := tx.ExecContext(ctx, `
		select
			set_config('hupi.acting_scope_kind', $1, true),
			set_config('hupi.acting_scope_owner', $2, true),
			set_config('hupi.workspace_scope_kind', $3, true),
			set_config('hupi.workspace_scope_owner', $4, true)
	`, actingUser.Kind, actingUser.Owner, workspace.Kind, workspace.Owner)
	if err != nil {
		return fmt.Errorf("dbscope: set session scope: %w", err)
	}
	return nil
}

// Run opens a new transaction, sets its RLS session variables for
// actingUser/workspace, runs fn, and commits — or rolls back if fn (or the
// commit itself) fails. For an operation with only one active scope
// (nearly everything except Store.Retrieve's anchor step), pass the same
// Scope as both actingUser and workspace.
//
// Callers must never hold the returned work open across a network call
// (an LLM or embedding request) — docs/HARDENING_PLAN.md D3. Each fn
// should be pure database work; do external calls before or after Run,
// never inside it.
func Run(ctx context.Context, db *sql.DB, actingUser, workspace identity.Scope, fn func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("dbscope: begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if err := SetSession(ctx, tx, actingUser, workspace); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("dbscope: commit: %w", err)
	}
	return nil
}
