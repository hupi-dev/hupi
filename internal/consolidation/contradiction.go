package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// maxRelatedSummariesForContradictionCheck bounds real LLM-call cost:
// each related summary sharing a touched entity with today's new one
// gets its own contradiction-check call, so an entity touched across
// many historical periods could otherwise turn one day's consolidation
// into many extra calls. Checked most-recently-touched first — a
// contradiction is far more likely against a period reasonably close to
// today than one from long ago, and this bounds the worst case rather
// than trying to check every historical period an entity ever appeared
// in.
const maxRelatedSummariesForContradictionCheck = 5

// contradictionCheckPrompt is deliberately narrow and separate from
// summarySystemPrompt — Phase C sub-problem 2
// (docs/CONSOLIDATION_COMPLETENESS_PLAN.md): a focused yes/no judgment
// per related summary ("does a new fact replace an old one"), not full
// consolidation. The explicit "not merely relate to the same entity"
// instruction is the real guard against Category 1's own already-known
// false-positive shape (two different, both-true facts about the same
// person aren't a contradiction) — asking the model to distinguish
// "update" from "unrelated additional fact," the same judgment
// buildSummaryPrompt's established-record instruction already asks it
// to make for same-period continuity, just extended across periods.
const contradictionCheckPrompt = `You are checking whether any of a set of NEW facts contradicts any of a set of EXISTING facts about the same entities.

A contradiction means a NEW fact states a different, incompatible value for the exact same specific real-world attribute an EXISTING fact already states — for example, a mortgage pre-approval amount that changed, or a job title that changed. It is NOT a contradiction if a NEW fact is simply a different, additional fact about the same entity, or only relates to the same entity/topic without stating a conflicting value for the same specific thing. When in doubt, do not report it as a contradiction — a missed contradiction is far less costly than incorrectly discarding a fact that was actually still true.

Respond with exactly one JSON object, nothing else, no markdown fences:
{"contradictions": [{"old_fact": "<the EXISTING fact's exact text>", "replacement": "<the corrected fact text, or an empty string to remove the old fact with nothing to replace it>"}], "corrected_prose": "<the EXISTING prose paragraph, rewritten to reflect every contradiction above instead of the stale value it currently states — omit or leave empty if contradictions is empty>"}

The EXISTING prose paragraph is given below alongside the EXISTING facts. If you report any contradictions, corrected_prose must be a complete rewrite of that whole paragraph — not just the changed sentence — with every stale value replaced and everything else preserved as-is, since this will wholesale replace the paragraph a person or another system would read.

If there are no real contradictions, respond with {"contradictions": [], "corrected_prose": ""}.`

type contradictionResult struct {
	OldFact     string `json:"old_fact"`
	Replacement string `json:"replacement"`
}

type contradictionResponse struct {
	Contradictions []contradictionResult `json:"contradictions"`
	CorrectedProse string                `json:"corrected_prose"`
}

// buildContradictionCheckPrompt assembles the user message for one
// related-summary comparison: today's new facts against one other
// period's existing facts and prose, labeled with both periods and the
// entities they share so the model has the same context a human
// reviewer would use to judge "same fact updated" vs. "different fact,
// same entity," and enough of the existing prose to rewrite it in place
// rather than guessing at surrounding context it can't see.
func buildContradictionCheckPrompt(newPeriod string, newFacts []string, oldLevel, oldPeriod, oldProse string, oldFacts []string, sharedEntityNames []string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Entities in common: %s\n\n", strings.Join(sharedEntityNames, ", "))
	fmt.Fprintf(&sb, "NEW facts (from %s):\n", newPeriod)
	for _, f := range newFacts {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	fmt.Fprintf(&sb, "\nEXISTING prose (from %s %s, which may need rewriting):\n%s\n", oldLevel, oldPeriod, oldProse)
	fmt.Fprintf(&sb, "\nEXISTING facts (from the same %s %s, which may need updating):\n", oldLevel, oldPeriod)
	for _, f := range oldFacts {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	return sb.String()
}

// loadGroundedKeyFactsByID resolves a summary's own encryption key
// version and returns its grounded key facts — the same facts retrieval
// ever surfaces (loadKeyFacts' own grounded=true filter), so a
// contradiction check never flags something against an already-untrusted
// ungrounded fact, and never proposes replacing a fact with a change
// that could only ever affect what's actually retrievable.
func (r *Runner) loadGroundedKeyFactsByID(ctx context.Context, scope identity.Scope, summaryID string) ([]string, error) {
	var keyVersion int
	if err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select key_version from summaries where id = $1 and scope_kind = $2 and scope_owner = $3
		`, summaryID, scope.Kind, scope.Owner).Scan(&keyVersion)
	}); err != nil {
		return nil, fmt.Errorf("load key version for summary %s: %w", summaryID, err)
	}
	enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve encryption key for summary %s: %w", summaryID, err)
	}
	var facts []string
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select fact from summary_key_facts where summary_id = $1 and grounded = true order by id
		`, summaryID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var factCT []byte
			if err := rows.Scan(&factCT); err != nil {
				return err
			}
			fact, err := enc.Decrypt(factCT)
			if err != nil {
				return fmt.Errorf("decrypt key fact for summary %s: %w", summaryID, err)
			}
			facts = append(facts, fact)
		}
		return rows.Err()
	})
	return facts, err
}

// relatedSummary is one other current summary sharing a touched entity
// with today's new one.
type relatedSummary struct {
	id, level, period string
}

// findRelatedSummaries returns other current summaries (any level or
// period) that share at least one entity with entitiesTouched — real
// schema support already exists for this
// (summaries.entities_touched is a stored, queryable column; see
// docs/CONSOLIDATION_COMPLETENESS_PLAN.md Phase C sub-problem 2 for why
// this needed no migration), just never queried this way before.
func (r *Runner) findRelatedSummaries(ctx context.Context, scope identity.Scope, excludeID string, entitiesTouched []string) ([]relatedSummary, error) {
	var out []relatedSummary
	err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, level, period from summaries
			where entities_touched && $1::text[] and supersedes is null
			  and id != $2 and scope_kind = $3 and scope_owner = $4
			order by created_at desc
			limit $5
		`, pgfmt.TextArray(entitiesTouched), excludeID, scope.Kind, scope.Owner, maxRelatedSummariesForContradictionCheck)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s relatedSummary
			if err := rows.Scan(&s.id, &s.level, &s.period); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// checkCrossPeriodContradictions is RunDaily's post-storage step for
// Phase C sub-problem 2: a new day's summary may state a fact that
// contradicts a fact in a DIFFERENT period's still-current summary about
// the same entity — schema/0001_init.sql's supersedes mechanism (and
// internal/store/retrieve.go's current-summary filter) never actually
// constrained supersession to the same period, so this is real,
// structurally-supported behavior nothing exercised before this.
//
// Best-effort and deliberately silent on failure beyond a log line: a
// new day's own consolidation already succeeded by the time this runs,
// and a problem in this separate, supplementary check shouldn't be able
// to fail RunDaily as a whole (the same reasoning
// entitiesMissingEmbeddings' backfill call already establishes for
// itself).
func (r *Runner) checkCrossPeriodContradictions(ctx context.Context, scope identity.Scope, newSummaryID, newPeriod string, entitiesTouched []string) {
	if len(entitiesTouched) == 0 {
		return
	}

	related, err := r.findRelatedSummaries(ctx, scope, newSummaryID, entitiesTouched)
	if err != nil {
		slog.Warn("consolidation: find related summaries for contradiction check failed", "summary", newSummaryID, "error", err)
		return
	}
	if len(related) == 0 {
		return
	}

	newFacts, err := r.loadGroundedKeyFactsByID(ctx, scope, newSummaryID)
	if err != nil {
		slog.Warn("consolidation: load new summary's key facts for contradiction check failed", "summary", newSummaryID, "error", err)
		return
	}
	if len(newFacts) == 0 {
		return
	}

	for _, old := range related {
		r.checkOneRelatedSummary(ctx, scope, newSummaryID, newPeriod, newFacts, old, entitiesTouched)
	}
}

// checkOneRelatedSummary runs one contradiction-check LLM call against a
// single related summary and, if it identifies a real contradiction,
// applies the correction via Runner.Correct — CurrentContent already
// produces exactly the "full existing output, ready to hand-edit"
// starting point Correct needs (see CurrentContent's own doc comment),
// so this only has to surgically replace the contradicted fact(s) within
// that snapshot, leaving prose/entities_touched/relationships untouched.
func (r *Runner) checkOneRelatedSummary(ctx context.Context, scope identity.Scope, newSummaryID, newPeriod string, newFacts []string, old relatedSummary, sharedEntityNames []string) {
	oldFacts, err := r.loadGroundedKeyFactsByID(ctx, scope, old.id)
	if err != nil {
		slog.Warn("consolidation: load related summary's key facts for contradiction check failed", "related_summary", old.id, "error", err)
		return
	}
	if len(oldFacts) == 0 {
		return
	}

	// Loaded before the LLM call, not after: current.Summary is the old
	// prose the model needs to see in order to rewrite it (the second
	// follow-up gap this closes — see contradictionCheckPrompt's own doc
	// comment), and current.KeyFacts (the *full* fact list, unlike
	// oldFacts' grounded-only comparison set above) is what actually gets
	// edited and handed to Correct below.
	current, err := r.CurrentContent(ctx, scope, old.id)
	if err != nil {
		slog.Warn("consolidation: load current content for contradiction check failed", "related_summary", old.id, "error", err)
		return
	}

	req := provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: contradictionCheckPrompt},
			{Role: provider.RoleUser, Content: buildContradictionCheckPrompt(newPeriod, newFacts, old.level, old.period, current.Summary, oldFacts, sharedEntityNames)},
		},
	}
	resp, err := r.consolidation.ChatCompletion(ctx, req)
	if err != nil {
		slog.Warn("consolidation: contradiction check LLM call failed", "related_summary", old.id, "error", err)
		return
	}
	var parsed contradictionResponse
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &parsed); err != nil {
		slog.Warn("consolidation: malformed contradiction check response, skipping", "related_summary", old.id, "error", err)
		return
	}
	if len(parsed.Contradictions) == 0 {
		return
	}

	applied := 0
	for _, c := range parsed.Contradictions {
		idx := -1
		for i, f := range current.KeyFacts {
			if f.Fact == c.OldFact {
				idx = i
				break
			}
		}
		if idx == -1 {
			// The model paraphrased instead of quoting exactly, or named
			// a fact that already changed — skip rather than guess which
			// fact it meant.
			continue
		}
		if c.Replacement == "" {
			current.KeyFacts = append(current.KeyFacts[:idx], current.KeyFacts[idx+1:]...)
		} else {
			current.KeyFacts[idx].Fact = c.Replacement
		}
		applied++
	}
	if applied == 0 {
		return
	}
	if strings.TrimSpace(parsed.CorrectedProse) != "" {
		current.Summary = parsed.CorrectedProse
	}

	// The replacement fact(s) above are, by construction, grounded in
	// newSummaryID's own sources, not old.id's — without this, Correct's
	// own re-grounding check only ever sees old.id's original sources
	// and the real, correct replacement comes back ungrounded every
	// time, real-verified via live testing (see Correct's own doc
	// comment on extraGroundingSourceText).
	extraGrounding, err := r.loadGroundingSourceTextForSummary(ctx, scope, newSummaryID)
	if err != nil {
		slog.Warn("consolidation: load triggering period's sources for grounding failed, correcting without them", "related_summary", old.id, "new_summary", newSummaryID, "error", err)
	}

	reason := fmt.Sprintf("system-detected contradiction: a %s summary stated a different value for the same fact", newPeriod)
	if err := r.Correct(ctx, scope, old.id, current, reason, systemActor, extraGrounding); err != nil {
		slog.Warn("consolidation: applying contradiction correction failed", "related_summary", old.id, "error", err)
		return
	}
	slog.Info("consolidation: cross-period contradiction corrected", "corrected_summary", old.id, "triggering_period", newPeriod, "facts_replaced", applied, "prose_rewritten", strings.TrimSpace(parsed.CorrectedProse) != "")
}

// loadGroundingSourceTextForSummary rebuilds a summary's own grounding
// source text from its recorded sources — factored out of Correct so
// checkOneRelatedSummary can also supply a *different* summary's sources
// as Correct's extraGroundingSourceText (see that param's own doc
// comment for why).
func (r *Runner) loadGroundingSourceTextForSummary(ctx context.Context, scope identity.Scope, summaryID string) (string, error) {
	var level, sourceEpisodeIDsLit, sourceSummaryPeriodsLit string
	if err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			select level, source_episode_ids, source_summary_periods from summaries
			where id = $1 and scope_kind = $2 and scope_owner = $3
		`, summaryID, scope.Kind, scope.Owner).Scan(&level, &sourceEpisodeIDsLit, &sourceSummaryPeriodsLit)
	}); err != nil {
		return "", fmt.Errorf("load summary %s for grounding source: %w", summaryID, err)
	}

	var sources []textSource
	var err error
	if level == "daily" {
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			sources, err = r.loadEpisodesByID(ctx, tx, scope, pgfmt.ParseTextArray(sourceEpisodeIDsLit))
			return err
		})
	} else {
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			var err error
			sources, err = r.loadSummaries(ctx, tx, scope, sourceLevelBelow(level), pgfmt.ParseTextArray(sourceSummaryPeriodsLit))
			return err
		})
	}
	if err != nil {
		return "", fmt.Errorf("load sources for summary %s: %w", summaryID, err)
	}
	return joinSources(sources), nil
}
