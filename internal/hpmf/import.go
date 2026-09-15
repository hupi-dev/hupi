package hpmf

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// ImportStats reports what ImportScope actually did — printed by
// cmd/hupi-import so a merge import's "nothing looked new" isn't silent.
type ImportStats struct {
	EpisodesImported, EpisodesSkipped   int
	SummariesImported, SummariesSkipped int
	EntitiesImported, EntitiesSkipped   int
}

// ImportScope loads one scope's exported directory (dir — an absolute
// path, already resolved by the caller per the bundle's manifest.json)
// into targetScope.
//
// If merge is false, targetScope must be empty (no existing episodes/
// summaries/entities) — ImportScope fails loudly rather than silently
// overwriting or interleaving with unrelated existing data.
//
// If merge is true: episodes are deduped by their `hash` column;
// entities are skipped if that id already exists in targetScope (no
// attribute merge — that's what consolidation's own upsertEntities does
// going forward, not something a stale snapshot should redo); summaries
// are skipped per (level, period) if targetScope already has any version
// of that period, and otherwise get freshly generated ids scoped to
// targetScope (never the exported ids verbatim — those were generated
// for the *source* scope's owner, see nextImportSummaryID) with
// `supersedes` remapped to match.
func ImportScope(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, dir string, targetScope identity.Scope, merge bool, actor string) (ImportStats, error) {
	var stats ImportStats

	enc, keyVersion, err := keys.GetOrCreate(ctx, targetScope)
	if err != nil {
		return stats, fmt.Errorf("hpmf: resolve encryption key for %s:%s: %w", targetScope.Kind, targetScope.Owner, err)
	}

	if !merge {
		empty, err := scopeIsEmpty(ctx, db, targetScope)
		if err != nil {
			return stats, err
		}
		if !empty {
			return stats, fmt.Errorf("hpmf: %s:%s already has data — pass -merge to import into it anyway", targetScope.Kind, targetScope.Owner)
		}
	}

	if err := importEntities(ctx, db, enc, keyVersion, dir, targetScope, &stats); err != nil {
		return stats, err
	}
	if err := importSummaries(ctx, db, enc, keyVersion, dir, targetScope, &stats); err != nil {
		return stats, err
	}
	if err := importEpisodes(ctx, db, enc, keyVersion, dir, targetScope, &stats); err != nil {
		return stats, err
	}

	auditErr := dbscope.Run(ctx, db, targetScope, targetScope, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventImport,
			Actor:          actor,
			ActingScope:    targetScope,
			WorkspaceScope: targetScope,
			Detail: map[string]any{
				"episodes_imported": stats.EpisodesImported, "episodes_skipped": stats.EpisodesSkipped,
				"summaries_imported": stats.SummariesImported, "summaries_skipped": stats.SummariesSkipped,
				"entities_imported": stats.EntitiesImported, "entities_skipped": stats.EntitiesSkipped,
			},
		})
	})
	if auditErr != nil {
		return stats, fmt.Errorf("hpmf: audit log write for import into %s:%s: %w", targetScope.Kind, targetScope.Owner, auditErr)
	}
	return stats, nil
}

func scopeIsEmpty(ctx context.Context, db *sql.DB, scope identity.Scope) (bool, error) {
	var count int
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select
				(select count(*) from episodes where scope_kind = $1 and scope_owner = $2) +
				(select count(*) from summaries where scope_kind = $1 and scope_owner = $2) +
				(select count(*) from entities where scope_kind = $1 and scope_owner = $2)
		`, scope.Kind, scope.Owner).Scan(&count)
	})
	if err != nil {
		return false, fmt.Errorf("hpmf: check whether %s:%s is empty: %w", scope.Kind, scope.Owner, err)
	}
	return count == 0, nil
}

func importEntities(ctx context.Context, db *sql.DB, enc *crypto.Encryptor, keyVersion int, dir string, scope identity.Scope, stats *ImportStats) error {
	path := filepath.Join(dir, "entities.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	records, err := readJSONL[EntityRecord](path)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		existing := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `select id from entities where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: load existing entity ids: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			existing[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, r := range records {
			if existing[r.ID] {
				stats.EntitiesSkipped++
				continue
			}
			attrsCT, err := enc.Encrypt(string(r.Attributes))
			if err != nil {
				return fmt.Errorf("hpmf: encrypt entity %s attributes: %w", r.ID, err)
			}
			_, err = tx.ExecContext(ctx, `
				insert into entities (id, kind, name, first_seen, last_updated, attributes, scope_kind, scope_owner, key_version)
				values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			`, r.ID, r.Kind, r.Name, r.FirstSeen, r.LastUpdated, attrsCT, scope.Kind, scope.Owner, keyVersion)
			if err != nil {
				return fmt.Errorf("hpmf: insert entity %s: %w", r.ID, err)
			}
			existing[r.ID] = true
			stats.EntitiesImported++
		}
		return nil
	})
}

// importSummaries walks summaries/<level>/<period>.json in no particular
// cross-file order (each file is independent — periods never reference
// each other), but *within* a file always inserts in array order, since
// a later version's `supersedes` can only point at an earlier one in the
// same file (see ImportScope's doc comment).
func importSummaries(ctx context.Context, db *sql.DB, enc *crypto.Encryptor, keyVersion int, dir string, scope identity.Scope, stats *ImportStats) error {
	files, err := findFiles(filepath.Join(dir, "summaries"), ".json")
	if err != nil {
		return err
	}

	for _, path := range files {
		level := filepath.Base(filepath.Dir(path))
		period := trimExt(filepath.Base(path))

		var versions []SummaryRecord
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("hpmf: read %s: %w", path, err)
		}
		if err := json.Unmarshal(data, &versions); err != nil {
			return fmt.Errorf("hpmf: parse %s: %w", path, err)
		}
		if len(versions) == 0 {
			continue
		}

		err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			exists, err := periodExists(ctx, tx, scope, level, period)
			if err != nil {
				return err
			}
			if exists {
				stats.SummariesSkipped += len(versions)
				return nil
			}

			idMap := map[string]string{} // exported id -> newly generated id, this file only
			for _, r := range versions {
				newID, err := nextImportSummaryID(ctx, tx, scope, level, period)
				if err != nil {
					return err
				}
				supersedes := ""
				if r.Supersedes != "" {
					supersedes = idMap[r.Supersedes] // "" if not found — a dangling reference in the export, treated as none
				}
				summaryCT, err := enc.Encrypt(r.Summary)
				if err != nil {
					return fmt.Errorf("hpmf: encrypt summary %s: %w", newID, err)
				}
				_, err = tx.ExecContext(ctx, `
					insert into summaries (
						id, period, level, status, generated_by_vendor, generated_by_model,
						source_episode_ids, source_summary_periods, summary, entities_touched,
						grounding_checked, supersedes, correction_reason, created_at,
						scope_kind, scope_owner, key_version
					) values ($1,$2,$3,$4,$5,$6,$7::text[],$8::text[],$9,$10::text[],$11,$12,$13,$14,$15,$16,$17)
				`,
					newID, period, level, r.Status, pgfmt.Nullable(r.GeneratedByVendor), pgfmt.Nullable(r.GeneratedByModel),
					pgfmt.TextArray(r.SourceEpisodeIDs), pgfmt.TextArray(r.SourceSummaryPeriods), summaryCT, pgfmt.TextArray(r.EntitiesTouched),
					r.GroundingChecked, pgfmt.Nullable(supersedes), pgfmt.Nullable(r.CorrectionReason), r.CreatedAt,
					scope.Kind, scope.Owner, keyVersion,
				)
				if err != nil {
					return fmt.Errorf("hpmf: insert summary %s (exported as %s): %w", newID, r.ID, err)
				}
				for _, kf := range r.KeyFacts {
					factCT, err := enc.Encrypt(kf.Fact)
					if err != nil {
						return fmt.Errorf("hpmf: encrypt key fact for %s: %w", newID, err)
					}
					_, err = tx.ExecContext(ctx, `
						insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, key_version)
						values ($1, $2, $3::text[], $4, $5)
					`, newID, factCT, pgfmt.TextArray(kf.SourceEpisodeIDs), kf.Grounded, keyVersion)
					if err != nil {
						return fmt.Errorf("hpmf: insert key fact for %s: %w", newID, err)
					}
				}
				idMap[r.ID] = newID
				stats.SummariesImported++
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func periodExists(ctx context.Context, q dbscope.Querier, scope identity.Scope, level, period string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `
		select exists(select 1 from summaries where level = $1 and period = $2 and scope_kind = $3 and scope_owner = $4)
	`, level, period, scope.Kind, scope.Owner).Scan(&exists)
	return exists, err
}

// nextImportSummaryID mirrors internal/consolidation's nextSummaryID
// exactly (same id scheme, same "max version so far" query) — duplicated
// rather than shared because internal/hpmf has no reason to depend on
// internal/consolidation's provider/LLM plumbing for a ten-line query.
func nextImportSummaryID(ctx context.Context, q dbscope.Querier, scope identity.Scope, level, period string) (string, error) {
	var maxVersion int
	err := q.QueryRowContext(ctx, `
		select coalesce(max(cast(substring(id from 'v([0-9]+)$') as int)), 0)
		from summaries where level = $1 and period = $2 and scope_kind = $3 and scope_owner = $4
	`, level, period, scope.Kind, scope.Owner).Scan(&maxVersion)
	if err != nil {
		return "", fmt.Errorf("hpmf: determine next summary version for %s %s: %w", level, period, err)
	}
	return fmt.Sprintf("sum_%s_%s_%s_v%d", scope.Owner, period, level, maxVersion+1), nil
}

func importEpisodes(ctx context.Context, db *sql.DB, enc *crypto.Encryptor, keyVersion int, dir string, scope identity.Scope, stats *ImportStats) error {
	files, err := findFiles(filepath.Join(dir, "episodes"), ".jsonl")
	if err != nil {
		return err
	}
	sort.Strings(files) // YYYY/MM/YYYY-MM-DD.jsonl sorts chronologically as strings

	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		existingHashes := map[string]bool{}
		rows, err := tx.QueryContext(ctx, `select hash from episodes where scope_kind = $1 and scope_owner = $2`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: load existing episode hashes: %w", err)
		}
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				rows.Close()
				return err
			}
			existingHashes[h] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		// episodes.id has no scope qualification (unlike entities) — it's
		// a global primary key, so an id already taken by a *different*
		// scope's row (importing the same bundle into more than one
		// target, or into a scope alongside the original) needs a fresh
		// id, not a silent no-op. idMap records old->actual id so a later
		// feedback episode's refers_to can follow a remapped target.
		idMap := map[string]string{}

		for _, path := range files {
			records, err := readJSONL[EpisodeRecord](path)
			if err != nil {
				return err
			}
			for _, r := range records {
				if existingHashes[r.Hash] {
					stats.EpisodesSkipped++
					continue
				}
				inputCT, err := enc.Encrypt(r.InputText)
				if err != nil {
					return fmt.Errorf("hpmf: encrypt episode %s input_text: %w", r.ID, err)
				}
				outputCT, err := enc.Encrypt(r.OutputText)
				if err != nil {
					return fmt.Errorf("hpmf: encrypt episode %s output_text: %w", r.ID, err)
				}
				noteCT, err := enc.Encrypt(r.Note)
				if err != nil {
					return fmt.Errorf("hpmf: encrypt episode %s note: %w", r.ID, err)
				}
				refsJSON, err := json.Marshal(refsOrEmpty(r.RetrievedRefs))
				if err != nil {
					return fmt.Errorf("hpmf: marshal episode %s retrieved_refs: %w", r.ID, err)
				}
				refersTo := r.RefersTo
				if remapped, ok := idMap[refersTo]; ok {
					refersTo = remapped
				}

				id := r.ID
				for attempt := 0; ; attempt++ {
					res, err := tx.ExecContext(ctx, `
						insert into episodes (
							id, ts, type, provider_vendor, provider_model,
							input_text, output_text, importance, hash, truncated,
							memory_gate, retrieved_refs, refers_to, rating, note,
							scope_kind, scope_owner, key_version
						) values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
						on conflict (id) do nothing
					`,
						id, r.TS, r.Type, pgfmt.Nullable(r.ProviderVendor), pgfmt.Nullable(r.ProviderModel),
						inputCT, outputCT, nullableFloat(r.Importance), r.Hash, r.Truncated,
						pgfmt.Nullable(r.MemoryGate), refsJSON, pgfmt.Nullable(refersTo), pgfmt.Nullable(r.Rating), noteCT,
						scope.Kind, scope.Owner, keyVersion,
					)
					if err != nil {
						return fmt.Errorf("hpmf: insert episode %s: %w", id, err)
					}
					n, err := res.RowsAffected()
					if err != nil {
						return fmt.Errorf("hpmf: insert episode %s: %w", id, err)
					}
					if n > 0 {
						break
					}
					// id already belongs to a different row entirely (not
					// this same episode re-imported — that case was
					// already filtered out by the hash check above): the
					// global id collided, most likely because the source
					// scope's data is also present in this same database
					// under its original owner. Generate a fresh id and
					// retry, remapping so a later refers_to still resolves.
					if attempt > 5 {
						return fmt.Errorf("hpmf: could not find a free id for episode %s after %d attempts", r.ID, attempt)
					}
					id = newImportEpisodeID(r.TS)
				}
				if id != r.ID {
					idMap[r.ID] = id
				}
				existingHashes[r.Hash] = true
				stats.EpisodesImported++
			}
		}
		return nil
	})
}

// newImportEpisodeID mirrors internal/gateway's unexported newEpisodeID
// (same shape: millisecond timestamp prefix + random suffix) — only
// needed on an id collision (see importEpisodes), not on every import.
func newImportEpisodeID(t time.Time) string {
	var suffix [10]byte
	_, _ = rand.Read(suffix[:])
	return fmt.Sprintf("ep_%013d%s", t.UnixMilli(), hex.EncodeToString(suffix[:]))
}

func refsOrEmpty(refs []identity.Ref) []identity.Ref {
	if refs == nil {
		return []identity.Ref{}
	}
	return refs
}

func nullableFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func readJSONL[T any](path string) ([]T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hpmf: read %s: %w", path, err)
	}
	var out []T
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var v T
		if err := dec.Decode(&v); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("hpmf: parse %s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, nil
}
