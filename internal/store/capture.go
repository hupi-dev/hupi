package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/gateway"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// Capture inserts one row into `episodes` (schema/0001_init.sql +
// schema/0002_tier3_phase1_identity.sql) — either a chat turn (type
// "interaction") or a feedback record (type "feedback", see
// gateway.HandleFeedback). Called and awaited by the handler before a
// turn is considered done (ARCHITECTURE.md § Capture), so this stays a
// single fast local write: no LLM calls, no network beyond the one
// Postgres round trip.
func (s *Store) Capture(ctx context.Context, scope identity.Scope, ep gateway.Episode) error {
	enc, keyVersion, err := s.keys.GetOrCreate(ctx, scope)
	if err != nil {
		return fmt.Errorf("store: resolve encryption key for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	inputCT, err := enc.Encrypt(ep.InputText)
	if err != nil {
		return fmt.Errorf("store: encrypt input_text: %w", err)
	}
	outputCT, err := enc.Encrypt(ep.OutputText)
	if err != nil {
		return fmt.Errorf("store: encrypt output_text: %w", err)
	}

	// note is only meaningful on feedback rows — leaving it as a real
	// NULL rather than an encrypted "" for every chat turn avoids writing
	// a ciphertext blob that will never be read back.
	var noteCT []byte
	if ep.Type == "feedback" {
		noteCT, err = enc.Encrypt(ep.Note)
		if err != nil {
			return fmt.Errorf("store: encrypt note: %w", err)
		}
	}

	refsJSON, err := json.Marshal(refsOrEmpty(ep.RetrievedRefs))
	if err != nil {
		return fmt.Errorf("store: marshal retrieved_refs: %w", err)
	}

	actor := ep.ActorUserID
	if actor == "" {
		actor = scope.Owner
	}

	err = dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into episodes (
				id, ts, type, provider_vendor, provider_model,
				input_text, output_text, importance, hash, truncated,
				memory_gate, retrieved_refs,
				refers_to, rating, note,
				scope_kind, scope_owner, key_version
			) values (
				$1, $2, $3, $4, $5,
				$6, $7, $8, $9, $10,
				$11, $12,
				$13, $14, $15,
				$16, $17, $18
			)
			on conflict (id) do nothing
		`,
			ep.ID, ep.TS, ep.Type, pgfmt.Nullable(ep.ProviderVendor), pgfmt.Nullable(ep.ProviderModel),
			inputCT, outputCT, ep.Importance, ep.Hash, ep.Truncated,
			pgfmt.Nullable(string(ep.MemoryGate)), refsJSON,
			pgfmt.Nullable(ep.RefersTo), pgfmt.Nullable(string(ep.Rating)), noteCT,
			scope.Kind, scope.Owner, keyVersion,
		)
		if err != nil {
			return err
		}
		// Logged in the same transaction as the episode insert (not a
		// separate round trip) so the two can never disagree about
		// whether the write happened — docs/GAP_CLOSURE_PLAN.md §4.3.
		// ActingScope/WorkspaceScope must be `scope` here, not actor's
		// own private scope, to satisfy audit_log's RLS insert check
		// (schema/0007) against this transaction's actual session
		// variables — `actor` (a plain column, not RLS-checked) is still
		// where "which team member" gets recorded.
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventCapture,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			TargetRef:      &identity.Ref{Kind: identity.RefKindEpisode, Scope: scope, ID: ep.ID},
			Detail:         map[string]any{"episode_type": ep.Type},
		})
	})
	if err != nil {
		return fmt.Errorf("store: insert episode %s: %w", ep.ID, err)
	}
	return nil
}

// refsOrEmpty ensures we marshal `[]` rather than SQL/JSON null for a nil
// slice — retrieved_refs is `not null default '[]'` in the schema, and a
// json null would violate that column's intent even though it wouldn't
// violate NOT NULL at the SQL level (a jsonb column holding the literal
// value `null` is still non-NULL to Postgres).
func refsOrEmpty(refs []identity.Ref) []identity.Ref {
	if refs == nil {
		return []identity.Ref{}
	}
	return refs
}
