package hpmf

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// ExportScope writes one scope's episodes/summaries/entities, decrypted,
// into dir (created if it doesn't exist) — dir is "." for a flat,
// single-scope export or "scopes/<kind>-<owner>" for one scope within a
// whole-deployment export (see ScopeManifest.Dir's doc comment); the
// caller decides which, this function doesn't care. Logs one `export`
// audit_log entry per phase (episodes, summaries, entities) — a real,
// confirmed bug this used to have (docs/CODEBASE_SURVEY_AND_REVIEW.md
// finding A10): a single combined entry written only after all three
// phases finished meant a crash partway through left real, decrypted
// data already written to local disk — a scope's entire history leaving
// the live store, exactly the event docs/GAP_CLOSURE_PLAN.md §4.2 calls
// significant enough to always record — with zero audit trail for any
// of it. True atomicity between a filesystem write and a Postgres audit
// row isn't achievable (they're two different systems), but auditing
// each phase immediately after its own files are written, the same
// pattern internal/reembed uses for its own batches, closes the gap for
// every phase that actually completed rather than deferring all of them
// to a single end-of-run entry that might never be reached.
func ExportScope(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir, actor string) (ScopeManifest, error) {
	m := ScopeManifest{ScopeKind: scope.Kind, ScopeOwner: scope.Owner, Dir: dir}

	logPhase := func(phase string, count int) error {
		return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
			return audit.Write(ctx, tx, audit.Entry{
				EventType:      audit.EventExport,
				Actor:          actor,
				ActingScope:    scope,
				WorkspaceScope: scope,
				Detail:         map[string]any{"phase": phase, "count": count},
			})
		})
	}

	// Each row resolves its own decrypting key by its own key_version
	// column below (keys.GetVersion), not one encryptor for the whole
	// scope — a scope's history can span more than one DEK generation if
	// hupi-rotate-key has ever touched it (docs/GAP_CLOSURE_PLAN.md §4.4).
	if err := exportEpisodes(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := logPhase("episodes", m.EpisodeCount); err != nil {
		return ScopeManifest{}, fmt.Errorf("hpmf: audit log write for episode export of %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if err := exportSummaries(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := logPhase("summaries", m.SummaryCount); err != nil {
		return ScopeManifest{}, fmt.Errorf("hpmf: audit log write for summary export of %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	if err := exportEntities(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := logPhase("entities", m.EntityCount); err != nil {
		return ScopeManifest{}, fmt.Errorf("hpmf: audit log write for entity export of %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	// Last, not interleaved with summaries/entities above: a relation
	// can point at either a key fact (from the summaries phase) or an
	// entity attribute (from the entities phase), so resolving its
	// locator (resolveMemoryLocator) re-queries memories directly rather
	// than depending on either phase's own already-closed result set.
	if err := exportMemoryRelations(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := logPhase("memory_relations", m.MemoryRelationCount); err != nil {
		return ScopeManifest{}, fmt.Errorf("hpmf: audit log write for memory relation export of %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	return m, nil
}

func exportEpisodes(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir string, m *ScopeManifest) error {
	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, ts, type, provider_vendor, provider_model, input_text, output_text,
			       importance, hash, truncated, memory_gate, retrieved_refs, refers_to, rating, note, key_version
			from episodes where scope_kind = $1 and scope_owner = $2 order by ts asc
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: query episodes: %w", err)
		}
		defer rows.Close()

		var w *jsonlWriter
		var currentDay string
		defer func() {
			if w != nil {
				w.Close()
			}
		}()

		for rows.Next() {
			var (
				r                             EpisodeRecord
				providerVendor, providerModel sql.NullString
				importance                    sql.NullFloat64
				memoryGate, refersTo, rating  sql.NullString
				inputCT, outputCT, noteCT     []byte
				refsJSON                      []byte
				keyVersion                    int
			)
			if err := rows.Scan(
				&r.ID, &r.TS, &r.Type, &providerVendor, &providerModel,
				&inputCT, &outputCT, &importance, &r.Hash, &r.Truncated,
				&memoryGate, &refsJSON, &refersTo, &rating, &noteCT, &keyVersion,
			); err != nil {
				return fmt.Errorf("hpmf: scan episode: %w", err)
			}
			r.ProviderVendor = providerVendor.String
			r.ProviderModel = providerModel.String
			if importance.Valid {
				r.Importance = &importance.Float64
			}
			r.MemoryGate = memoryGate.String
			r.RefersTo = refersTo.String
			r.Rating = rating.String

			enc, err := keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				return fmt.Errorf("hpmf: resolve encryption key for episode %s: %w", r.ID, err)
			}
			if r.InputText, err = enc.Decrypt(inputCT); err != nil {
				return fmt.Errorf("hpmf: decrypt episode %s input_text: %w", r.ID, err)
			}
			if r.OutputText, err = enc.Decrypt(outputCT); err != nil {
				return fmt.Errorf("hpmf: decrypt episode %s output_text: %w", r.ID, err)
			}
			if note, err := enc.Decrypt(noteCT); err != nil {
				return fmt.Errorf("hpmf: decrypt episode %s note: %w", r.ID, err)
			} else {
				r.Note = note
			}
			if len(refsJSON) > 0 {
				if err := json.Unmarshal(refsJSON, &r.RetrievedRefs); err != nil {
					return fmt.Errorf("hpmf: parse retrieved_refs for episode %s: %w", r.ID, err)
				}
			}

			day := r.TS.Format("2006-01-02")
			if day != currentDay {
				if w != nil {
					if err := w.Close(); err != nil {
						return err
					}
				}
				path := filepath.Join(dir, "episodes", r.TS.Format("2006"), r.TS.Format("01"), day+".jsonl")
				w, err = newJSONLWriter(path)
				if err != nil {
					return err
				}
				currentDay = day
			}
			if err := w.Write(r); err != nil {
				return err
			}

			m.EpisodeCount++
			ts := r.TS
			if m.EarliestEpisodeAt == nil || ts.Before(*m.EarliestEpisodeAt) {
				m.EarliestEpisodeAt = &ts
			}
			if m.LatestEpisodeAt == nil || ts.After(*m.LatestEpisodeAt) {
				m.LatestEpisodeAt = &ts
			}
		}
		return rows.Err()
	})
}

// exportSummaries writes one JSON array per (level, period) —
// summaries/<level>/<period>.json — holding every version ever written
// for that period, not just the current one: a correction's old version
// and its correction_reason are part of this project's integrity story
// (MEMORY_FORMAT.md § Grounding & correction) and don't get silently
// dropped just because the record left the live database.
func exportSummaries(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir string, m *ScopeManifest) error {
	type key struct{ level, period string }
	grouped := map[key][]SummaryRecord{}
	var order []key

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, period, level, status, generated_by_vendor, generated_by_model,
			       source_episode_ids, source_summary_periods, summary, entities_touched,
			       grounding_checked, supersedes, correction_reason, created_at, key_version
			from summaries where scope_kind = $1 and scope_owner = $2 order by level, period, created_at
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: query summaries: %w", err)
		}

		// Fully scanned into memory and rows closed before any nested
		// query (loadKeyFacts, below) runs on the same tx — issuing a
		// second query on a *sql.Tx while an earlier Rows cursor from
		// that same tx is still open isn't safe (surfaces as "driver: bad
		// connection" under pgx, found by actually running this against
		// real Postgres, not by inspection).
		var scanned []SummaryRecord
		for rows.Next() {
			var (
				r                                                                SummaryRecord
				genVendor, genModel, supersedes, reason                          sql.NullString
				sourceEpisodeIDsLit, sourceSummaryPeriodsLit, entitiesTouchedLit string
				summaryCT                                                        []byte
				keyVersion                                                       int
			)
			if err := rows.Scan(
				&r.ID, &r.Period, &r.Level, &r.Status, &genVendor, &genModel,
				&sourceEpisodeIDsLit, &sourceSummaryPeriodsLit, &summaryCT, &entitiesTouchedLit,
				&r.GroundingChecked, &supersedes, &reason, &r.CreatedAt, &keyVersion,
			); err != nil {
				rows.Close()
				return fmt.Errorf("hpmf: scan summary: %w", err)
			}
			r.GeneratedByVendor = genVendor.String
			r.GeneratedByModel = genModel.String
			r.Supersedes = supersedes.String
			r.CorrectionReason = reason.String
			r.SourceEpisodeIDs = pgfmt.ParseTextArray(sourceEpisodeIDsLit)
			r.SourceSummaryPeriods = pgfmt.ParseTextArray(sourceSummaryPeriodsLit)
			r.EntitiesTouched = pgfmt.ParseTextArray(entitiesTouchedLit)

			enc, err := keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				rows.Close()
				return fmt.Errorf("hpmf: resolve encryption key for summary %s: %w", r.ID, err)
			}
			summary, err := enc.Decrypt(summaryCT)
			if err != nil {
				rows.Close()
				return fmt.Errorf("hpmf: decrypt summary %s: %w", r.ID, err)
			}
			r.Summary = summary
			scanned = append(scanned, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		for _, r := range scanned {
			facts, err := loadKeyFacts(ctx, tx, keys, scope, r.ID)
			if err != nil {
				return err
			}
			r.KeyFacts = facts

			k := key{r.Level, r.Period}
			if _, seen := grouped[k]; !seen {
				order = append(order, k)
			}
			grouped[k] = append(grouped[k], r)
			m.SummaryCount++
		}
		return nil
	})
	if err != nil {
		return err
	}

	for _, k := range order {
		path := filepath.Join(dir, "summaries", k.level, k.period+".json")
		if err := writeJSONFile(path, grouped[k]); err != nil {
			return err
		}
	}
	return nil
}

func loadKeyFacts(ctx context.Context, q dbscope.Querier, keys *crypto.KeyStore, scope identity.Scope, summaryID string) ([]KeyFactRecord, error) {
	rows, err := q.QueryContext(ctx, `
		select fact, source_episode_ids, grounded, key_version, source_count, expires_at::text, expire_reason, is_inference
		from summary_key_facts where summary_id = $1 order by fact
	`, summaryID)
	if err != nil {
		return nil, fmt.Errorf("hpmf: query key facts for %s: %w", summaryID, err)
	}
	defer rows.Close()

	var out []KeyFactRecord
	for rows.Next() {
		var factCT []byte
		var sourceIDsLit string
		var kf KeyFactRecord
		var keyVersion int
		var expiresAt, expireReason sql.NullString
		if err := rows.Scan(&factCT, &sourceIDsLit, &kf.Grounded, &keyVersion, &kf.SourceCount, &expiresAt, &expireReason, &kf.IsInference); err != nil {
			return nil, fmt.Errorf("hpmf: scan key fact for %s: %w", summaryID, err)
		}
		enc, err := keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("hpmf: resolve encryption key for key fact of %s: %w", summaryID, err)
		}
		fact, err := enc.Decrypt(factCT)
		if err != nil {
			return nil, fmt.Errorf("hpmf: decrypt key fact for %s: %w", summaryID, err)
		}
		kf.Fact = fact
		kf.SourceEpisodeIDs = pgfmt.ParseTextArray(sourceIDsLit)
		kf.ExpiresAt = expiresAt.String
		kf.ExpireReason = expireReason.String
		out = append(out, kf)
	}
	return out, rows.Err()
}

func exportEntities(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir string, m *ScopeManifest) error {
	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, kind, name, first_seen, last_updated
			from entities where scope_kind = $1 and scope_owner = $2 order by id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: query entities: %w", err)
		}
		var records []EntityRecord
		for rows.Next() {
			var r EntityRecord
			if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.FirstSeen, &r.LastUpdated); err != nil {
				rows.Close()
				return fmt.Errorf("hpmf: scan entity: %w", err)
			}
			records = append(records, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		w, err := newJSONLWriter(filepath.Join(dir, "entities.jsonl"))
		if err != nil {
			return err
		}
		defer w.Close()

		// Attributes are read from the unified memories table
		// (entityattrs.Current), not entities.attributes — queried one
		// entity at a time, after the entities rows cursor above is fully
		// closed, same two-pass convention internal/backfillmemories
		// already uses for the identical reason (issuing a second query
		// on the same tx while the first rows set is still open isn't
		// safe to rely on).
		for _, r := range records {
			attrs, err := entityattrs.Current(ctx, tx, keys, scope, r.ID)
			if err != nil {
				return fmt.Errorf("hpmf: load current attributes for entity %s: %w", r.ID, err)
			}
			if len(attrs) > 0 {
				attrsJSON, err := json.Marshal(attrs)
				if err != nil {
					return fmt.Errorf("hpmf: marshal entity %s attributes: %w", r.ID, err)
				}
				r.Attributes = attrsJSON
			}
			if err := w.Write(r); err != nil {
				return err
			}
			m.EntityCount++
		}
		return nil
	})
}

// exportMemoryRelations writes memory_relations.jsonl — one line per
// edge, each endpoint resolved to a portable MemoryLocator rather than
// the live schema's own memories.id (see that type's doc comment for
// why). Rows are fully scanned and closed before resolveMemoryLocator's
// own per-row queries run on the same tx, the same two-pass convention
// exportSummaries already uses for its own nested loadKeyFacts calls.
func exportMemoryRelations(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir string, m *ScopeManifest) error {
	type rawRelation struct {
		relationType, fromID, toID string
	}
	var raw []rawRelation
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select relation_type, from_memory_id, to_memory_id from memory_relations
			where scope_kind = $1 and scope_owner = $2
			order by created_at, id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: query memory relations: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r rawRelation
			if err := rows.Scan(&r.relationType, &r.fromID, &r.toID); err != nil {
				return err
			}
			raw = append(raw, r)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}

	var records []MemoryRelationRecord
	err = dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		for _, r := range raw {
			from, err := resolveMemoryLocator(ctx, tx, keys, scope, r.fromID)
			if err != nil {
				return fmt.Errorf("hpmf: resolve from_memory_id %s: %w", r.fromID, err)
			}
			to, err := resolveMemoryLocator(ctx, tx, keys, scope, r.toID)
			if err != nil {
				return fmt.Errorf("hpmf: resolve to_memory_id %s: %w", r.toID, err)
			}
			records = append(records, MemoryRelationRecord{RelationType: r.relationType, From: from, To: to})
		}
		return nil
	})
	if err != nil {
		return err
	}

	w, err := newJSONLWriter(filepath.Join(dir, "memory_relations.jsonl"))
	if err != nil {
		return err
	}
	defer w.Close()
	for _, rec := range records {
		if err := w.Write(rec); err != nil {
			return err
		}
		m.MemoryRelationCount++
	}
	return nil
}

// resolveMemoryLocator turns one memories.id into a portable
// MemoryLocator — a key fact (is_static = false: its owning summary's
// own id + decrypted fact text, the same content summary_key_facts.fact
// holds, since storeSummary writes both atomically from the same
// plaintext) or an entity attribute (is_static = true: entity_id +
// attribute_key, never needing decryption since those two columns are
// the locator itself, not the attribute's value).
func resolveMemoryLocator(ctx context.Context, tx *sql.Tx, keys *crypto.KeyStore, scope identity.Scope, memoryID string) (MemoryLocator, error) {
	var isStatic bool
	var summaryID, entityID, attributeKey sql.NullString
	var contentCT []byte
	var keyVersion int
	err := tx.QueryRowContext(ctx, `
		select is_static, summary_id, entity_id, attribute_key, content, key_version
		from memories where id = $1 and scope_kind = $2 and scope_owner = $3
	`, memoryID, scope.Kind, scope.Owner).Scan(&isStatic, &summaryID, &entityID, &attributeKey, &contentCT, &keyVersion)
	if err != nil {
		return MemoryLocator{}, fmt.Errorf("hpmf: load memories row %s: %w", memoryID, err)
	}
	if isStatic {
		return MemoryLocator{EntityID: entityID.String, AttributeKey: attributeKey.String}, nil
	}
	enc, err := keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return MemoryLocator{}, fmt.Errorf("hpmf: resolve encryption key for memories row %s: %w", memoryID, err)
	}
	fact, err := enc.Decrypt(contentCT)
	if err != nil {
		return MemoryLocator{}, fmt.Errorf("hpmf: decrypt memories row %s: %w", memoryID, err)
	}
	return MemoryLocator{SummaryID: summaryID.String, Fact: fact}, nil
}

// writeJSONFile writes v as indented JSON to path, creating parent
// directories as needed.
func writeJSONFile(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("hpmf: create directory for %s: %w", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("hpmf: marshal %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("hpmf: write %s: %w", path, err)
	}
	return nil
}
