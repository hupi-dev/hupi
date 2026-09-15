// Package rotate implements online, resumable per-scope key rotation —
// docs/GAP_CLOSURE_PLAN.md §4.4. Generating a new DEK is cheap
// (internal/crypto.KeyStore.CreateNextVersion); the hard part this
// package owns is migrating every already-encrypted row from the old
// version to the new one without downtime and without losing progress if
// the process doing it gets killed partway through.
//
// The core safety property: once Start creates a scope's new (to_)
// version, every *new* write immediately uses it (KeyStore.GetOrCreate
// always resolves the current/highest version) — this package only ever
// has to migrate rows that predate the rotation, never chase a moving
// target. A row this package has already migrated no longer matches the
// "still on the old version" query any future batch issues, which is
// what makes resuming after a crash safe without needing to trust a
// precisely-tracked cursor for *correctness* (cursor_table/cursor_id are
// still recorded, in schema/0010's key_rotations table, purely so
// `hupi-rotate-key -status` has something to report — not because a
// resumed run depends on them to avoid reprocessing a row).
package rotate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusFailed     Status = "failed"
)

// tableOrder is the fixed sequence a rotation walks — episodes first
// since they're normally the largest table, then summaries (which also
// carries summary_key_facts along for the ride, see migrateSummaryBatch),
// then entities.
var tableOrder = []string{"episodes", "summaries", "entities"}

func nextTable(current string) string {
	for i, t := range tableOrder {
		if t == current {
			if i+1 < len(tableOrder) {
				return tableOrder[i+1]
			}
			return "done"
		}
	}
	return "done"
}

// RotationStatus is one scope's key_rotations row.
type RotationStatus struct {
	Scope       identity.Scope
	FromVersion int
	ToVersion   int
	Status      Status
	CursorTable string
	CursorID    string
	StartedAt   time.Time
	CompletedAt *time.Time
}

type Runner struct {
	db   *sql.DB
	keys *crypto.KeyStore
}

func New(db *sql.DB, keys *crypto.KeyStore) *Runner {
	return &Runner{db: db, keys: keys}
}

// Start begins rotating scope to a new key version, or — if a rotation is
// already in_progress — returns that one's from/to versions unchanged,
// so calling Start repeatedly (e.g. every cron tick until a batch job
// finishes) is always safe. Refuses to start a second rotation on top of
// a completed one's leftover row without this being a fresh call: a
// completed or failed row is overwritten with a new from/to pair and
// reset to in_progress, since key_rotations is live progress, not
// history (schema/0010's comment).
func (r *Runner) Start(ctx context.Context, scope identity.Scope, actor string) (fromVersion, toVersion int, err error) {
	existing, ok, err := r.Status(ctx, scope)
	if err != nil {
		return 0, 0, err
	}
	if ok && existing.Status == StatusInProgress {
		return existing.FromVersion, existing.ToVersion, nil
	}

	fromVersion, err = r.keys.CurrentVersion(ctx, scope)
	if err != nil {
		return 0, 0, err
	}
	if fromVersion == 0 {
		return 0, 0, fmt.Errorf("rotate: %s:%s has no key yet — nothing to rotate", scope.Kind, scope.Owner)
	}
	toVersion, err = r.keys.CreateNextVersion(ctx, scope)
	if err != nil {
		return 0, 0, err
	}

	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			insert into key_rotations (scope_kind, scope_owner, from_version, to_version, status, cursor_table, cursor_id, started_at, completed_at)
			values ($1, $2, $3, $4, 'in_progress', 'episodes', '', now(), null)
			on conflict (scope_kind, scope_owner) do update set
				from_version = excluded.from_version,
				to_version   = excluded.to_version,
				status       = excluded.status,
				cursor_table = excluded.cursor_table,
				cursor_id    = excluded.cursor_id,
				started_at   = excluded.started_at,
				completed_at = excluded.completed_at
		`, scope.Kind, scope.Owner, fromVersion, toVersion)
		if err != nil {
			return fmt.Errorf("insert key_rotations row: %w", err)
		}
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventKeyRotation,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail:         map[string]any{"action": "start", "from_version": fromVersion, "to_version": toVersion},
		})
	})
	if err != nil {
		return 0, 0, fmt.Errorf("rotate: start rotation for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	return fromVersion, toVersion, nil
}

// Status returns scope's current key_rotations row, and false if it has
// never had one (never rotated).
func (r *Runner) Status(ctx context.Context, scope identity.Scope) (RotationStatus, bool, error) {
	var s RotationStatus
	var status string
	var completedAt sql.NullTime
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select from_version, to_version, status, cursor_table, cursor_id, started_at, completed_at
			from key_rotations where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&s.FromVersion, &s.ToVersion, &status, &s.CursorTable, &s.CursorID, &s.StartedAt, &completedAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return RotationStatus{}, false, nil
	}
	if err != nil {
		return RotationStatus{}, false, fmt.Errorf("rotate: load status for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	s.Scope = scope
	s.Status = Status(status)
	if completedAt.Valid {
		s.CompletedAt = &completedAt.Time
	}
	return s, true, nil
}

// Continue processes up to batchSize more rows of scope's in_progress
// rotation and returns how many it migrated. done is true once every
// table has nothing left on the old version — Continue itself marks the
// rotation completed at that point, so the caller doesn't need a separate
// "finish" call. Safe to call repeatedly in a loop until done; safe to
// call again after a crash, resuming from wherever the database actually
// is (see package doc comment for why that doesn't depend on the
// persisted cursor for correctness).
func (r *Runner) Continue(ctx context.Context, scope identity.Scope, batchSize int, actor string) (processed int, done bool, err error) {
	st, ok, err := r.Status(ctx, scope)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		return 0, false, fmt.Errorf("rotate: no rotation for %s:%s — call Start first", scope.Kind, scope.Owner)
	}
	if st.Status != StatusInProgress {
		return 0, true, nil
	}

	fromEnc, err := r.keys.GetVersion(ctx, scope, st.FromVersion)
	if err != nil {
		return 0, false, err
	}
	toEnc, err := r.keys.GetVersion(ctx, scope, st.ToVersion)
	if err != nil {
		return 0, false, err
	}

	table := st.CursorTable
	for table != "done" {
		var n int
		switch table {
		case "episodes":
			n, err = r.migrateEpisodeBatch(ctx, scope, fromEnc, toEnc, st.FromVersion, st.ToVersion, batchSize)
		case "summaries":
			n, err = r.migrateSummaryBatch(ctx, scope, fromEnc, toEnc, st.FromVersion, st.ToVersion, batchSize)
		case "entities":
			n, err = r.migrateEntityBatch(ctx, scope, fromEnc, toEnc, st.FromVersion, st.ToVersion, batchSize)
		default:
			return 0, false, fmt.Errorf("rotate: unknown cursor_table %q for %s:%s", table, scope.Kind, scope.Owner)
		}
		if err != nil {
			return 0, false, fmt.Errorf("rotate: migrate %s for %s:%s: %w", table, scope.Kind, scope.Owner, err)
		}
		if n > 0 {
			return n, false, nil
		}
		table = nextTable(table)
		if err := r.setCursorTable(ctx, scope, table); err != nil {
			return 0, false, err
		}
	}

	if err := r.markCompleted(ctx, scope, actor); err != nil {
		return 0, false, err
	}
	return 0, true, nil
}

func (r *Runner) setCursorTable(ctx context.Context, scope identity.Scope, table string) error {
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			update key_rotations set cursor_table = $1, cursor_id = '' where scope_kind = $2 and scope_owner = $3
		`, table, scope.Kind, scope.Owner)
		return err
	})
}

func (r *Runner) markCompleted(ctx context.Context, scope identity.Scope, actor string) error {
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			update key_rotations set status = 'completed', completed_at = now() where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("mark completed: %w", err)
		}
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventKeyRotation,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail:         map[string]any{"action": "completed"},
		})
	})
}

func (r *Runner) migrateEpisodeBatch(ctx context.Context, scope identity.Scope, fromEnc, toEnc *crypto.Encryptor, fromVersion, toVersion, batchSize int) (int, error) {
	type row struct {
		id                        string
		inputCT, outputCT, noteCT []byte
	}
	var rows []row

	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, input_text, output_text, note from episodes
			where scope_kind = $1 and scope_owner = $2 and key_version = $3
			order by id limit $4
		`, scope.Kind, scope.Owner, fromVersion, batchSize)
		if err != nil {
			return fmt.Errorf("query episodes: %w", err)
		}
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.inputCT, &rr.outputCT, &rr.noteCT); err != nil {
				dbRows.Close()
				return fmt.Errorf("scan episode: %w", err)
			}
			rows = append(rows, rr)
		}
		if err := dbRows.Err(); err != nil {
			dbRows.Close()
			return err
		}
		dbRows.Close()

		for _, rr := range rows {
			input, err := fromEnc.Decrypt(rr.inputCT)
			if err != nil {
				return fmt.Errorf("decrypt episode %s input_text: %w", rr.id, err)
			}
			newInputCT, err := toEnc.Encrypt(input)
			if err != nil {
				return fmt.Errorf("re-encrypt episode %s input_text: %w", rr.id, err)
			}
			output, err := fromEnc.Decrypt(rr.outputCT)
			if err != nil {
				return fmt.Errorf("decrypt episode %s output_text: %w", rr.id, err)
			}
			newOutputCT, err := toEnc.Encrypt(output)
			if err != nil {
				return fmt.Errorf("re-encrypt episode %s output_text: %w", rr.id, err)
			}
			// note is NULL for every non-feedback episode (internal/store's
			// Capture only ever encrypts it for type='feedback') — leaving a
			// NULL column NULL, rather than re-encrypting "" into it, avoids
			// turning a genuinely-empty column into a wasted ciphertext blob.
			var newNoteCT any
			if rr.noteCT != nil {
				note, err := fromEnc.Decrypt(rr.noteCT)
				if err != nil {
					return fmt.Errorf("decrypt episode %s note: %w", rr.id, err)
				}
				newNoteCT, err = toEnc.Encrypt(note)
				if err != nil {
					return fmt.Errorf("re-encrypt episode %s note: %w", rr.id, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `
				update episodes set input_text = $1, output_text = $2, note = $3, key_version = $4
				where id = $5 and scope_kind = $6 and scope_owner = $7
			`, newInputCT, newOutputCT, newNoteCT, toVersion, rr.id, scope.Kind, scope.Owner); err != nil {
				return fmt.Errorf("update episode %s: %w", rr.id, err)
			}
		}
		if len(rows) > 0 {
			if _, err := tx.ExecContext(ctx, `
				update key_rotations set cursor_id = $1 where scope_kind = $2 and scope_owner = $3
			`, rows[len(rows)-1].id, scope.Kind, scope.Owner); err != nil {
				return fmt.Errorf("advance cursor: %w", err)
			}
		}
		return nil
	})
	return len(rows), err
}

func (r *Runner) migrateSummaryBatch(ctx context.Context, scope identity.Scope, fromEnc, toEnc *crypto.Encryptor, fromVersion, toVersion, batchSize int) (int, error) {
	type row struct {
		id        string
		summaryCT []byte
	}
	var rows []row

	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, summary from summaries
			where scope_kind = $1 and scope_owner = $2 and key_version = $3
			order by id limit $4
		`, scope.Kind, scope.Owner, fromVersion, batchSize)
		if err != nil {
			return fmt.Errorf("query summaries: %w", err)
		}
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.summaryCT); err != nil {
				dbRows.Close()
				return fmt.Errorf("scan summary: %w", err)
			}
			rows = append(rows, rr)
		}
		if err := dbRows.Err(); err != nil {
			dbRows.Close()
			return err
		}
		dbRows.Close()

		for _, rr := range rows {
			text, err := fromEnc.Decrypt(rr.summaryCT)
			if err != nil {
				return fmt.Errorf("decrypt summary %s: %w", rr.id, err)
			}
			newCT, err := toEnc.Encrypt(text)
			if err != nil {
				return fmt.Errorf("re-encrypt summary %s: %w", rr.id, err)
			}
			if _, err := tx.ExecContext(ctx, `
				update summaries set summary = $1, key_version = $2 where id = $3
			`, newCT, toVersion, rr.id); err != nil {
				return fmt.Errorf("update summary %s: %w", rr.id, err)
			}

			// Key facts always move in lockstep with their parent summary
			// (they're written in the same transaction, under the same
			// key, in internal/consolidation's storeSummary) — migrating
			// them here, keyed by summary_id rather than their own cursor,
			// is what keeps that invariant true through a rotation too.
			if err := r.migrateKeyFacts(ctx, tx, fromEnc, toEnc, rr.id, fromVersion, toVersion); err != nil {
				return fmt.Errorf("migrate key facts for summary %s: %w", rr.id, err)
			}
		}
		if len(rows) > 0 {
			if _, err := tx.ExecContext(ctx, `
				update key_rotations set cursor_id = $1 where scope_kind = $2 and scope_owner = $3
			`, rows[len(rows)-1].id, scope.Kind, scope.Owner); err != nil {
				return fmt.Errorf("advance cursor: %w", err)
			}
		}
		return nil
	})
	return len(rows), err
}

func (r *Runner) migrateKeyFacts(ctx context.Context, tx *sql.Tx, fromEnc, toEnc *crypto.Encryptor, summaryID string, fromVersion, toVersion int) error {
	type row struct {
		id     int64
		factCT []byte
	}
	var rows []row

	dbRows, err := tx.QueryContext(ctx, `
		select id, fact from summary_key_facts where summary_id = $1 and key_version = $2
	`, summaryID, fromVersion)
	if err != nil {
		return fmt.Errorf("query key facts: %w", err)
	}
	for dbRows.Next() {
		var rr row
		if err := dbRows.Scan(&rr.id, &rr.factCT); err != nil {
			dbRows.Close()
			return fmt.Errorf("scan key fact: %w", err)
		}
		rows = append(rows, rr)
	}
	if err := dbRows.Err(); err != nil {
		dbRows.Close()
		return err
	}
	dbRows.Close()

	for _, rr := range rows {
		fact, err := fromEnc.Decrypt(rr.factCT)
		if err != nil {
			return fmt.Errorf("decrypt key fact %d: %w", rr.id, err)
		}
		newCT, err := toEnc.Encrypt(fact)
		if err != nil {
			return fmt.Errorf("re-encrypt key fact %d: %w", rr.id, err)
		}
		if _, err := tx.ExecContext(ctx, `
			update summary_key_facts set fact = $1, key_version = $2 where id = $3
		`, newCT, toVersion, rr.id); err != nil {
			return fmt.Errorf("update key fact %d: %w", rr.id, err)
		}
	}
	return nil
}

func (r *Runner) migrateEntityBatch(ctx context.Context, scope identity.Scope, fromEnc, toEnc *crypto.Encryptor, fromVersion, toVersion, batchSize int) (int, error) {
	type row struct {
		id      string
		attrsCT []byte
	}
	var rows []row

	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		dbRows, err := tx.QueryContext(ctx, `
			select id, attributes from entities
			where scope_kind = $1 and scope_owner = $2 and key_version = $3
			order by id limit $4
		`, scope.Kind, scope.Owner, fromVersion, batchSize)
		if err != nil {
			return fmt.Errorf("query entities: %w", err)
		}
		for dbRows.Next() {
			var rr row
			if err := dbRows.Scan(&rr.id, &rr.attrsCT); err != nil {
				dbRows.Close()
				return fmt.Errorf("scan entity: %w", err)
			}
			rows = append(rows, rr)
		}
		if err := dbRows.Err(); err != nil {
			dbRows.Close()
			return err
		}
		dbRows.Close()

		for _, rr := range rows {
			attrs, err := fromEnc.Decrypt(rr.attrsCT)
			if err != nil {
				return fmt.Errorf("decrypt entity %s: %w", rr.id, err)
			}
			newCT, err := toEnc.Encrypt(attrs)
			if err != nil {
				return fmt.Errorf("re-encrypt entity %s: %w", rr.id, err)
			}
			if _, err := tx.ExecContext(ctx, `
				update entities set attributes = $1, key_version = $2
				where id = $3 and scope_kind = $4 and scope_owner = $5
			`, newCT, toVersion, rr.id, scope.Kind, scope.Owner); err != nil {
				return fmt.Errorf("update entity %s: %w", rr.id, err)
			}
		}
		if len(rows) > 0 {
			if _, err := tx.ExecContext(ctx, `
				update key_rotations set cursor_id = $1 where scope_kind = $2 and scope_owner = $3
			`, rows[len(rows)-1].id, scope.Kind, scope.Owner); err != nil {
				return fmt.Errorf("advance cursor: %w", err)
			}
		}
		return nil
	})
	return len(rows), err
}

// Prune deletes scope_keys rows for versions older than a completed
// rotation's to_version — the explicit, separate step
// docs/GAP_CLOSURE_PLAN.md §4.4 calls for rather than doing this
// automatically: keeping old key material costs nothing and removes any
// risk of a missed row becoming permanently unreadable, so this is opt-in.
// Refuses on anything but a completed rotation, and double-checks no row
// anywhere in the scope still references a version it's about to delete
// — trusting the status flag alone isn't enough for something this
// destructive and irreversible.
//
// This deletes the persisted wrapped DEK; it does not (cannot) scrub
// every process's memory. This Runner's own KeyStore has the pruned
// version evicted from its cache below, but a separately-running process
// that already resolved the same version — the live gateway, most
// realistically — keeps its in-memory copy until it restarts. If the
// rotation was prompted by a suspected compromise, restart every process
// sharing this KeyStore's database after pruning, not just this one.
func (r *Runner) Prune(ctx context.Context, scope identity.Scope, actor string) (int, error) {
	st, ok, err := r.Status(ctx, scope)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("rotate: no rotation record for %s:%s", scope.Kind, scope.Owner)
	}
	if st.Status != StatusCompleted {
		return 0, fmt.Errorf("rotate: rotation for %s:%s is %s, not completed — refusing to prune while old data might still need it", scope.Kind, scope.Owner, st.Status)
	}

	var stillReferenced int
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select
				(select count(*) from episodes where scope_kind=$1 and scope_owner=$2 and key_version < $3) +
				(select count(*) from summaries where scope_kind=$1 and scope_owner=$2 and key_version < $3) +
				(select count(*) from entities where scope_kind=$1 and scope_owner=$2 and key_version < $3) +
				(select count(*) from summary_key_facts f join summaries s on s.id = f.summary_id
					where s.scope_kind=$1 and s.scope_owner=$2 and f.key_version < $3)
		`, scope.Kind, scope.Owner, st.ToVersion).Scan(&stillReferenced)
	})
	if err != nil {
		return 0, fmt.Errorf("rotate: verify no row still needs an old key for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if stillReferenced > 0 {
		return 0, fmt.Errorf("rotate: %d row(s) for %s:%s still reference a key version older than %d — refusing to prune (the completed status may be stale)", stillReferenced, scope.Kind, scope.Owner, st.ToVersion)
	}

	var pruned int
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			delete from scope_keys where scope_kind = $1 and scope_owner = $2 and version < $3
		`, scope.Kind, scope.Owner, st.ToVersion)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		pruned = int(n)
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventKeyRotation,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail:         map[string]any{"action": "prune", "pruned_versions": pruned, "kept_from_version": st.ToVersion},
		})
	})
	if err != nil {
		return 0, fmt.Errorf("rotate: prune old keys for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	// Evict this process's own cached copies of what was just deleted —
	// found by a test that ran this against real Postgres and noticed
	// GetVersion kept succeeding on a version whose database row was
	// already gone. Only covers this KeyStore instance; see Evict's doc
	// comment for why a separately-running process (a live gateway) isn't
	// affected by this and would need its own restart to fully forget it.
	for v := st.FromVersion; v < st.ToVersion; v++ {
		r.keys.Evict(scope, v)
	}
	return pruned, nil
}
