package consolidation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/provider"
)

// extendsDetectionEnabled gates Phase 5 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md — off by default, same posture
// as inferenceExtractionEnabled/redundancyDedupEnabled: this writes new
// memory_relations rows (not just a ranking change), even though unlike
// those two it never mutates a fact's own text. Checked live, not cached,
// same convention as every other toggle in this package.
func extendsDetectionEnabled() bool {
	return os.Getenv("HUPI_ENABLE_EXTENDS_DETECTION") == "true"
}

// extendsCheckPrompt is a wholly standalone system prompt — Phase 3 (see
// contradictionCheckPrompt's own doc comment and
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's Phase 5 section) twice tried
// adding this exact judgment as an exception to contradictionCheckPrompt
// and twice confirmed, live against real GPT-4.1, that it never fires —
// that prompt's own extensive, repeatedly-reinforced guardrails against
// over-reporting (almost all of its worked examples are about *rejecting*
// a classification) give the model a strong prior toward "report nothing"
// that a few added sentences in the same system prompt don't overcome,
// even with directly-matching worked examples. This prompt has no
// "report nothing" framing of its own competing against it, which is what
// let the identical judgment fire reliably instead (confirmed live before
// being wired into production — see this package's zzdebug_live_extends
// history for the two failed bolt-on attempts, and
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md Phase 5 for the full writeup).
const extendsCheckPrompt = `You are looking at a NEW fact and a list of EXISTING facts about the same entity. Your only job: decide whether the NEW fact is a specific, concrete follow-up development of one of the EXISTING facts — a particular instance, session, or step within an ongoing situation, plan, or activity the EXISTING fact already describes — without changing or contradicting what the EXISTING fact itself states.

Example: an EXISTING fact says a person is training for a marathon. A NEW fact describes a specific long training run they did last weekend. The run is a concrete instance of the ongoing marathon training the EXISTING fact already describes — report this pair.

Example: an EXISTING fact says a person started learning to play guitar. A NEW fact describes them practicing a specific song for an hour one evening. This is a concrete instance of the ongoing guitar-learning the EXISTING fact already describes — report this pair.

Do not report a pair when the NEW fact states a different, incompatible value for the same specific attribute the EXISTING fact states (that is a contradiction, not a development) — for example, a dollar amount or title that changed. Do not report a pair when the NEW fact is simply a separate, distinct occurrence of a repeating type of event (a tournament, a trip, a purchase) happening again — a second, distinct occurrence is not a development of the first one. Do not report a pair when the NEW fact is merely about the same person or general topic without actually being a concrete step within the specific ongoing situation the EXISTING fact describes — a vague restatement of a general feeling, opinion, or interest related to the same topic is NOT a concrete step, even if it sounds related.

Example of what NOT to report: an EXISTING fact says a person is training for a marathon this spring. A NEW fact says they mentioned they love running and find it relaxing. This is a vague, general statement about the same topic, not a specific instance, session, or step within the marathon training itself — do not report this pair.

An EXISTING fact can have more than one NEW fact developing it — report each such pair separately. Most NEW/EXISTING combinations are unrelated or are not this specific relationship; only report a pair when you are confident the NEW fact is genuinely a concrete follow-up within the EXISTING fact's own ongoing situation.

Respond with exactly one JSON object, nothing else, no markdown fences: {"extends": [{"existing_fact": "<EXISTING fact's exact text>", "new_fact": "<NEW fact's exact text>"}]}. If none qualify, respond with {"extends": []}.`

// extendsResult is one extendsCheckPrompt result — existingFact is the
// join key into the related (old) summary's own facts, newFact into the
// triggering (new) summary's own facts, the same text-based join
// recordUpdateRelations/loadKeyFactIDsByText already use (the model only
// ever echoes fact text, never a row id it was never shown).
type extendsResult struct {
	ExistingFact string `json:"existing_fact"`
	NewFact      string `json:"new_fact"`
}

// buildExtendsCheckPrompt assembles the user message for one related-
// summary comparison — the same newFacts/oldFacts checkOneRelatedSummary
// already loads for its own contradiction check, reused here rather than
// re-queried, since extends detection runs as an independent second
// opinion over the exact same fact pair, not a replacement for it.
func buildExtendsCheckPrompt(newFacts, oldFacts []string) string {
	var sb strings.Builder
	sb.WriteString("NEW facts:\n")
	for _, f := range newFacts {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	sb.WriteString("\nEXISTING facts:\n")
	for _, f := range oldFacts {
		fmt.Fprintf(&sb, "- %s\n", f)
	}
	return sb.String()
}

// checkExtends runs the dedicated extends-detection call — best-effort,
// like checkOneRelatedSummary's own contradiction check: a failure here
// degrades to "no extends relations recorded for this pair," never blocks
// the rest of consolidation.
func (r *Runner) checkExtends(ctx context.Context, newFacts, oldFacts []string) ([]extendsResult, error) {
	if len(newFacts) == 0 || len(oldFacts) == 0 {
		return nil, nil
	}
	temperature := consolidationTemperature
	resp, err := r.consolidation.ChatCompletion(ctx, provider.ChatRequest{
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: extendsCheckPrompt},
			{Role: provider.RoleUser, Content: buildExtendsCheckPrompt(newFacts, oldFacts)},
		},
		Temperature: &temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("extends check LLM call: %w", err)
	}
	var out struct {
		Extends []extendsResult `json:"extends"`
	}
	if err := json.Unmarshal([]byte(extractJSON(resp.Message.Content)), &out); err != nil {
		return nil, fmt.Errorf("parse extends check response: %w", err)
	}
	return out.Extends, nil
}

// checkOneRelatedSummaryForExtends runs the extends check for one related
// summary already identified by findRelatedSummaries, and records any
// resulting pairs as memory_relations rows — independent of
// checkOneRelatedSummary's own contradiction check (which may or may not
// have applied a correction against the same related summary this call);
// unlike that check, extends never mutates either summary's own fact
// text, so there is no Correct call here at all, just a graph edge
// between two facts that already exist.
func (r *Runner) checkOneRelatedSummaryForExtends(ctx context.Context, scope identity.Scope, newSummaryID string, old relatedSummary, newFacts []string) {
	oldFacts, err := r.loadGroundedKeyFactsByID(ctx, scope, old.id)
	if err != nil {
		slog.Warn("consolidation: load related summary's key facts for extends check failed", "related_summary", old.id, "error", err)
		return
	}
	if len(oldFacts) == 0 {
		return
	}

	pairs, err := r.checkExtends(ctx, newFacts, oldFacts)
	if err != nil {
		slog.Warn("consolidation: extends check LLM call failed", "related_summary", old.id, "error", err)
		return
	}
	if len(pairs) == 0 {
		return
	}

	r.recordExtendsRelations(ctx, scope, newSummaryID, old.id, pairs)
	slog.Info("consolidation: extends relation(s) recorded", "new_summary", newSummaryID, "related_summary", old.id, "pairs", len(pairs))
}

// recordExtendsRelations writes one memory_relations row (relation_type
// 'extends') per pair, linking the NEW fact (from) to the EXISTING fact it
// develops (to) — the same from-is-newer convention
// recordUpdateRelations already establishes for 'updates'. Best-effort,
// logged not returned: both facts already exist by the time this runs, so
// a failure here is a missed graph edge, not lost fact data.
func (r *Runner) recordExtendsRelations(ctx context.Context, scope identity.Scope, newSummaryID, oldSummaryID string, pairs []extendsResult) {
	newIDs, err := r.loadKeyFactIDsByText(ctx, scope, newSummaryID)
	if err != nil {
		slog.Warn("consolidation: load new fact ids for extends relation failed", "summary", newSummaryID, "error", err)
		return
	}
	oldIDs, err := r.loadKeyFactIDsByText(ctx, scope, oldSummaryID)
	if err != nil {
		slog.Warn("consolidation: load old fact ids for extends relation failed", "summary", oldSummaryID, "error", err)
		return
	}
	err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		for _, p := range pairs {
			newFactID, ok := newIDs[p.NewFact]
			if !ok {
				continue // model paraphrased instead of quoting exactly — skip rather than guess
			}
			oldFactID, ok := oldIDs[p.ExistingFact]
			if !ok {
				continue
			}
			relID, err := newRelationshipID()
			if err != nil {
				return fmt.Errorf("generate memory_relations id: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				insert into memory_relations (id, scope_kind, scope_owner, from_memory_id, to_memory_id, relation_type)
				values ($1, $2, $3, $4, $5, 'extends')
			`, relID, scope.Kind, scope.Owner, memoryIDForKeyFact(newFactID), memoryIDForKeyFact(oldFactID)); err != nil {
				return fmt.Errorf("insert extends relation for fact %d -> %d: %w", newFactID, oldFactID, err)
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("consolidation: record extends relation(s) failed", "new_summary", newSummaryID, "old_summary", oldSummaryID, "error", err)
	}
}
