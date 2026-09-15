package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

type storeSummaryInput struct {
	scope                identity.Scope
	level                string
	period               string
	sourceEpisodeIDs     []string // daily only
	sourceSummaryPeriods []string // weekly/monthly/yearly only
	output               ConsolidationOutput
	groundingSourceText  string
	supersedes           string // set only by Runner.Correct — id of the prior version this corrects
	correctionReason     string // required alongside supersedes, see MEMORY_FORMAT.md § Grounding & correction
	actor                string // audit_log actor — systemActor for RunDaily/RunRollup, the operator's -actor for Correct
}

// storeSummary runs the grounding check, then writes the summary and its
// key facts in one transaction, upserts touched entities, and embeds the
// summary text — in that order, matching ARCHITECTURE.md §§ Consolidation
// integrity safeguards and Consolidation engine. The grounding check is a
// network call to an LLM and deliberately runs before the transaction
// opens (docs/HARDENING_PLAN.md D3); everything from nextSummaryID's
// version lookup through the entity upserts shares one transaction so the
// version-counting query and the insert that relies on it are consistent.
func (r *Runner) storeSummary(ctx context.Context, in storeSummaryInput) error {
	grounded, err := r.groundingCheck(ctx, in.groundingSourceText, in.output.KeyFacts)
	if err != nil {
		return fmt.Errorf("grounding check: %w", err)
	}

	enc, keyVersion, err := r.keys.GetOrCreate(ctx, in.scope)
	if err != nil {
		return fmt.Errorf("resolve encryption key for %s:%s: %w", in.scope.Kind, in.scope.Owner, err)
	}

	summaryCT, err := enc.Encrypt(in.output.Summary)
	if err != nil {
		return fmt.Errorf("encrypt summary text: %w", err)
	}

	entityIDs := make([]string, 0, len(in.output.EntitiesTouched))
	for _, e := range in.output.EntitiesTouched {
		if e.ID != "" {
			entityIDs = append(entityIDs, e.ID)
		}
	}

	var id string
	err = dbscope.Run(ctx, r.db, in.scope, in.scope, func(tx *sql.Tx) error {
		var err error
		id, err = r.nextSummaryID(ctx, tx, in.scope, in.level, in.period)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			insert into summaries (
				id, period, level, status, generated_by_vendor, generated_by_model,
				source_episode_ids, source_summary_periods, summary, entities_touched,
				grounding_checked, supersedes, correction_reason,
				scope_kind, scope_owner, key_version
			) values (
				$1, $2, $3, 'draft', $4, $5,
				$6::text[], $7::text[], $8, $9::text[],
				true, $10, $11,
				$12, $13, $14
			)
		`,
			id, in.period, in.level, r.consolidation.Vendor(), r.consolidation.Model(),
			pgfmt.TextArray(in.sourceEpisodeIDs), pgfmt.TextArray(in.sourceSummaryPeriods), summaryCT, pgfmt.TextArray(entityIDs),
			pgfmt.Nullable(in.supersedes), pgfmt.Nullable(in.correctionReason),
			in.scope.Kind, in.scope.Owner, keyVersion,
		)
		if err != nil {
			return fmt.Errorf("insert summary %s: %w", id, err)
		}

		for i, kf := range in.output.KeyFacts {
			factCT, err := enc.Encrypt(kf.Fact)
			if err != nil {
				return fmt.Errorf("encrypt key fact: %w", err)
			}
			// Per-fact episode citations only make sense at the daily
			// level — above that, traceability runs through
			// source_summary_periods on the summary itself (see
			// MEMORY_FORMAT.md § Summary record).
			var citeIDs []string
			if in.level == "daily" {
				citeIDs = kf.SourceEpisodeIDs
			}
			_, err = tx.ExecContext(ctx, `
				insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, key_version)
				values ($1, $2, $3::text[], $4, $5)
			`, id, factCT, pgfmt.TextArray(citeIDs), grounded[i], keyVersion)
			if err != nil {
				return fmt.Errorf("insert key fact %d for summary %s: %w", i, id, err)
			}
		}

		if err := r.upsertEntities(ctx, tx, in.scope, in.output.EntitiesTouched); err != nil {
			return fmt.Errorf("upsert entities for summary %s: %w", id, err)
		}

		eventType := audit.EventCapture
		if in.supersedes != "" {
			eventType = audit.EventCorrect
		}
		actor := in.actor
		if actor == "" {
			actor = systemActor
		}
		if err := audit.Write(ctx, tx, audit.Entry{
			EventType:      eventType,
			Actor:          actor,
			ActingScope:    in.scope,
			WorkspaceScope: in.scope,
			TargetRef:      &identity.Ref{Kind: identity.RefKindSummary, Scope: in.scope, ID: id},
			Detail:         map[string]any{"level": in.level, "period": in.period},
		}); err != nil {
			return fmt.Errorf("audit log write for summary %s: %w", id, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Embedding runs after commit, best-effort: a failed embed means this
	// summary won't surface via vector search until the next reindex, not
	// that consolidation itself failed — the summary row is already
	// durably stored.
	if err := r.embedSummary(ctx, in.scope, id, in.output.Summary); err != nil {
		return fmt.Errorf("embed summary %s (row committed, embedding not): %w", id, err)
	}
	return nil
}

// nextSummaryID assigns sum_<scope_owner>_<period>_<level>_v<N>,
// incrementing N over any prior (including superseded) versions for the
// same scope+period+level — see MEMORY_FORMAT.md § Grounding & correction
// on why corrections are new versions, never edits. Runs inside
// storeSummary's transaction so the version lookup and the insert that
// depends on it are consistent within one transaction, not two.
//
// The scope_owner segment exists because summaries.id remains a plain,
// single-column primary key (unlike entities — see
// schema/0002_tier3_phase1_identity.sql's comment on why summaries didn't
// get the same composite-key treatment: summary_key_facts.summary_id and
// summaries.supersedes both hold real foreign keys into this column, which
// requires it to stay globally unique on its own). Without a scope segment,
// two different scopes' first daily summary on the same date would
// generate the identical id and collide.
func (r *Runner) nextSummaryID(ctx context.Context, q dbscope.Querier, scope identity.Scope, level, period string) (string, error) {
	var maxVersion int
	err := q.QueryRowContext(ctx, `
		select coalesce(max(cast(substring(id from 'v([0-9]+)$') as int)), 0)
		from summaries where level = $1 and period = $2 and scope_kind = $3 and scope_owner = $4
	`, level, period, scope.Kind, scope.Owner).Scan(&maxVersion)
	if err != nil {
		return "", fmt.Errorf("determine next summary version for %s %s: %w", level, period, err)
	}
	return fmt.Sprintf("sum_%s_%s_%s_v%d", scope.Owner, period, level, maxVersion+1), nil
}

// upsertEntities merges each entity's new attributes over its existing
// ones rather than replacing them wholesale (docs/DESIGN_VS_BUILT.md #1):
// a consolidation run that only mentions one attribute of a previously
// richer entity must not erase everything else that was known about it.
// `for update` guards the read against another consolidation process
// running concurrently against the same entity. Entities are looked up and
// written within scope — the same "project:hupi" id can exist once per
// scope since entities' primary key is (scope_kind, scope_owner, id).
func (r *Runner) upsertEntities(ctx context.Context, tx *sql.Tx, scope identity.Scope, updates []EntityUpdate) error {
	// Writes always go under the *current* version — resolved once here,
	// not per entity, since it can't change mid-transaction. Reads of
	// each entity's *existing* attributes below use that specific row's
	// own key_version instead, which may still be an older one — this is
	// the one write path that's also a read, so both GetOrCreate and
	// GetVersion show up in the same function.
	enc, keyVersion, err := r.keys.GetOrCreate(ctx, scope)
	if err != nil {
		return fmt.Errorf("resolve encryption key for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	for _, e := range updates {
		if e.ID == "" {
			continue
		}

		merged := e.Attributes
		var existingCT []byte
		var existingVersion int
		err := tx.QueryRowContext(ctx,
			`select attributes, key_version from entities where scope_kind = $1 and scope_owner = $2 and id = $3 for update`,
			scope.Kind, scope.Owner, e.ID,
		).Scan(&existingCT, &existingVersion)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// new entity — nothing to merge, e.Attributes as given
		case err != nil:
			return fmt.Errorf("load existing entity %s for merge: %w", e.ID, err)
		default:
			existingEnc, keyErr := r.keys.GetVersion(ctx, scope, existingVersion)
			if keyErr != nil {
				return fmt.Errorf("resolve encryption key for existing entity %s: %w", e.ID, keyErr)
			}
			existingJSON, decErr := existingEnc.Decrypt(existingCT)
			if decErr != nil {
				return fmt.Errorf("decrypt existing attributes for entity %s: %w", e.ID, decErr)
			}
			var existing map[string]string
			if existingJSON != "" {
				if jsonErr := json.Unmarshal([]byte(existingJSON), &existing); jsonErr != nil {
					return fmt.Errorf("parse existing attributes for entity %s: %w", e.ID, jsonErr)
				}
			}
			merged = mergeAttributes(existing, e.Attributes)
		}

		attrsJSON, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("marshal attributes for entity %s: %w", e.ID, err)
		}
		// Re-encrypted under the current version even if the existing row
		// was on an older one — a touched entity naturally migrates
		// forward, one row at a time, independent of whether
		// hupi-rotate-key ever gets around to it.
		attrsCT, err := enc.Encrypt(string(attrsJSON))
		if err != nil {
			return fmt.Errorf("encrypt attributes for entity %s: %w", e.ID, err)
		}
		_, err = tx.ExecContext(ctx, `
			insert into entities (id, kind, name, attributes, scope_kind, scope_owner, key_version)
			values ($1, $2, $3, $4, $5, $6, $7)
			on conflict (scope_kind, scope_owner, id) do update set
				name = excluded.name,
				attributes = excluded.attributes,
				key_version = excluded.key_version,
				last_updated = current_date
		`, e.ID, e.Kind, e.Name, attrsCT, scope.Kind, scope.Owner, keyVersion)
		if err != nil {
			return fmt.Errorf("upsert entity %s: %w", e.ID, err)
		}
	}
	return nil
}

// mergeAttributes overlays incoming key/value pairs onto the existing set:
// new keys win, keys this run didn't mention survive from before. This is
// what makes entity state cumulative instead of a lossy overwrite.
func mergeAttributes(existing, incoming map[string]string) map[string]string {
	if existing == nil {
		return incoming
	}
	merged := make(map[string]string, len(existing)+len(incoming))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range incoming {
		merged[k] = v
	}
	return merged
}

func (r *Runner) embedSummary(ctx context.Context, scope identity.Scope, id, text string) error {
	resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{text}})
	if err != nil {
		return err
	}
	if len(resp.Vectors) == 0 {
		return errors.New("embedder returned no vectors")
	}
	vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `update summaries set embedding = $1::vector where id = $2`, vectorLiteral, id)
		return err
	})
}
