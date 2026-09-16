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
	"time"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

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
}

func New(db *sql.DB, keys *crypto.KeyStore, consolidation, grounding, embedder provider.Provider) *Runner {
	return &Runner{db: db, keys: keys, consolidation: consolidation, grounding: grounding, embedder: embedder}
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
func (r *Runner) RunDaily(ctx context.Context, scope identity.Scope, date time.Time) error {
	period := date.Format("2006-01-02")

	var sources []textSource
	var existingCurrentID string
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		var err error
		sources, err = r.loadDailyEpisodes(ctx, tx, scope, date)
		if err != nil {
			return err
		}
		existingCurrentID, err = r.currentSummaryID(ctx, tx, scope, "daily", period)
		return err
	})
	if err != nil {
		return fmt.Errorf("consolidation: load episodes for %s: %w", period, err)
	}
	if len(sources) == 0 {
		return nil
	}

	output, err := r.generateSummary(ctx, scope, "daily", period, sources)
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

	// Runs after the summary is durably stored, not before: a failure
	// here shouldn't block the summary itself, since summaries are the
	// primary retrieval surface and this is a supplementary one (see
	// docs/DESIGN_VS_BUILT.md #3).
	if err := r.embedHighImportanceEpisodes(ctx, scope, date); err != nil {
		return fmt.Errorf("consolidation: embed high-importance episodes for %s: %w", period, err)
	}
	return nil
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
func (r *Runner) Correct(ctx context.Context, scope identity.Scope, oldSummaryID string, output ConsolidationOutput, reason, actor string) error {
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

// episodeEmbedImportanceThreshold gates which episodes are worth embedding
// individually — most content is only ever meant to be reachable via the
// daily summary that folds it in; this is a supplementary path for
// anything importance-scored high enough to be worth finding directly
// (docs/DESIGN_VS_BUILT.md #3).
const episodeEmbedImportanceThreshold = 0.6

// embedHighImportanceEpisodes is the write side of ARCHITECTURE.md's
// "vector search over summaries + high-importance episodes": embeds any of
// the day's episodes (within scope) clearing episodeEmbedImportanceThreshold
// that don't have an embedding yet, so internal/store.Retrieve's
// vectorSearchEpisodes has something to find. Embedding never happens
// inside Capture itself — that would reintroduce a network call into the
// capture hot path, exactly what Capture was redesigned to avoid.
//
// The candidate read and each write are separate short transactions
// (docs/HARDENING_PLAN.md D3): the embed call between them is a network
// round trip to the embedding provider, and it happens once per candidate
// in the loop below — holding one transaction across all of them would
// mean holding it open for as long as the slowest of N network calls.
func (r *Runner) embedHighImportanceEpisodes(ctx context.Context, scope identity.Scope, date time.Time) error {
	start := date.Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)

	var sources []textSource
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, input_text, output_text, key_version from episodes
			where type = 'interaction' and ts >= $1 and ts < $2
			  and importance >= $3 and embedding is null
			  and scope_kind = $4 and scope_owner = $5
		`, start, end, episodeEmbedImportanceThreshold, scope.Kind, scope.Owner)
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
		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{s.text}})
		if err != nil {
			return fmt.Errorf("embed episode %s: %w", s.id, err)
		}
		if len(resp.Vectors) == 0 {
			return fmt.Errorf("embedder returned no vectors for episode %s", s.id)
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `update episodes set embedding = $1::vector where id = $2`, vectorLiteral, s.id)
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
// Idempotent by level+period+scope: a second call for a period that
// already has a rollup is a no-op, not a second undifferentiated draft.
// This matters once a cron scheduler is calling this (docs/GAP_CLOSURE_PLAN.md
// §4.1) — RunDaily doesn't need the same guard because a duplicate daily
// draft is a pre-existing, unrelated gap, but a scheduler that isn't safe
// to double-fire isn't a scheduler.
func (r *Runner) RunRollup(ctx context.Context, scope identity.Scope, level, sourceLevel, period string, sourcePeriods []string) error {
	var sources []textSource
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		exists, err := r.summaryExists(ctx, tx, scope, level, period)
		if err != nil || exists {
			return err
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

	output, err := r.generateSummary(ctx, scope, level, period, sources)
	if err != nil {
		return fmt.Errorf("consolidation: generate %s summary for %s: %w", level, period, err)
	}

	return r.storeSummary(ctx, storeSummaryInput{
		scope:                scope,
		level:                level,
		period:               period,
		sourceSummaryPeriods: sourcePeriods,
		output:               output,
		groundingSourceText:  joinSources(sources),
		actor:                systemActor,
	})
}

func (r *Runner) loadDailyEpisodes(ctx context.Context, q dbscope.Querier, scope identity.Scope, date time.Time) ([]textSource, error) {
	start := date.Truncate(24 * time.Hour)
	end := start.Add(24 * time.Hour)

	rows, err := q.QueryContext(ctx, `
		select id, input_text, output_text, key_version from episodes
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
		select id, input_text, output_text, key_version from episodes
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
		if err := rows.Scan(&id, &inputCT, &outputCT, &keyVersion); err != nil {
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
		out = append(out, textSource{id: id, text: fmt.Sprintf("USER: %s\nASSISTANT: %s", input, output)})
	}
	return out, rows.Err()
}

// summaryExists reports whether scope already has any summary (any
// version, corrected or not) at level+period — RunRollup's idempotency
// guard: a rollup's sources are other summaries, already-finalized by the
// time it runs, so a second call for a period that already rolled up has
// nothing new to fold in and should be a pure no-op, not even a
// supersession. (RunDaily is different — see currentSummaryID — because a
// day's episodes can keep arriving between runs, so re-running it *does*
// have something new to fold in.)
func (r *Runner) summaryExists(ctx context.Context, q dbscope.Querier, scope identity.Scope, level, period string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `
		select exists(
			select 1 from summaries
			where level = $1 and period = $2 and scope_kind = $3 and scope_owner = $4
		)
	`, level, period, scope.Kind, scope.Owner).Scan(&exists)
	return exists, err
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

// generateSummary picks a scope-appropriate system prompt (team-neutral
// voice for shared scope, per docs/TIER3_PLAN.md §5) before calling the
// consolidation LLM.
func (r *Runner) generateSummary(ctx context.Context, scope identity.Scope, level, period string, sources []textSource) (ConsolidationOutput, error) {
	systemPrompt := summarySystemPrompt
	if scope.Kind == identity.ScopeKindShared {
		systemPrompt = teamSummarySystemPrompt
	}

	resp, err := r.consolidation.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: systemPrompt},
			{Role: provider.RoleUser, Content: buildSummaryPrompt(level, period, sources)},
		},
	})
	if err != nil {
		return ConsolidationOutput{}, fmt.Errorf("consolidation LLM call: %w", err)
	}

	var out ConsolidationOutput
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		return ConsolidationOutput{}, fmt.Errorf("parse consolidation output: %w", err)
	}
	return out, nil
}

func joinSources(sources []textSource) string {
	var out string
	for _, s := range sources {
		out += fmt.Sprintf("--- id: %s ---\n%s\n\n", s.id, s.text)
	}
	return out
}
