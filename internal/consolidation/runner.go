// Package consolidation implements the nightly rollup job from
// ARCHITECTURE.md § Consolidation engine: daily episodes -> a grounding-
// checked daily summary, and (via RunRollup) lower-level summaries -> the
// next level up. It's invoked by cron (see cmd/hupi-consolidate), never by
// a live chat request — a crash or slow run here delays memory getting
// consolidated, it never blocks or breaks an in-flight conversation.
package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/metrics"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// consolidationParseRetries is how many times generateSummary re-asks
// the LLM after a genuinely malformed (not just wrapped-in-prose —
// extractJSON already handles that) JSON response, before giving up. A
// real, reproduced failure: one LLM call, out of many otherwise-valid
// ones from the same model, returned a raw JSON syntax error
// (invalid character '"' after object key:value pair) — non-
// deterministic enough that a second attempt is a real, cheap fix, not
// a way of hiding a systematic problem.
const consolidationParseRetries = 2

// Runner performs consolidation runs against Postgres.
type Runner struct {
	db   *sql.DB
	keys *crypto.KeyStore // one Encryptor per scope, see docs/HARDENING_PLAN.md D5-D7

	// consolidation is active_consolidation_provider — deliberately a
	// separate, more stable setting than active_chat_provider, see
	// ARCHITECTURE.md § Provider abstraction: switching your daily driver
	// model shouldn't incidentally change how your memory gets written.
	consolidation provider.Provider

	// grounding runs the second-pass fact check (grounding.go) and may be
	// the same Provider as consolidation, or a smaller/cheaper model —
	// ARCHITECTURE.md explicitly allows either.
	grounding provider.Provider

	embedder provider.Provider

	// TeamPromptOverride, if set, supplies a different consolidation
	// system prompt for a given scope (ok=false falls back to
	// summarySystemPrompt) — how a Tier-3 extension plugs in the
	// team-neutral-voice prompt (docs/TIER3_PLAN.md §5) without this
	// package needing to know team-voice content exists. Nil in the OSS
	// build: every summary uses summarySystemPrompt regardless of scope,
	// which is also simply correct for a deployment with only private
	// scopes — this hook is never consulted there.
	TeamPromptOverride func(scope identity.Scope) (prompt string, ok bool)
}

// teamPromptProvider is nil in the OSS build — set by team.go's init()
// when the Tier-3 extension is present. New wires whatever this
// currently is into every Runner it constructs, so callers never need
// their own nil-check or wiring.
var teamPromptProvider func(scope identity.Scope) (prompt string, ok bool)

func New(db *sql.DB, keys *crypto.KeyStore, consolidation, grounding, embedder provider.Provider) *Runner {
	return &Runner{
		db: db, keys: keys, consolidation: consolidation, grounding: grounding, embedder: embedder,
		TeamPromptOverride: teamPromptProvider,
	}
}

// systemActor is the audit_log actor for anything this package writes on
// its own initiative (RunDaily, RunRollup) rather than at a human
// operator's explicit request (Correct) — docs/GAP_CLOSURE_PLAN.md §4.3.
const systemActor = "system:consolidation"

// RunDaily summarizes one calendar day's interaction episodes, within the
// given scope, into a new draft daily summary. A day with no episodes in
// that scope is a no-op, not an error — most days for most
// users/teams will have gaps. Under Tier 3, this is called once per
// active scope (docs/TIER3_PLAN.md §5), not once globally.
func (r *Runner) RunDaily(ctx context.Context, scope identity.Scope, date time.Time) (err error) {
	start := time.Now()
	skippedNoEpisodes := false
	defer func() {
		metrics.ConsolidationDuration.Observe(time.Since(start).Seconds())
		switch {
		case err != nil:
			metrics.ConsolidationRunsTotal.WithLabelValues("error").Inc()
		case skippedNoEpisodes:
			metrics.ConsolidationRunsTotal.WithLabelValues("skipped_no_episodes").Inc()
		default:
			metrics.ConsolidationRunsTotal.WithLabelValues("ok").Inc()
		}
	}()

	period := date.Format("2006-01-02")

	var sources []textSource
	var existingCurrentID string
	var knownEntities []knownEntityContext
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		sources, err = r.loadDailyEpisodes(ctx, tx, scope, date)
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return nil
		}
		existingCurrentID, err = r.currentSummaryID(ctx, tx, scope, "daily", period)
		if err != nil {
			return err
		}
		// Phase C sub-problem 1 (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
		// a quick, read-only scan for existing entities this day's raw
		// text mentions, alongside the other quick reads this same
		// transaction already does — the consolidation LLM call itself
		// (a network call) still happens outside any transaction, below.
		combined := joinSources(sources)
		knownEntities, err = r.findKnownEntities(ctx, tx, scope, combined)
		return err
	})
	if err != nil {
		return fmt.Errorf("consolidation: load episodes for %s: %w", period, err)
	}
	if len(sources) == 0 {
		skippedNoEpisodes = true
		return nil
	}

	// See buildSummaryPrompt's doc comment: without this, regenerating
	// from raw episodes on a re-consolidation can see the user's original
	// raw statement and a later, already-corrected answer as conflicting
	// claims and walk the correction back, since nothing in the raw
	// episodes reveals that the correction was deliberate.
	var establishedRecord string
	if existingCurrentID != "" {
		if err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			establishedRecord, err = r.loadSummaryText(ctx, tx, scope, existingCurrentID)
			return err
		}); err != nil {
			return fmt.Errorf("consolidation: load established record %s for %s: %w", existingCurrentID, period, err)
		}
	}

	output, err := r.generateDailySummary(ctx, scope, period, sources, establishedRecord, knownEntities)
	if err != nil {
		return fmt.Errorf("consolidation: generate daily summary for %s: %w", period, err)
	}

	sourceIDs := make([]string, len(sources))
	for i, s := range sources {
		sourceIDs[i] = s.id
	}

	// A second RunDaily call for a day that already has a current draft
	// (a manual re-run, or a day that's still accumulating episodes when
	// a scheduler fires more than once) regenerates from *all* of that
	// day's episodes and supersedes the existing draft, rather than
	// inserting an unrelated duplicate that leaves two rows both claiming
	// to be "current" (see internal/store/retrieve.go's doc comment on
	// what that means) — found by actually re-running consolidation
	// against a day that had already been consolidated and watching
	// retrieval surface both the old and new drafts side by side. This is
	// a system-driven supersession, not a human correction — actor stays
	// systemActor, and the reason says so explicitly, so it stays
	// distinguishable from a real hupi-correct in the audit trail even
	// though it's the same supersedes/correction_reason mechanism.
	var correctionReason string
	if existingCurrentID != "" {
		correctionReason = "automatic re-consolidation, not a human correction: regenerated from the full day's episodes"
	}

	if err := r.storeSummary(ctx, storeSummaryInput{
		scope:               scope,
		level:               "daily",
		period:              period,
		sourceEpisodeIDs:    sourceIDs,
		output:              output,
		groundingSourceText: joinSources(sources),
		supersedes:          existingCurrentID,
		correctionReason:    correctionReason,
		actor:               systemActor,
	}); err != nil {
		return err
	}

	// Phase D item 4 (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): always
	// called, not just on a genuine re-consolidation — a real, confirmed
	// gap this used to have (docs/CODEBASE_SURVEY_AND_REVIEW.md finding
	// A7): gating this on existingCurrentID != "" assumed a day's
	// first-ever summary could never leave an existing rollup stale,
	// since "nothing existed to be stale before this day had a summary at
	// all." That reasoning misses a day backfilled out of order (a late
	// import, a manual `-date` re-run, episodes captured late) *after*
	// its week's rollup already ran once with this day as a real gap —
	// weeklyRollup (cmd/hupi-consolidate/rollup.go) always writes the
	// full 7-day calendar template into source_summary_periods
	// regardless of which days had a summary at the time, so this day's
	// period is already listed on that existing rollup even though it
	// contributed nothing to it yet. refreshRollupsCovering's own query
	// finds exactly that row via source_summary_periods, and
	// rollupIsStale correctly flags it stale (this summary's created_at
	// postdates the rollup's) — the only thing stopping that from ever
	// running was this guard. refreshRollupsCovering is cheap to call
	// unconditionally: its own query finds zero rows, and does nothing
	// further, whenever no rollup actually covers this period yet.
	r.refreshRollupsCovering(ctx, scope, "daily", period)

	// Phase C sub-problem 2 (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
	// best-effort, after the day's own summary is durably stored — see
	// checkCrossPeriodContradictions' own doc comment for why a failure
	// here must never fail RunDaily itself. Needs the *canonical* entity
	// ids storeSummary just wrote (not output.EntitiesTouched's raw,
	// pre-canonicalization ones), so this re-reads the row it just
	// stored rather than trying to thread canonicalization's result back
	// out of storeSummary.
	var newSummaryID string
	var entitiesTouchedLit string
	if err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select id, entities_touched from summaries
			where level = 'daily' and period = $1 and scope_kind = $2 and scope_owner = $3
			  and not exists (select 1 from summaries newer where newer.supersedes = summaries.id)
		`, period, scope.Kind, scope.Owner).Scan(&newSummaryID, &entitiesTouchedLit)
	}); err != nil {
		slog.Warn("consolidation: reload stored summary for contradiction check failed", "period", period, "error", err)
	} else {
		r.checkCrossPeriodContradictions(ctx, scope, newSummaryID, period, pgfmt.ParseTextArray(entitiesTouchedLit))
	}

	// Runs after the summary is durably stored, not before: a failure
	// here shouldn't block the summary itself, since summaries are the
	// primary retrieval surface and this is a supplementary one (see
	// docs/DESIGN_VS_BUILT.md #3). This doc comment described the
	// intended behavior correctly but the code didn't deliver it — a
	// real, confirmed bug (docs/CODEBASE_SURVEY_AND_REVIEW.md finding
	// A6): returning the error here propagated all the way to
	// cmd/hupi-consolidate's scope loop, registering a transient
	// embedding-provider hiccup as a failed consolidation run even
	// though the summary, its key facts, and every entity were already
	// committed, and — because RunDaily returned immediately — silently
	// skipped the entity-embedding backfill below for that scope/day
	// too. Logging and continuing, the same pattern this package already
	// uses for checkCrossPeriodContradictions just above, fixes both.
	if err := r.embedHighImportanceEpisodes(ctx, scope, date); err != nil {
		slog.Warn("consolidation: embed high-importance episodes failed", "period", period, "error", err)
	}

	// One-time backfill target for entities that existed before
	// schema/0012_entity_embeddings.sql added the column, plus a safety
	// net for any entity a prior storeSummary's embedEntities call failed
	// to embed. Piggybacks on "this scope had activity today" the same
	// way embedHighImportanceEpisodes does, rather than needing its own
	// schedule — an entity that's genuinely never touched again stays
	// substring-matchable only, which is the same as today, not a
	// regression. Same best-effort fix as above: finding or embedding the
	// backfill set is supplementary, not something a transient failure
	// should register as a failed consolidation run over.
	missingIDs, err := r.entitiesMissingEmbeddings(ctx, scope)
	if err != nil {
		slog.Warn("consolidation: find entities missing embeddings failed", "period", period, "error", err)
		return nil
	}
	if err := r.embedEntities(ctx, scope, missingIDs); err != nil {
		slog.Warn("consolidation: backfill entity embeddings failed", "period", period, "error", err)
	}

	// Keeps scope_corpus_size current for internal/store/retrieve.go's
	// keywordSearchTierForScope — best-effort and last, same posture as
	// embedHighImportanceEpisodes/entitiesMissingEmbeddings above: a
	// transient failure here shouldn't register as a failed consolidation
	// run, since this scope's summary is already durably stored. Only
	// reached when len(sources) > 0 (the early return above), matching
	// the plan's reasoning that a day with no new episodes doesn't change
	// a scope's corpus size.
	if err := r.updateScopeCorpusSize(ctx, scope); err != nil {
		slog.Warn("consolidation: update scope corpus size failed", "period", period, "error", err)
	}
	return nil
}

// updateScopeCorpusSize records scope's current episode/summary counts into
// scope_corpus_size, the signal internal/store/retrieve.go's
// keywordSearchTierForScope reads to decide how much a retrieval call lets
// BM25 keyword search cost for this scope — see that table's own migration
// (schema/0022_scope_corpus_size.sql) for why raw counts, not a precomputed
// tier. Counts current summaries only (not exists a newer one that
// supersedes it), matching every other "current" query in this package.
func (r *Runner) updateScopeCorpusSize(ctx context.Context, scope identity.Scope) error {
	var episodeCount, summaryCount int
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			select count(*) from episodes
			where type = 'interaction' and scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&episodeCount); err != nil {
			return fmt.Errorf("count episodes: %w", err)
		}
		if err := tx.QueryRowContext(ctx, `
			select count(*) from summaries s
			where scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		`, scope.Kind, scope.Owner).Scan(&summaryCount); err != nil {
			return fmt.Errorf("count summaries: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			insert into scope_corpus_size (scope_kind, scope_owner, episode_count, summary_count, updated_at)
			values ($1, $2, $3, $4, now())
			on conflict (scope_kind, scope_owner) do update
				set episode_count = excluded.episode_count,
				    summary_count = excluded.summary_count,
				    updated_at = excluded.updated_at
		`, scope.Kind, scope.Owner, episodeCount, summaryCount)
		if err != nil {
			return fmt.Errorf("upsert scope corpus size: %w", err)
		}
		return nil
	})
	return err
}

// entitiesMissingEmbeddings returns every entity in scope that doesn't
// have a *current* embedding — never embedded at all, or embedded under
// a different model than the one configured right now (see RunDaily's
// backfill call above, and schema/0013_embedding_model_tracking.sql for
// why a stale-model embedding needs the same treatment as a missing one:
// comparing vectors from two different models is meaningless, and this
// query is what lets an entity touched by today's consolidation recover
// from a provider switch without waiting for a full hupi-reembed run).
func (r *Runner) entitiesMissingEmbeddings(ctx context.Context, scope identity.Scope) ([]string, error) {
	currentModel := EmbedderIdentity(r.embedder)
	var ids []string
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id from entities
			where (embedding is null or embedding_model is distinct from $1)
			  and scope_kind = $2 and scope_owner = $3
		`, currentModel, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	return ids, err
}

// Correct writes a new version of an existing summary that supersedes it —
// MEMORY_FORMAT.md § Grounding & correction: never edit a summary in
// place, always write a new version with `supersedes` and
// `correction_reason` set, so the wrong version and *why* it was wrong
// both stay in the bundle. output is typically hand-authored by whoever
// spotted the error, not regenerated by the same LLM that got it wrong —
// this re-runs the grounding check against the original source material
// regardless, since a human correction can be wrong too. scope must match
// the scope the original summary was written in — Correct never moves a
// summary from one scope to another. actor is recorded on the audit_log
// entry (docs/GAP_CLOSURE_PLAN.md §4.3) — cmd/hupi-correct's -actor flag,
// not derived from anything in this package, since a correction is always
// a deliberate human action.
//
// extraGroundingSourceText, when non-empty, is appended to the source
// material a fact gets checked against — normally empty (a human
// correction via cmd/hupi-correct is grounded in the summary's own
// original sources, nothing else). checkOneRelatedSummary
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase C sub-problem 2) is the
// one caller that needs this: a cross-period correction's replacement
// fact is, by construction, actually grounded in the *triggering*
// period's own sources, not oldSummaryID's — without this, groundingCheck
// only ever sees the old summary's sources and the real, correct
// replacement fact comes back ungrounded every time, real-verified via
// live testing.
func (r *Runner) Correct(ctx context.Context, scope identity.Scope, oldSummaryID string, output ConsolidationOutput, reason, actor, extraGroundingSourceText string) error {
	if reason == "" {
		return fmt.Errorf("consolidation: correction_reason is required to correct %s", oldSummaryID)
	}

	var level, period, sourceEpisodeIDsLit, sourceSummaryPeriodsLit string
	var supersededBy sql.NullString
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		if scanErr := tx.QueryRowContext(ctx, `
			select level, period, source_episode_ids, source_summary_periods
			from summaries where id = $1 and scope_kind = $2 and scope_owner = $3
		`, oldSummaryID, scope.Kind, scope.Owner).Scan(&level, &period, &sourceEpisodeIDsLit, &sourceSummaryPeriodsLit); scanErr != nil {
			return fmt.Errorf("load summary: %w", scanErr)
		}
		// A correction must always target the *current* version — see
		// vectorSearchSummaries's doc comment in internal/store/retrieve.go
		// for what "current" means. Without this check, correcting a
		// summary ID that some other correction already superseded creates
		// two divergent rows both claiming to be current (both have
		// nothing superseding them), and retrieval has no principled way
		// to pick between them. Found by deliberately reproducing it: an
		// operator pointing -summary-id at an already-superseded id
		// silently forked the history instead of erroring.
		scanErr := tx.QueryRowContext(ctx, `
			select id from summaries
			where supersedes = $1 and scope_kind = $2 and scope_owner = $3
		`, oldSummaryID, scope.Kind, scope.Owner).Scan(&supersededBy)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return fmt.Errorf("check for existing supersession: %w", scanErr)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("consolidation: load summary %s to correct: %w", oldSummaryID, err)
	}
	if supersededBy.Valid {
		return fmt.Errorf("consolidation: %s is already superseded by %s — correct that version instead", oldSummaryID, supersededBy.String)
	}

	sourceEpisodeIDs := pgfmt.ParseTextArray(sourceEpisodeIDsLit)
	sourceSummaryPeriods := pgfmt.ParseTextArray(sourceSummaryPeriodsLit)

	var sources []textSource
	if level == "daily" {
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			sources, err = r.loadEpisodesByID(ctx, tx, scope, sourceEpisodeIDs)
			return err
		})
		if err != nil {
			return fmt.Errorf("consolidation: reload source episodes for correction: %w", err)
		}
	} else {
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			sources, err = r.loadSummaries(ctx, tx, scope, sourceLevelBelow(level), sourceSummaryPeriods)
			return err
		})
		if err != nil {
			return fmt.Errorf("consolidation: reload source summaries for correction: %w", err)
		}
	}
	groundingSourceText := joinSources(sources)
	if extraGroundingSourceText != "" {
		groundingSourceText += "\n\n" + extraGroundingSourceText
	}

	return r.storeSummary(ctx, storeSummaryInput{
		scope:                scope,
		level:                level,
		period:               period,
		sourceEpisodeIDs:     sourceEpisodeIDs,
		sourceSummaryPeriods: sourceSummaryPeriods,
		output:               output,
		groundingSourceText:  groundingSourceText,
		supersedes:           oldSummaryID,
		replaceEntityAttrs:   true,
		correctionReason:     reason,
		actor:                actor,
	})
}

// CurrentContent loads summaryID's current decrypted content — its prose,
// key facts, and the current attributes of every entity it lists as
// touched — shaped as a ConsolidationOutput ready to hand-edit and feed
// back into Correct via cmd/hupi-correct's -content. This exists because
// Correct replaces a touched entity's attributes wholesale rather than
// merging (see upsertEntities' doc comment on why): a correction authored
// from a blank slate silently drops every attribute the author didn't
// think to restate, discovered by doing exactly that in manual testing.
// Starting from this instead means editing only the one fact that
// actually changed. Entities the summary lists in entities_touched that
// have since been deleted are skipped, not an error — there's nothing
// current left to dump for them.
func (r *Runner) CurrentContent(ctx context.Context, scope identity.Scope, summaryID string) (ConsolidationOutput, error) {
	var summaryCT []byte
	var keyVersion int
	var entityIDsLit string
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select summary, key_version, entities_touched from summaries
			where id = $1 and scope_kind = $2 and scope_owner = $3
		`, summaryID, scope.Kind, scope.Owner).Scan(&summaryCT, &keyVersion, &entityIDsLit)
	})
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("load summary %s: %w", summaryID, err)
	}

	enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("resolve encryption key for summary %s: %w", summaryID, err)
	}
	summaryText, err := enc.Decrypt(summaryCT)
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("decrypt summary %s: %w", summaryID, err)
	}

	var keyFacts []KeyFactOutput
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, queryErr := tx.QueryContext(ctx, `
			select fact, source_episode_ids from summary_key_facts where summary_id = $1
		`, summaryID)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var factCT []byte
			var sourceIDsLit string
			if scanErr := rows.Scan(&factCT, &sourceIDsLit); scanErr != nil {
				return scanErr
			}
			factText, decErr := enc.Decrypt(factCT)
			if decErr != nil {
				return fmt.Errorf("decrypt key fact: %w", decErr)
			}
			keyFacts = append(keyFacts, KeyFactOutput{Fact: factText, SourceEpisodeIDs: pgfmt.ParseTextArray(sourceIDsLit)})
		}
		return rows.Err()
	})
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("load key facts for summary %s: %w", summaryID, err)
	}

	entityIDs := pgfmt.ParseTextArray(entityIDsLit)
	entities := make([]EntityUpdate, 0, len(entityIDs))
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		for _, id := range entityIDs {
			var kind, name string
			var attrsCT []byte
			var entKeyVersion int
			scanErr := tx.QueryRowContext(ctx, `
				select kind, name, attributes, key_version from entities
				where id = $1 and scope_kind = $2 and scope_owner = $3
			`, id, scope.Kind, scope.Owner).Scan(&kind, &name, &attrsCT, &entKeyVersion)
			if errors.Is(scanErr, sql.ErrNoRows) {
				continue
			}
			if scanErr != nil {
				return fmt.Errorf("load entity %s: %w", id, scanErr)
			}
			entEnc, keyErr := r.keys.GetVersion(ctx, scope, entKeyVersion)
			if keyErr != nil {
				return fmt.Errorf("resolve encryption key for entity %s: %w", id, keyErr)
			}
			attrsJSON, decErr := entEnc.Decrypt(attrsCT)
			if decErr != nil {
				return fmt.Errorf("decrypt attributes for entity %s: %w", id, decErr)
			}
			var attrs map[string]string
			if attrsJSON != "" {
				if jsonErr := json.Unmarshal([]byte(attrsJSON), &attrs); jsonErr != nil {
					return fmt.Errorf("parse attributes for entity %s: %w", id, jsonErr)
				}
			}
			entities = append(entities, EntityUpdate{ID: id, Kind: kind, Name: name, Attributes: attrs})
		}
		return nil
	})
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("load touched entities for summary %s: %w", summaryID, err)
	}

	return ConsolidationOutput{Summary: summaryText, KeyFacts: keyFacts, EntitiesTouched: entities}, nil
}

// sourceLevelBelow maps a rollup level to the level directly beneath it in
// the daily->weekly->monthly->yearly hierarchy. A summary only records
// which *periods* it was built from (source_summary_periods), not which
// level those periods are at — they're always exactly one level down by
// construction, so Correct needs this to reload the right source rows.
func sourceLevelBelow(level string) string {
	switch level {
	case "weekly":
		return "daily"
	case "monthly":
		return "weekly"
	case "yearly":
		return "monthly"
	default:
		return ""
	}
}

// EpisodeEmbedImportanceThreshold gates which episodes are worth embedding
// individually — most content is only ever meant to be reachable via the
// daily summary that folds it in; this is a supplementary path for
// anything importance-scored high enough to be worth finding directly
// (docs/DESIGN_VS_BUILT.md #3).
const EpisodeEmbedImportanceThreshold = 0.6

// embedHighImportanceEpisodes is the write side of ARCHITECTURE.md's
// "vector search over summaries + high-importance episodes": embeds any of
// the day's episodes (within scope) clearing EpisodeEmbedImportanceThreshold
// that don't have a *current* embedding — never embedded, or embedded under
// a since-changed provider (see entitiesMissingEmbeddings and
// schema/0013_embedding_model_tracking.sql for why that's treated the same
// as missing) — so internal/store.Retrieve's vectorSearchEpisodes has
// something to find. Embedding never happens inside Capture itself — that
// would reintroduce a network call into the capture hot path, exactly what
// Capture was redesigned to avoid.
//
// The candidate read and each write are separate short transactions
// (docs/HARDENING_PLAN.md D3): the embed call between them is a network
// round trip to the embedding provider, and it happens once per candidate
// in the loop below — holding one transaction across all of them would
// mean holding it open for as long as the slowest of N network calls.
func (r *Runner) embedHighImportanceEpisodes(ctx context.Context, scope identity.Scope, date time.Time) error {
	start := date.Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)
	currentModel := EmbedderIdentity(r.embedder)

	var sources []textSource
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, input_text, output_text, key_version, ts from episodes
			where type = 'interaction' and ts >= $1 and ts < $2
			  and importance >= $3
			  and (embedding is null or embedding_model is distinct from $4)
			  and scope_kind = $5 and scope_owner = $6
		`, start, end, EpisodeEmbedImportanceThreshold, currentModel, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		sources, err = r.scanEpisodeSources(ctx, rows, scope)
		return err
	})
	if err != nil {
		return err
	}

	for _, s := range sources {
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(s.text)}})
		if err != nil {
			return fmt.Errorf("embed episode %s: %w", s.id, err)
		}
		if len(resp.Vectors) == 0 {
			return fmt.Errorf("embedder returned no vectors for episode %s", s.id)
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `update episodes set embedding = $1::vector, embedding_model = $2 where id = $3`, vectorLiteral, currentModel, s.id)
			return err
		})
		if err != nil {
			return fmt.Errorf("write embedding for episode %s: %w", s.id, err)
		}
	}
	return nil
}

// RunRollup summarizes summaries one level down (e.g. seven daily
// summaries into one weekly), within scope, rather than raw episodes —
// kept O(number of summaries), not O(raw logs), per ARCHITECTURE.md's
// consolidation engine step 3. Computing which periods belong in a given
// week/month/year (calendar boundaries, ISO week numbers) is the caller's
// job, not the Runner's — this only needs an already-decided list.
//
// A second call for a period that already has a current, non-stale
// rollup is a no-op, not a second undifferentiated draft — this matters
// once a cron scheduler is calling this (docs/GAP_CLOSURE_PLAN.md §4.1),
// since a scheduler that isn't safe to double-fire isn't a scheduler.
// "Non-stale" (not simply "already exists") is Phase D item 4's real
// fix (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a source period's own
// current summary can legitimately change after this rollup was already
// generated (RunDaily's own same-day re-consolidation; Phase C
// correcting a contradiction), and the original write-once behavior had
// no way to notice that happened, ever — nothing in the natural cron
// cadence (dueRollups only revisits a given calendar period once) would
// call RunRollup for that period again on its own. rollupIsStale checks
// for exactly this; when stale, this regenerates and supersedes the old
// rollup the same way RunDaily regenerates and supersedes an existing
// day's draft. After a successful store (new or regenerated), this
// cascades upward via refreshRollupsCovering — a refreshed weekly rollup
// can itself make an already-existing monthly rollup stale, and so on up
// to yearly — so callers only ever need to trigger this once, at the
// level that actually changed.
func (r *Runner) RunRollup(ctx context.Context, scope identity.Scope, level, sourceLevel, period string, sourcePeriods []string) error {
	var sources []textSource
	var existingCurrentID string
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		existingCurrentID, err = r.currentSummaryID(ctx, tx, scope, level, period)
		if err != nil {
			return err
		}
		if existingCurrentID != "" {
			stale, err := r.rollupIsStale(ctx, tx, scope, existingCurrentID, sourceLevel, sourcePeriods)
			if err != nil || !stale {
				return err
			}
		}
		sources, err = r.loadSummaries(ctx, tx, scope, sourceLevel, sourcePeriods)
		return err
	})
	if err != nil {
		return fmt.Errorf("consolidation: load %s summaries for %s %s: %w", sourceLevel, level, period, err)
	}
	if len(sources) == 0 {
		return nil
	}

	// No establishedRecord: a rollup regenerates wholesale from its
	// current sources every time, unlike RunDaily's own draft, which
	// accumulates episodes incrementally within one day — there's no
	// meaningful "partial rollup" to preserve continuity with. No
	// knownEntities either — Phase C sub-problem 1 is scoped to raw
	// daily episode text for now (see generateSummary's own doc
	// comment).
	output, err := r.generateSummary(ctx, scope, level, period, sources, "", nil)
	if err != nil {
		return fmt.Errorf("consolidation: generate %s summary for %s: %w", level, period, err)
	}

	var correctionReason string
	if existingCurrentID != "" {
		correctionReason = "automatic re-consolidation, not a human correction: a source period changed since this rollup was last generated"
	}
	if err := r.storeSummary(ctx, storeSummaryInput{
		scope:                scope,
		level:                level,
		period:               period,
		sourceSummaryPeriods: sourcePeriods,
		output:               output,
		groundingSourceText:  joinSources(sources),
		supersedes:           existingCurrentID,
		correctionReason:     correctionReason,
		actor:                systemActor,
	}); err != nil {
		return err
	}

	r.refreshRollupsCovering(ctx, scope, level, period)
	return nil
}

// rollupIsStale reports whether any of a rollup's own source periods has
// a *current* summary created after the rollup itself was — see
// RunRollup's own doc comment for why this needs checking at all.
func (r *Runner) rollupIsStale(ctx context.Context, q dbscope.Querier, scope identity.Scope, rollupID, sourceLevel string, sourcePeriods []string) (bool, error) {
	var rollupCreatedAt time.Time
	if err := q.QueryRowContext(ctx, `select created_at from summaries where id = $1`, rollupID).Scan(&rollupCreatedAt); err != nil {
		return false, fmt.Errorf("load rollup %s created_at: %w", rollupID, err)
	}
	var newestSourceCreatedAt sql.NullTime
	if err := q.QueryRowContext(ctx, `
		select max(created_at) from summaries
		where level = $1 and period = any($2::text[]) and supersedes is null
		  and scope_kind = $3 and scope_owner = $4
	`, sourceLevel, pgfmt.TextArray(sourcePeriods), scope.Kind, scope.Owner).Scan(&newestSourceCreatedAt); err != nil {
		return false, fmt.Errorf("load newest source created_at for rollup %s: %w", rollupID, err)
	}
	return newestSourceCreatedAt.Valid && newestSourceCreatedAt.Time.After(rollupCreatedAt), nil
}

// rollupLevelAbove is sourceLevelBelow's inverse — the level whose
// rollup would summarize correctedLevel, or "" for yearly (nothing rolls
// up further).
func rollupLevelAbove(level string) string {
	switch level {
	case "daily":
		return "weekly"
	case "weekly":
		return "monthly"
	case "monthly":
		return "yearly"
	default:
		return ""
	}
}

// refreshRollupsCovering re-invokes RunRollup for every already-existing,
// current rollup that summarizes correctedPeriod at correctedLevel —
// Phase D item 4's real trigger (docs/CONSOLIDATION_COMPLETENESS_PLAN.md):
// nothing in the natural cron cadence ever revisits a past calendar
// period on its own, so a correction to one period needs to explicitly
// prod whatever already-generated rollups cover it. Best-effort, like
// checkCrossPeriodContradictions — logged, never propagated, so a
// problem refreshing a rollup can't fail the correction that triggered
// it. RunRollup itself calls this again after a successful store. so one
// call here cascades upward through weekly -> monthly -> yearly as far
// as real, already-existing rollups go.
func (r *Runner) refreshRollupsCovering(ctx context.Context, scope identity.Scope, correctedLevel, correctedPeriod string) {
	rollupLevel := rollupLevelAbove(correctedLevel)
	if rollupLevel == "" {
		return
	}

	type coveringRollup struct {
		period        string
		sourcePeriods []string
	}
	var rollups []coveringRollup
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select period, source_summary_periods from summaries
			where level = $1 and $2 = any(source_summary_periods) and supersedes is null
			  and scope_kind = $3 and scope_owner = $4
		`, rollupLevel, correctedPeriod, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var period, sourcePeriodsLit string
			if err := rows.Scan(&period, &sourcePeriodsLit); err != nil {
				return err
			}
			rollups = append(rollups, coveringRollup{period: period, sourcePeriods: pgfmt.ParseTextArray(sourcePeriodsLit)})
		}
		return rows.Err()
	})
	if err != nil {
		slog.Warn("consolidation: find rollups covering corrected period failed", "corrected_level", correctedLevel, "corrected_period", correctedPeriod, "error", err)
		return
	}

	for _, ru := range rollups {
		if err := r.RunRollup(ctx, scope, rollupLevel, correctedLevel, ru.period, ru.sourcePeriods); err != nil {
			slog.Warn("consolidation: refresh rollup after correction failed", "rollup_level", rollupLevel, "rollup_period", ru.period, "error", err)
		}
	}
}

func (r *Runner) loadDailyEpisodes(ctx context.Context, q dbscope.Querier, scope identity.Scope, date time.Time) ([]textSource, error) {
	start := date.Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)

	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version, ts from episodes
		where type = 'interaction' and ts >= $1 and ts < $2
		  and scope_kind = $3 and scope_owner = $4
		order by ts asc
	`, start, end, scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return r.scanEpisodeSources(ctx, rows, scope)
}

// loadEpisodesByID reloads a specific, already-known set of episode ids —
// used by Correct to reconstruct the original grounding source text for a
// daily summary being corrected, as opposed to loadDailyEpisodes' date
// range query for a fresh consolidation run.
func (r *Runner) loadEpisodesByID(ctx context.Context, q dbscope.Querier, scope identity.Scope, ids []string) ([]textSource, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version, ts from episodes
		where id = any($1::text[]) and scope_kind = $2 and scope_owner = $3
		order by ts asc
	`, pgfmt.TextArray(ids), scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return r.scanEpisodeSources(ctx, rows, scope)
}

func (r *Runner) scanEpisodeSources(ctx context.Context, rows *sql.Rows, scope identity.Scope) ([]textSource, error) {
	var out []textSource
	for rows.Next() {
		var id string
		var inputCT, outputCT []byte
		var keyVersion int
		var ts time.Time
		if err := rows.Scan(&id, &inputCT, &outputCT, &keyVersion, &ts); err != nil {
			return nil, err
		}
		enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for episode %s: %w", id, err)
		}
		input, err := enc.Decrypt(inputCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt episode %s input_text: %w", id, err)
		}
		output, err := enc.Decrypt(outputCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt episode %s output_text: %w", id, err)
		}
		out = append(out, textSource{id: id, text: EpisodeEmbedText(input, output), date: ts.Format("2006-01-02")})
	}
	return out, rows.Err()
}

// EpisodeEmbedText renders one episode's turn as the single canonical
// string both a normal consolidation run embeds (embedHighImportanceEpisodes,
// via scanEpisodeSources above) and feeds to the consolidation LLM as one
// of a day's sources — exported so internal/reembed embeds exactly the
// same text a fresh consolidation run would have, rather than a
// second, driftable copy of this format string.
func EpisodeEmbedText(input, output string) string {
	return fmt.Sprintf("USER: %s\nASSISTANT: %s", input, output)
}

// currentSummaryID returns the id of the current version of scope's
// level+period summary — "current" meaning no other row's supersedes
// points at it (see internal/store/retrieve.go's doc comment on
// vectorSearchSummaries for why that's the definition and not "this row's
// own supersedes is null") — or "" if no summary exists yet for that
// level+period. RunDaily uses this to chain a re-consolidation onto the
// existing draft instead of leaving two rows both "current" for the same
// day. created_at desc as the tiebreak is only relevant for scope+period
// combinations that already had more than one simultaneously-"current"
// row before this fix existed; a single well-formed chain never needs it.
func (r *Runner) currentSummaryID(ctx context.Context, q dbscope.Querier, scope identity.Scope, level, period string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		select s.id from summaries s
		where s.level = $1 and s.period = $2 and s.scope_kind = $3 and s.scope_owner = $4
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		order by s.created_at desc, s.id desc
		limit 1
	`, level, period, scope.Kind, scope.Owner).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// loadSummaryText loads and decrypts a single summary's prose by id —
// used by RunDaily to give generateSummary the day's established record
// on a re-consolidation (see buildSummaryPrompt), and by CurrentContent
// for cmd/hupi-correct's -dump-template.
func (r *Runner) loadSummaryText(ctx context.Context, q dbscope.Querier, scope identity.Scope, id string) (string, error) {
	var summaryCT []byte
	var keyVersion int
	if err := q.QueryRowContext(ctx, `
		select summary, key_version from summaries where id = $1 and scope_kind = $2 and scope_owner = $3
	`, id, scope.Kind, scope.Owner).Scan(&summaryCT, &keyVersion); err != nil {
		return "", err
	}
	enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return "", fmt.Errorf("resolve encryption key: %w", err)
	}
	return enc.Decrypt(summaryCT)
}

// loadSummaries feeds rollups (weekly from daily, monthly from weekly, ...)
// — it must load the *current* version of each source period, not
// whichever version happens to have never been superseded by anything.
// "Current" is "no other row's supersedes points at this id", not "this
// row's own supersedes is null" — see the long comment on
// internal/store.vectorSearchSummaries for why those are different and
// how getting this backwards silently feeds a rolled-back/corrected
// version into every higher-level rollup.
func (r *Runner) loadSummaries(ctx context.Context, q dbscope.Querier, scope identity.Scope, level string, periods []string) ([]textSource, error) {
	rows, err := q.QueryContext(ctx, `
		select id, summary, key_version from summaries s
		where level = $1 and period = any($2::text[])
		  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
		  and scope_kind = $3 and scope_owner = $4
		order by period asc
	`, level, pgfmt.TextArray(periods), scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []textSource
	for rows.Next() {
		var id string
		var summaryCT []byte
		var keyVersion int
		if err := rows.Scan(&id, &summaryCT, &keyVersion); err != nil {
			return nil, err
		}
		enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for summary %s: %w", id, err)
		}
		text, err := enc.Decrypt(summaryCT)
		if err != nil {
			return nil, fmt.Errorf("decrypt summary %s: %w", id, err)
		}
		out = append(out, textSource{id: id, text: text})
	}
	return out, rows.Err()
}

// consolidationMaxTokens overrides the Anthropic provider's own default
// (4096, internal/provider/anthropic.go's toAnthropicRequest) explicitly,
// rather than leaving this call to fall back to it silently. Found for
// real, not guessed: a genuinely busy day (2026-10-02, 9 raw sources
// clustered into 3 topic groups) reproducibly truncated mid-JSON on one
// cluster's own summary+key_facts+entities_touched output two attempts in
// a row ("unexpected end of JSON input"), confirmed by re-running
// hupi-consolidate -date 2026-10-02 directly against production and
// seeing the identical failure both times — not a transient provider
// hiccup. 8192 is a reasoned, not measured, doubling of the prior
// default: generous enough for a single topic cluster's worth of output
// without being an arbitrary large guess. Re-measure if a busier day
// ever truncates against this new ceiling too.
const consolidationMaxTokens = 8192

// consolidationTemperature pins every consolidation-time extraction and
// verification LLM call (generateSummary, groundingCheckOneAttempt,
// extractPerEpisodeFacts, checkOneRelatedSummary's contradiction check)
// to a low, deterministic temperature instead of leaving Temperature nil
// and silently falling back to the provider's own default — 1.0 for both
// OpenAI's and Anthropic's chat completion APIs, a value tuned for
// creative variety, not for "did the source text state X." These calls
// are all the same kind of task: a faithfulness judgment against a fixed
// source text, which should get the same answer on the same input, not a
// stochastic sample.
//
// Found this was unset at all while investigating a real run-to-run
// non-determinism bug (docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md Part B):
// two independent from-scratch re-ingests of the identical LoCoMo
// conv-42 conversation, same prompt, same code, read the exact same
// explicit, unambiguous source sentence ("I won an international
// tournament yesterday!", session dated 2022-08-22) as a win in one run
// and a loss in the other — confirmed by reading conv-42's raw source
// text directly, not either run's own extraction, that the source itself
// is not ambiguous here. That made the prior Part B prompt-wording
// experiment's single-run before/after comparison statistically
// meaningless: with no temperature control, a single baseline run and a
// single changed-prompt run can't be told apart from two baseline runs
// against each other, and separate investigation sessions' own
// unmodified baselines of this same scope independently landed on
// "tournaments won: 2" and "tournaments won: 6" with no prompt change at
// all (docs/BENCHMARKS.md §12 and this doc's Part B section).
//
// 0.0 is not a literal guarantee of bit-identical output across runs —
// providers don't commit to deterministic sampling even at temperature
// 0 (token-level ties, server-side batching jitter, etc. can still
// differ) — but it removes the one source of variance actually under
// this codebase's control, and is the right default for an extraction
// task regardless of whether it fully closes this specific gap.
const consolidationTemperature = 0.0

// generateSummary picks a scope-appropriate system prompt (team-neutral
// voice for shared scope, per docs/TIER3_PLAN.md §5, via
// Runner.TeamPromptOverride) before calling the consolidation LLM.
// establishedRecord is non-empty only when RunDaily is re-consolidating a
// day that already has a current draft — see buildSummaryPrompt's doc
// comment for why that needs special handling. knownEntities is
// generateDailySummary's own findKnownEntities result (nil from
// RunRollup — Phase C sub-problem 1 is scoped to raw daily episode text
// for now, see docs/CONSOLIDATION_COMPLETENESS_PLAN.md).
func (r *Runner) generateSummary(ctx context.Context, scope identity.Scope, level, period string, sources []textSource, establishedRecord string, knownEntities []knownEntityContext) (ConsolidationOutput, error) {
	systemPrompt := summarySystemPrompt
	if r.TeamPromptOverride != nil {
		if p, ok := r.TeamPromptOverride(scope); ok {
			systemPrompt = p
		}
	}

	maxTokens := consolidationMaxTokens
	temperature := consolidationTemperature
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: systemPrompt},
			{Role: provider.RoleUser, Content: buildSummaryPrompt(level, period, sources, establishedRecord, knownEntities)},
		},
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
	}

	var lastErr error
	for attempt := 1; attempt <= consolidationParseRetries; attempt++ {
		resp, err := r.consolidation.ChatCompletion(ctx, req)
		if err != nil {
			return ConsolidationOutput{}, fmt.Errorf("consolidation LLM call: %w", err)
		}
		var out ConsolidationOutput
		if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
			lastErr = fmt.Errorf("parse consolidation output: %w", err)
			slog.Warn("consolidation: malformed JSON from consolidation LLM, retrying", "attempt", attempt, "error", err)
			continue
		}
		return out, nil
	}
	return ConsolidationOutput{}, lastErr
}

// joinSources mirrors buildSummaryPrompt's own date-label format
// (prompts.go) — the grounding checker needs the same date context the
// consolidation model got, or it has no way to judge a correctly
// resolved absolute date ("2023-05-07") as actually supported by a
// source that only literally says "yesterday".
func joinSources(sources []textSource) string {
	var out string
	for _, s := range sources {
		if s.date != "" {
			out += fmt.Sprintf("--- id: %s (date: %s) ---\n%s\n\n", s.id, s.date, s.text)
		} else {
			out += fmt.Sprintf("--- id: %s ---\n%s\n\n", s.id, s.text)
		}
	}
	return out
}
