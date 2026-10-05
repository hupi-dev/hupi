// Package backfillmemories populates the new `memories` table
// (schema/0025_unified_memories.sql) from the two storage mechanisms it
// replaces — entities.attributes and summary_key_facts — Phase 0 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md.
//
// Deliberately read-only against entities/summary_key_facts: this never
// deletes or modifies the old tables, only inserts into the new one.
// Per the plan doc's own rollback-window open question, the old tables
// stay the source of truth (and the only thing application code reads
// from) until a later, separate cutover change — this package's whole
// job is getting memories into a state where that cutover is safe to
// attempt, not performing the cutover itself.
//
// Idempotent and resumable by construction, the same way
// internal/reembed is (see that package's own doc comment on why it
// keeps no persisted cursor): every memories row this package would ever
// write has a deterministic id derived from its source row, so a
// crash-and-restart (or a deliberate re-run after new entities/facts
// were added) just finds fewer rows "pending" — there's no cursor to
// get out of sync with the data, and no risk of double-inserting a row
// that already exists (every insert is ON CONFLICT DO NOTHING on top of
// the deterministic id, as a second, defensive layer under the
// NOT EXISTS pre-filter each batch query already applies).
package backfillmemories

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

type Runner struct {
	db   *sql.DB
	keys *crypto.KeyStore
}

func New(db *sql.DB, keys *crypto.KeyStore) *Runner {
	return &Runner{db: db, keys: keys}
}

// LogRun writes one audit_log entry per batch — see cmd/hupi-backfill-memories's
// own doc comment on backfillAll for why this is called once per
// non-empty batch, not once at the very end.
func (r *Runner) LogRun(ctx context.Context, scope identity.Scope, actor string, processed int, table string) error {
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventBackfillMemories,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail:         map[string]any{"rows_backfilled": processed, "source_table": table},
		})
	})
}

// ScopeStatus is a live snapshot of how many source rows in scope still
// need a corresponding memories row — always recomputed, never
// persisted, same reasoning as internal/reembed.ScopeStatus.
type ScopeStatus struct {
	AttributeEntitiesPending int // entities with attributes not yet backfilled
	KeyFactsPending          int
}

func (s ScopeStatus) Total() int {
	return s.AttributeEntitiesPending + s.KeyFactsPending
}

// memoryIDForAttribute derives a stable id for the memories row that
// backfills one (entity, attribute key) pair. Hashed rather than a plain
// concatenation of entity_id/key: both are arbitrary, model-produced
// strings (docs/CONSOLIDATION_ARCHITECTURE_REVIEW_PLAN.md's own history
// of models inventing unexpected key shapes is exactly why nothing in
// this codebase trusts an LLM-produced string to be safe for direct use
// as a prefix/delimiter scheme) — hashing sidesteps needing to reason
// about every character either could contain.
func memoryIDForAttribute(scope identity.Scope, entityID, key string) string {
	h := sha256.Sum256([]byte(scope.Kind + "|" + scope.Owner + "|" + entityID + "|" + key))
	return "mem_attr_" + hex.EncodeToString(h[:])[:24]
}

// memoryIDForKeyFact derives a stable id for the memories row that
// backfills one summary_key_facts row. Simpler than the attribute case:
// summary_key_facts.id is already a globally unique bigserial, so no
// hashing is needed to make this collision-free.
func memoryIDForKeyFact(factID int64) string {
	return "mem_fact_" + strconv.FormatInt(factID, 10)
}

// Status counts, per source table, how many rows in scope don't yet have
// a corresponding memories row — see the package doc comment for why
// this is a live count, not a persisted cursor.
func (r *Runner) Status(ctx context.Context, scope identity.Scope) (ScopeStatus, error) {
	var st ScopeStatus
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from entities e
			where e.scope_kind = $1 and e.scope_owner = $2
			  and e.attributes is not null
			  and not exists (
			    select 1 from memories m
			    where m.scope_kind = e.scope_kind and m.scope_owner = e.scope_owner
			      and m.entity_id = e.id and m.is_static = true
			  )
		`, scope.Kind, scope.Owner).Scan(&st.AttributeEntitiesPending); err != nil {
			return fmt.Errorf("count pending attribute entities: %w", err)
		}

		if err := tx.QueryRowContext(ctx, `
			select count(*) from summary_key_facts f
			where f.scope_kind = $1 and f.scope_owner = $2
			  and not exists (select 1 from memories m where m.id = 'mem_fact_' || f.id::text)
		`, scope.Kind, scope.Owner).Scan(&st.KeyFactsPending); err != nil {
			return fmt.Errorf("count pending key facts: %w", err)
		}
		return nil
	})
	return st, err
}

// Continue backfills up to batchSize source rows from whichever table
// still has pending work — entities/attributes first, then
// summary_key_facts, mirroring internal/reembed.Continue's own
// one-table-at-a-time-per-call shape (see that function's doc comment
// for why: simpler per-call error handling, and progress reporting that
// can say which table a batch came from). done is true once both tables
// report zero pending.
func (r *Runner) Continue(ctx context.Context, scope identity.Scope, batchSize int) (processed int, table string, done bool, err error) {
	n, err := r.backfillAttributeBatch(ctx, scope, batchSize)
	if err != nil {
		return 0, "entities", false, err
	}
	if n > 0 {
		return n, "entities", false, nil
	}

	n, err = r.backfillKeyFactBatch(ctx, scope, batchSize)
	if err != nil {
		return 0, "summary_key_facts", false, err
	}
	if n > 0 {
		return n, "summary_key_facts", false, nil
	}

	return 0, "", true, nil
}

func (r *Runner) backfillAttributeBatch(ctx context.Context, scope identity.Scope, batchSize int) (int, error) {
	type entityRow struct {
		id         string
		attrsCT    []byte
		keyVersion int
	}
	var rows []entityRow
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select e.id, e.attributes, e.key_version from entities e
			where e.scope_kind = $1 and e.scope_owner = $2
			  and e.attributes is not null
			  and not exists (
			    select 1 from memories m
			    where m.scope_kind = e.scope_kind and m.scope_owner = e.scope_owner
			      and m.entity_id = e.id and m.is_static = true
			  )
			order by e.id limit $3
		`, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query entities: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr entityRow
			if err := dbRows.Scan(&rr.id, &rr.attrsCT, &rr.keyVersion); err != nil {
				return fmt.Errorf("scan entity: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	for _, rr := range rows {
		readEnc, err := r.keys.GetVersion(ctx, scope, rr.keyVersion)
		if err != nil {
			return 0, fmt.Errorf("get key version %d for entity %s: %w", rr.keyVersion, rr.id, err)
		}
		attrsJSON, err := readEnc.Decrypt(rr.attrsCT)
		if err != nil {
			return 0, fmt.Errorf("decrypt entity %s attributes: %w", rr.id, err)
		}
		var attrs map[string]string
		if attrsJSON != "" {
			if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
				return 0, fmt.Errorf("parse entity %s attributes: %w", rr.id, err)
			}
		}

		// Writes always go under the scope's *current* key version, same
		// convention every other write path in this codebase follows
		// (GetOrCreate's own doc comment) — the attribute values being
		// backfilled may have been encrypted under an older version
		// originally (rr.keyVersion, used only to decrypt them above),
		// but the new per-key ciphertext this produces is new content,
		// not a copy of the old blob, so it gets the current version.
		writeEnc, writeVersion, err := r.keys.GetOrCreate(ctx, scope)
		if err != nil {
			return 0, fmt.Errorf("get current key for entity %s: %w", rr.id, err)
		}

		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			for key, value := range attrs {
				ct, err := writeEnc.Encrypt(value)
				if err != nil {
					return fmt.Errorf("encrypt attribute %s.%s: %w", rr.id, key, err)
				}
				memID := memoryIDForAttribute(scope, rr.id, key)
				_, err = tx.ExecContext(ctx, `
					insert into memories (id, scope_kind, scope_owner, entity_id, content, key_version, is_static, grounded)
					values ($1, $2, $3, $4, $5, $6, true, true)
					on conflict (id) do nothing
				`, memID, scope.Kind, scope.Owner, rr.id, ct, writeVersion)
				if err != nil {
					return fmt.Errorf("insert memory for %s.%s: %w", rr.id, key, err)
				}
			}
			// An entity with an attributes blob that decrypts to an empty
			// map (attrsJSON == "" or "{}") would otherwise never satisfy
			// the NOT EXISTS pre-filter above — nothing gets inserted for
			// it, so it would show up as "pending" forever on every
			// subsequent Status/Continue call. A placeholder-free, static
			// marker row with no real attribute content keeps that entity
			// correctly counted as "done" without needing a separate
			// tracking mechanism just for this edge case.
			if len(attrs) == 0 {
				emptyCT, err := writeEnc.Encrypt("")
				if err != nil {
					return fmt.Errorf("encrypt empty-attributes marker for %s: %w", rr.id, err)
				}
				_, err = tx.ExecContext(ctx, `
					insert into memories (id, scope_kind, scope_owner, entity_id, content, key_version, is_static, grounded)
					values ($1, $2, $3, $4, $5, $6, true, false)
					on conflict (id) do nothing
				`, memoryIDForAttribute(scope, rr.id, "__empty__"), scope.Kind, scope.Owner, rr.id, emptyCT, writeVersion)
				if err != nil {
					return fmt.Errorf("insert empty-attributes marker for %s: %w", rr.id, err)
				}
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}

func (r *Runner) backfillKeyFactBatch(ctx context.Context, scope identity.Scope, batchSize int) (int, error) {
	type factRow struct {
		id                 int64
		summaryID          string
		factCT             []byte
		keyVersion         int
		grounded           bool
		sourceEpisodeIDLit string
	}
	var rows []factRow
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select f.id, f.summary_id, f.fact, f.key_version, f.grounded, f.source_episode_ids::text
			from summary_key_facts f
			where f.scope_kind = $1 and f.scope_owner = $2
			  and not exists (select 1 from memories m where m.id = 'mem_fact_' || f.id::text)
			order by f.id limit $3
		`, scope.Kind, scope.Owner, batchSize)
		if err != nil {
			return fmt.Errorf("query key facts: %w", err)
		}
		defer dbRows.Close()
		for dbRows.Next() {
			var rr factRow
			if err := dbRows.Scan(&rr.id, &rr.summaryID, &rr.factCT, &rr.keyVersion, &rr.grounded, &rr.sourceEpisodeIDLit); err != nil {
				return fmt.Errorf("scan key fact: %w", err)
			}
			rows = append(rows, rr)
		}
		return dbRows.Err()
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}

	return len(rows), dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		for _, rr := range rows {
			// The fact's own ciphertext is copied verbatim, under its
			// original key_version — unlike the attribute case above,
			// this is the exact same content moving to a new column, not
			// new plaintext being encrypted, so there's no reason to
			// decrypt it at all (and real reason not to: fewer places
			// real fact text ever exists in process memory during a bulk
			// migration is strictly better).
			sourceIDs := pgfmt.ParseTextArray(rr.sourceEpisodeIDLit)
			_, err := tx.ExecContext(ctx, `
				insert into memories (id, scope_kind, scope_owner, summary_id, content, key_version, is_static, grounded, source_episode_ids)
				values ($1, $2, $3, $4, $5, $6, false, $7, $8::text[])
				on conflict (id) do nothing
			`, memoryIDForKeyFact(rr.id), scope.Kind, scope.Owner, rr.summaryID, rr.factCT, rr.keyVersion, rr.grounded, pgfmt.TextArray(sourceIDs))
			if err != nil {
				return fmt.Errorf("insert memory for key fact %d: %w", rr.id, err)
			}
		}
		return nil
	})
}
