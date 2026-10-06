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
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// ImportStats reports what ImportScope actually did — printed by
// cmd/hupi-import so a merge import's "nothing looked new" isn't silent.
type ImportStats struct {
	EpisodesImported, EpisodesSkipped   int
	SummariesImported, SummariesSkipped int
	EntitiesImported, EntitiesSkipped   int

	// SummariesWithDanglingSupersedes counts imported summary versions
	// whose `supersedes` pointed at a predecessor not present in the
	// bundle (e.g. a partial/filtered export) — the row is still
	// imported, but as an unlinked version rather than a correction, so
	// this is the only record that lineage was lost (see importSummaries).
	SummariesWithDanglingSupersedes int

	MemoryRelationsImported int
	// MemoryRelationsSkipped counts an edge whose from/to MemoryLocator
	// couldn't be resolved against this import run's own freshly-written
	// data — its summary's period was skipped as already-present (a
	// merge import only ever tracks *this run's* exported-id -> new-id
	// mapping, not every summary that happens to already exist in the
	// target scope from some earlier import or consolidation run), or an
	// attribute's entity/key no longer has a current row. Not an error:
	// same posture as SummariesWithDanglingSupersedes — the edge is
	// genuinely unresolvable this run, not a sign of a corrupt bundle.
	MemoryRelationsSkipped int
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

	// Audits each phase immediately after it commits, not once at the
	// very end — the same real, confirmed bug and fix shape as
	// ExportScope just above (docs/CODEBASE_SURVEY_AND_REVIEW.md finding
	// A10): entities/summaries/episodes each already commit in their own
	// transaction, so a crash between phases used to leave real,
	// already-committed imported data with zero audit trail, since the
	// one combined entry only ever reflected the *end* state. Auditing
	// per phase means whatever actually committed before a crash is
	// exactly what gets audited — no more, no less.
	logPhase := func(phase string, imported, skipped int, extra map[string]any) error {
		detail := map[string]any{"phase": phase, "imported": imported, "skipped": skipped}
		for k, v := range extra {
			detail[k] = v
		}
		return dbscope.Run(ctx, db, targetScope, targetScope, func(tx *sql.Tx) error {
			return audit.Write(ctx, tx, audit.Entry{
				EventType:      audit.EventImport,
				Actor:          actor,
				ActingScope:    targetScope,
				WorkspaceScope: targetScope,
				Detail:         detail,
			})
		})
	}

	if err := importEntities(ctx, db, enc, keyVersion, dir, targetScope, &stats); err != nil {
		return stats, wrapPartialImportFailure(err, targetScope, merge)
	}
	if err := logPhase("entities", stats.EntitiesImported, stats.EntitiesSkipped, nil); err != nil {
		return stats, wrapPartialImportFailure(fmt.Errorf("hpmf: audit log write for entity import into %s:%s: %w", targetScope.Kind, targetScope.Owner, err), targetScope, merge)
	}
	// summaryIDMap collects exported summary id -> freshly-assigned id
	// across every period file importSummaries processes (not just one
	// file's own local remapping) — importMemoryRelations needs the
	// complete picture to resolve a key-fact locator that references any
	// summary this run actually imported, regardless of which file it
	// came from.
	summaryIDMap := map[string]string{}
	if err := importSummaries(ctx, db, enc, keyVersion, dir, targetScope, &stats, summaryIDMap); err != nil {
		return stats, wrapPartialImportFailure(err, targetScope, merge)
	}
	if err := logPhase("summaries", stats.SummariesImported, stats.SummariesSkipped, map[string]any{"dangling_supersedes": stats.SummariesWithDanglingSupersedes}); err != nil {
		return stats, wrapPartialImportFailure(fmt.Errorf("hpmf: audit log write for summary import into %s:%s: %w", targetScope.Kind, targetScope.Owner, err), targetScope, merge)
	}
	if err := importEpisodes(ctx, db, enc, keyVersion, dir, targetScope, &stats); err != nil {
		return stats, wrapPartialImportFailure(err, targetScope, merge)
	}
	if err := logPhase("episodes", stats.EpisodesImported, stats.EpisodesSkipped, nil); err != nil {
		return stats, wrapPartialImportFailure(fmt.Errorf("hpmf: audit log write for episode import into %s:%s: %w", targetScope.Kind, targetScope.Owner, err), targetScope, merge)
	}
	// Last: a relation can reference either a key fact (just imported
	// above, resolved via summaryIDMap) or an entity attribute (imported
	// first, above) — both phases' data needs to already be committed.
	if err := importMemoryRelations(ctx, db, enc, dir, targetScope, summaryIDMap, &stats); err != nil {
		return stats, wrapPartialImportFailure(err, targetScope, merge)
	}
	if err := logPhase("memory_relations", stats.MemoryRelationsImported, stats.MemoryRelationsSkipped, nil); err != nil {
		return stats, wrapPartialImportFailure(fmt.Errorf("hpmf: audit log write for memory relation import into %s:%s: %w", targetScope.Kind, targetScope.Owner, err), targetScope, merge)
	}
	return stats, nil
}

// wrapPartialImportFailure appends a hint pointing at -merge to any error
// ImportScope returns after its upfront scopeIsEmpty check has already
// passed (review finding C6). Each phase (and, within importSummaries,
// each period file) commits in its own transaction, so a failure at any
// of these call sites can easily leave real, already-committed data in
// targetScope even though the whole import ultimately failed — without
// this, a non-merge retry would only discover it needs -merge after a
// second round trip, hitting the "already has data" check above from
// scratch. Only applies when merge is false: a merge import failing
// doesn't change what flag a retry needs.
func wrapPartialImportFailure(err error, targetScope identity.Scope, merge bool) error {
	if err == nil || merge {
		return err
	}
	return fmt.Errorf("%w (if an earlier phase already committed data to %s:%s, retry with -merge to continue)", err, targetScope.Kind, targetScope.Owner)
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
			_, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, first_seen, last_updated, scope_kind, scope_owner, key_version)
				values ($1, $2, $3, $4, $5, $6, $7, $8)
			`, r.ID, r.Kind, r.Name, r.FirstSeen, r.LastUpdated, scope.Kind, scope.Owner, keyVersion)
			if err != nil {
				return fmt.Errorf("hpmf: insert entity %s: %w", r.ID, err)
			}

			// Attributes land in the unified memories table (schema/0025,
			// schema/0027), one row per key — same shape upsertEntities
			// writes going forward, not entities.attributes, which this
			// import no longer populates at all (see that function's own
			// doc comment on why). A brand-new entity has no prior
			// attribute row to supersede, so every row here starts a fresh
			// chain.
			var attrs map[string]string
			if len(r.Attributes) > 0 {
				if err := json.Unmarshal(r.Attributes, &attrs); err != nil {
					return fmt.Errorf("hpmf: parse entity %s attributes: %w", r.ID, err)
				}
			}
			for key, value := range attrs {
				ct, err := enc.Encrypt(value)
				if err != nil {
					return fmt.Errorf("hpmf: encrypt entity %s attribute %s: %w", r.ID, key, err)
				}
				rowID, err := entityattrs.NewID()
				if err != nil {
					return fmt.Errorf("hpmf: generate id for entity %s attribute %s: %w", r.ID, key, err)
				}
				_, err = tx.ExecContext(ctx, `
					insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded)
					values ($1, $2, $3, $4, $5, $6, $7, true, true)
				`, rowID, scope.Kind, scope.Owner, r.ID, key, ct, keyVersion)
				if err != nil {
					return fmt.Errorf("hpmf: insert attribute for entity %s key %s: %w", r.ID, key, err)
				}
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
func importSummaries(ctx context.Context, db *sql.DB, enc *crypto.Encryptor, keyVersion int, dir string, scope identity.Scope, stats *ImportStats, summaryIDMap map[string]string) error {
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

			// supersedes only ever points within the same file (a
			// period's own version chain) — summaryIDMap is shared
			// across every file only so importMemoryRelations can look
			// up any of this run's summaries afterward, not because
			// cross-file supersedes resolution is expected here.
			idMap := summaryIDMap
			for _, r := range versions {
				newID, err := nextImportSummaryID(ctx, tx, scope, level, period)
				if err != nil {
					return err
				}
				supersedes := ""
				if r.Supersedes != "" {
					var ok bool
					supersedes, ok = idMap[r.Supersedes]
					if !ok {
						// A dangling reference: the predecessor this
						// version's supersedes pointed at isn't present in
						// the bundle (e.g. a partial/filtered export).
						// Still imported — refusing the whole file over a
						// lost lineage link would be worse — but as an
						// unlinked version, indistinguishable from an
						// original, non-corrected summary unless this is
						// recorded somewhere.
						slog.Warn("hpmf: summary version's supersedes reference not found in bundle, importing as unlinked",
							"exported_id", r.ID, "missing_predecessor", r.Supersedes, "scope_kind", scope.Kind, "scope_owner", scope.Owner)
						stats.SummariesWithDanglingSupersedes++
					}
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
					var factRowID int64
					err = tx.QueryRowContext(ctx, `
						insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, key_version, scope_kind, scope_owner, source_count, expires_at, expire_reason, is_inference)
						values ($1, $2, $3::text[], $4, $5, $6, $7, $8, $9::date, $10, $11)
						returning id
					`, newID, factCT, pgfmt.TextArray(kf.SourceEpisodeIDs), kf.Grounded, keyVersion, scope.Kind, scope.Owner,
						keyFactSourceCountOrDefault(kf.SourceCount), pgfmt.Nullable(kf.ExpiresAt), pgfmt.Nullable(kf.ExpireReason), kf.IsInference).Scan(&factRowID)
					if err != nil {
						return fmt.Errorf("hpmf: insert key fact for %s: %w", newID, err)
					}

					// Mirrored into memories under the same deterministic
					// id scheme storeSummary's own live write uses
					// (memoryIDForImportedKeyFact) — without this, an
					// imported fact would have no memories row at all,
					// breaking RefKindMemory citation resolution for it
					// and leaving nothing for importMemoryRelations below
					// to point an edge at.
					if _, err := tx.ExecContext(ctx, `
						insert into memories (id, scope_kind, scope_owner, summary_id, content, key_version, is_static, grounded, source_episode_ids, expires_at, expire_reason, source_count, is_inference)
						values ($1, $2, $3, $4, $5, $6, false, $7, $8::text[], $9::date, $10, $11, $12)
					`, memoryIDForImportedKeyFact(factRowID), scope.Kind, scope.Owner, newID, factCT, keyVersion, kf.Grounded,
						pgfmt.TextArray(kf.SourceEpisodeIDs), pgfmt.Nullable(kf.ExpiresAt), pgfmt.Nullable(kf.ExpireReason),
						keyFactSourceCountOrDefault(kf.SourceCount), kf.IsInference); err != nil {
						return fmt.Errorf("hpmf: insert memories mirror for key fact %d (%s): %w", factRowID, newID, err)
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

// keyFactSourceCountOrDefault treats an unset (zero-value) SourceCount
// as 1, the schema's own column default — needed because a bundle
// exported before KeyFactRecord gained this field (or a hand-edited one
// that simply omits it, since the json tag is omitempty) unmarshals to
// 0, not 1, and 0 would otherwise misrepresent a never-reinforced fact
// as somehow reinforced zero times.
func keyFactSourceCountOrDefault(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// memoryIDForImportedKeyFact mirrors
// internal/consolidation/store.go's own memoryIDForKeyFact exactly
// (same id scheme: "mem_fact_" + the summary_key_facts row id) —
// duplicated rather than imported, same established precedent as that
// function's own doc comment (internal/gateway/attribution.go's
// extractJSON): a one-line, self-contained helper isn't worth a
// cross-package dependency.
func memoryIDForImportedKeyFact(factID int64) string {
	return "mem_fact_" + strconv.FormatInt(factID, 10)
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

// importMemoryRelations reads memory_relations.jsonl and recreates each
// edge against the data this same run just imported — run after both
// importEntities and importSummaries have committed, since a relation
// can reference either. Each MemoryLocator is resolved to a real,
// freshly-written memories.id: a key fact locator via summaryIDMap (the
// exported summary id -> this run's own new id) plus a by-text match
// among that summary's own key facts; an attribute locator via
// entityattrs.CurrentRowIDs. Either resolution failing means the edge is
// unresolvable this run (see ImportStats.MemoryRelationsSkipped's own
// doc comment) — skipped with a warning logged, never a hard failure,
// same posture importSummaries already takes for a dangling supersedes
// reference.
func importMemoryRelations(ctx context.Context, db *sql.DB, enc *crypto.Encryptor, dir string, scope identity.Scope, summaryIDMap map[string]string, stats *ImportStats) error {
	path := filepath.Join(dir, "memory_relations.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	records, err := readJSONL[MemoryRelationRecord](path)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}

	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		// Cached per (new summary id) so a summary with several
		// relations referencing it only decrypts its key facts once,
		// not once per edge.
		keyFactsBySummary := map[string]map[string]string{} // new summary id -> fact text -> memories id

		resolve := func(loc MemoryLocator) (string, bool, error) {
			if loc.EntityID != "" {
				ids, err := entityattrs.CurrentRowIDs(ctx, tx, scope, loc.EntityID)
				if err != nil {
					return "", false, fmt.Errorf("hpmf: load current attribute ids for entity %s: %w", loc.EntityID, err)
				}
				id, ok := ids[loc.AttributeKey]
				return id, ok, nil
			}
			newSummaryID, ok := summaryIDMap[loc.SummaryID]
			if !ok {
				return "", false, nil
			}
			byText, ok := keyFactsBySummary[newSummaryID]
			if !ok {
				rows, err := tx.QueryContext(ctx, `
					select id, content from memories
					where scope_kind = $1 and scope_owner = $2 and summary_id = $3 and is_static = false
				`, scope.Kind, scope.Owner, newSummaryID)
				if err != nil {
					return "", false, fmt.Errorf("hpmf: load key fact memories for summary %s: %w", newSummaryID, err)
				}
				byText = map[string]string{}
				for rows.Next() {
					var id string
					var contentCT []byte
					if err := rows.Scan(&id, &contentCT); err != nil {
						rows.Close()
						return "", false, err
					}
					fact, err := enc.Decrypt(contentCT)
					if err != nil {
						rows.Close()
						return "", false, fmt.Errorf("hpmf: decrypt key fact memories row %s: %w", id, err)
					}
					byText[fact] = id
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					return "", false, err
				}
				rows.Close()
				keyFactsBySummary[newSummaryID] = byText
			}
			id, ok := byText[loc.Fact]
			return id, ok, nil
		}

		for _, r := range records {
			fromID, ok, err := resolve(r.From)
			if err != nil {
				return err
			}
			if !ok {
				slog.Warn("hpmf: memory relation's from-side couldn't be resolved this run, skipping", "relation_type", r.RelationType, "from", r.From, "scope_kind", scope.Kind, "scope_owner", scope.Owner)
				stats.MemoryRelationsSkipped++
				continue
			}
			toID, ok, err := resolve(r.To)
			if err != nil {
				return err
			}
			if !ok {
				slog.Warn("hpmf: memory relation's to-side couldn't be resolved this run, skipping", "relation_type", r.RelationType, "to", r.To, "scope_kind", scope.Kind, "scope_owner", scope.Owner)
				stats.MemoryRelationsSkipped++
				continue
			}

			relID, err := newImportRelationID()
			if err != nil {
				return fmt.Errorf("hpmf: generate memory_relations id: %w", err)
			}
			// Guards a bundle imported twice (by mistake, or a repeated
			// -merge run) from doubling every edge — this run's own
			// freshly-resolved ids are a reliable dedup key even though
			// relation rows have no natural unique constraint of their
			// own (schema/0025's own comment on why).
			res, err := tx.ExecContext(ctx, `
				insert into memory_relations (id, scope_kind, scope_owner, from_memory_id, to_memory_id, relation_type)
				select $1, $2, $3, $4, $5, $6
				where not exists (
					select 1 from memory_relations
					where scope_kind = $2 and scope_owner = $3 and from_memory_id = $4 and to_memory_id = $5 and relation_type = $6
				)
			`, relID, scope.Kind, scope.Owner, fromID, toID, r.RelationType)
			if err != nil {
				return fmt.Errorf("hpmf: insert memory relation %s -> %s: %w", fromID, toID, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				stats.MemoryRelationsImported++
			} else {
				stats.MemoryRelationsSkipped++
			}
		}
		return nil
	})
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

// newImportRelationID mirrors internal/consolidation/store.go's own
// newRelationshipID exactly (same "rel_" + random hex shape) — same
// duplication reasoning as memoryIDForImportedKeyFact above.
func newImportRelationID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate relation id: %w", err)
	}
	return "rel_" + hex.EncodeToString(buf), nil
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
