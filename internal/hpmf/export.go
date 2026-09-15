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
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// ExportScope writes one scope's episodes/summaries/entities, decrypted,
// into dir (created if it doesn't exist) — dir is "." for a flat,
// single-scope export or "scopes/<kind>-<owner>" for one scope within a
// whole-deployment export (see ScopeManifest.Dir's doc comment); the
// caller decides which, this function doesn't care. Logs one `export`
// audit_log entry per call — a scope's entire history leaving the live
// store is significant enough to record even when nothing about the
// live data itself changes (docs/GAP_CLOSURE_PLAN.md §4.2).
func ExportScope(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir, actor string) (ScopeManifest, error) {
	m := ScopeManifest{ScopeKind: scope.Kind, ScopeOwner: scope.Owner, Dir: dir}

	// Each row resolves its own decrypting key by its own key_version
	// column below (keys.GetVersion), not one encryptor for the whole
	// scope — a scope's history can span more than one DEK generation if
	// hupi-rotate-key has ever touched it (docs/GAP_CLOSURE_PLAN.md §4.4).
	if err := exportEpisodes(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := exportSummaries(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}
	if err := exportEntities(ctx, db, keys, scope, dir, &m); err != nil {
		return ScopeManifest{}, err
	}

	auditErr := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventExport,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			Detail: map[string]any{
				"episode_count": m.EpisodeCount, "summary_count": m.SummaryCount, "entity_count": m.EntityCount,
			},
		})
	})
	if auditErr != nil {
		return ScopeManifest{}, fmt.Errorf("hpmf: audit log write for export of %s:%s: %w", scope.Kind, scope.Owner, auditErr)
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
		select fact, source_episode_ids, grounded, key_version from summary_key_facts where summary_id = $1 order by fact
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
		if err := rows.Scan(&factCT, &sourceIDsLit, &kf.Grounded, &keyVersion); err != nil {
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
		out = append(out, kf)
	}
	return out, rows.Err()
}

func exportEntities(ctx context.Context, db *sql.DB, keys *crypto.KeyStore, scope identity.Scope, dir string, m *ScopeManifest) error {
	return dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, kind, name, first_seen, last_updated, attributes, key_version
			from entities where scope_kind = $1 and scope_owner = $2 order by id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return fmt.Errorf("hpmf: query entities: %w", err)
		}
		defer rows.Close()

		w, err := newJSONLWriter(filepath.Join(dir, "entities.jsonl"))
		if err != nil {
			return err
		}
		defer w.Close()

		for rows.Next() {
			var r EntityRecord
			var attrsCT []byte
			var keyVersion int
			if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.FirstSeen, &r.LastUpdated, &attrsCT, &keyVersion); err != nil {
				return fmt.Errorf("hpmf: scan entity: %w", err)
			}
			enc, err := keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				return fmt.Errorf("hpmf: resolve encryption key for entity %s: %w", r.ID, err)
			}
			attrs, err := enc.Decrypt(attrsCT)
			if err != nil {
				return fmt.Errorf("hpmf: decrypt entity %s attributes: %w", r.ID, err)
			}
			if attrs != "" {
				r.Attributes = json.RawMessage(attrs)
			}
			if err := w.Write(r); err != nil {
				return err
			}
			m.EntityCount++
		}
		return rows.Err()
	})
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
