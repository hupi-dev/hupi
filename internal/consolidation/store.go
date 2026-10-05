package consolidation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
	"hupi/internal/provider"
)

// validEntityKinds mirrors schema/0001_init.sql's entities_kind_check
// constraint exactly — Go can't read a Postgres CHECK constraint at
// compile time, so this needs to be kept in sync by hand if that
// constraint's allowed values ever change. self_model deliberately isn't
// here even though it's in the DB constraint: it's a specially curated
// entity read directly by internal/store/retrieve.go's anchor logic, not
// something the consolidation LLM is ever prompted to invent on its own
// (see summarySystemPrompt's own kind enumeration, which omits it too).
var validEntityKinds = map[string]bool{
	"person": true, "project": true, "preference": true, "skill": true,
	"place": true, "organization": true,
}

type storeSummaryInput struct {
	scope                identity.Scope
	level                string
	period               string
	sourceEpisodeIDs     []string // daily only
	sourceSummaryPeriods []string // weekly/monthly/yearly only
	output               ConsolidationOutput
	groundingSourceText  string
	supersedes           string // id of the prior version this replaces — set by Runner.Correct (a human correction) or by RunDaily re-consolidating an already-drafted day (a system regeneration); which one is recorded in correctionReason and in the audit_log actor, not by this field alone
	correctionReason     string // required alongside supersedes for Runner.Correct (MEMORY_FORMAT.md § Grounding & correction); RunDaily sets a system-authored one so a re-consolidation stays distinguishable from a human correction in the record
	actor                string // audit_log actor — systemActor for RunDaily/RunRollup, the operator's -actor for Correct
	replaceEntityAttrs   bool   // true only for Runner.Correct — see upsertEntities' replace parameter doc comment for why a human correction replaces a touched entity's attributes wholesale instead of merging
}

// storeSummary runs the grounding check, then writes the summary and its
// key facts in one transaction, upserts touched entities, and embeds the
// summary text — in that order, matching ARCHITECTURE.md §§ Consolidation
// integrity safeguards and Consolidation engine. The grounding check is a
// network call to an LLM and deliberately runs before the transaction
// opens (docs/HARDENING_PLAN.md D3); everything from nextSummaryID's
// version lookup through the entity upserts shares one transaction so the
// version-counting query and the insert that relies on it are consistent.
// Returns the new summary's own id — Correct needs it (Phase 3 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md) to look up a just-written
// key fact's own memories row id when recording an "updates" relation.
func (r *Runner) storeSummary(ctx context.Context, in storeSummaryInput) (string, error) {
	grounded, err := r.groundingCheck(ctx, in.groundingSourceText, in.output.KeyFacts)
	if err != nil {
		return "", fmt.Errorf("grounding check: %w", err)
	}

	enc, keyVersion, err := r.keys.GetOrCreate(ctx, in.scope)
	if err != nil {
		return "", fmt.Errorf("resolve encryption key for %s:%s: %w", in.scope.Kind, in.scope.Owner, err)
	}

	summaryCT, err := enc.Encrypt(in.output.Summary)
	if err != nil {
		return "", fmt.Errorf("encrypt summary text: %w", err)
	}

	// Canonicalize each touched entity's id from kind+name rather than
	// trusting whatever id string the caller (the consolidation LLM, or a
	// hand-authored hupi-correct) happened to generate this time — see
	// canonicalEntityID's doc comment for why: the same real-world entity
	// otherwise fragments into multiple rows across separate runs. A
	// fresh slice, not an in-place edit of in.output.EntitiesTouched:
	// Correct's caller passed that ConsolidationOutput in and shouldn't
	// see its own value silently mutated.
	entities := make([]EntityUpdate, 0, len(in.output.EntitiesTouched))
	for _, e := range in.output.EntitiesTouched {
		if e.ID != "" {
			// A real, reproduced failure mode: the consolidation LLM
			// invents a plausible-sounding but unsupported kind (e.g.
			// "conversation", "family") outside the enum the prompt
			// actually asked for. Left unchecked, this reaches
			// upsertEntities' INSERT and fails with a raw Postgres
			// check-constraint violation — which (before this check)
			// aborted the whole day's consolidation over one bad entity,
			// discarding an otherwise-valid summary and every other
			// touched entity along with it. Skipping just this one entity
			// and logging it is the same "one bad thing shouldn't block
			// the rest" philosophy groundingCheck's degrade-not-fail
			// design and cmd/hupi-consolidate's per-scope loop already
			// follow.
			if !validEntityKinds[e.Kind] {
				slog.Warn("consolidation: skipping entity with invalid kind",
					"kind", e.Kind, "name", e.Name, "scope_kind", in.scope.Kind, "scope_owner", in.scope.Owner)
				continue
			}
			e.ID = canonicalEntityID(e.Kind, e.Name, e.ID)
		}
		entities = append(entities, e)
	}

	entityIDs := make([]string, 0, len(entities))
	for _, e := range entities {
		if e.ID != "" {
			entityIDs = append(entityIDs, e.ID)
		}
	}

	var id string
	var groundedFacts []keyFactToEmbed
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

		// validSourceEpisodeID backstops kf.SourceEpisodeIDs against
		// review finding B16: unlike upsertRelationships' own
		// entityExists check for subject_id/object_id, a key fact's
		// source_episode_ids was never validated at all before this fix
		// — a hallucinated id (the consolidation LLM citing something it
		// never actually saw) or a stale/typo'd one would be stored
		// permanently unchecked, since a text[] column can't carry a real
		// foreign key the way a scalar column can. in.sourceEpisodeIDs is
		// the exact, known-real set of episodes this call's own sources
		// came from (already loaded from the DB earlier in RunDaily/
		// Correct) — stricter than a bare existence check, since it also
		// catches a citation that names a real episode id that just
		// wasn't actually one of this summary's own sources, not only
		// ones that don't exist at all.
		validSourceEpisodeID := make(map[string]bool, len(in.sourceEpisodeIDs))
		for _, epID := range in.sourceEpisodeIDs {
			validSourceEpisodeID[epID] = true
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
				for _, epID := range kf.SourceEpisodeIDs {
					if validSourceEpisodeID[epID] {
						citeIDs = append(citeIDs, epID)
						continue
					}
					slog.Warn("consolidation: dropping key fact citation that isn't one of this summary's real source episodes",
						"episode_id", epID, "fact", kf.Fact, "scope_kind", in.scope.Kind, "scope_owner", in.scope.Owner)
				}
			}
			// Phase 1 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: native
			// fact expiration. parseOptionalDate is the same helper
			// relationships' valid_from/valid_until already use — real
			// SQL NULL for anything empty or unparseable, never a
			// fabricated date, same "one malformed field shouldn't fail
			// the whole fact" posture as that field's own validation.
			expiresAt := parseOptionalDate(kf.ExpiresAt)
			var expireReason any
			if expiresAt != nil && kf.ExpireReason != "" {
				expireReason = kf.ExpireReason
			}
			// Phase 2 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: every
			// ordinary, freshly-extracted fact (the consolidation LLM
			// never sets KeyFactOutput.SourceCount itself) gets the
			// schema's own default of 1, not a literal Go zero value —
			// only contradiction.go's redundant-handling branch (via a
			// dump-template-and-resubmit Correct cycle) ever sets this
			// above 1.
			sourceCount := kf.SourceCount
			if sourceCount <= 0 {
				sourceCount = 1
			}

			var factRowID int64
			err = tx.QueryRowContext(ctx, `
				insert into summary_key_facts (summary_id, fact, source_episode_ids, grounded, key_version, scope_kind, scope_owner, expires_at, expire_reason, source_count)
				values ($1, $2, $3::text[], $4, $5, $6, $7, $8::date, $9, $10)
				returning id
			`, id, factCT, pgfmt.TextArray(citeIDs), grounded[i], keyVersion, in.scope.Kind, in.scope.Owner, expiresAt, expireReason, sourceCount).Scan(&factRowID)
			if err != nil {
				return fmt.Errorf("insert key fact %d for summary %s: %w", i, id, err)
			}

			// Mirrored into the unified memories table under the same
			// deterministic id internal/backfillmemories.memoryIDForKeyFact
			// derives from a fact's own row id ("mem_fact_<id>", duplicated
			// here rather than imported — see internal/gateway/attribution.go's
			// extractJSON for this codebase's established precedent on a
			// small, single-line helper not justifying a cross-package
			// dependency). Citation construction (internal/store/retrieve.go)
			// needs a real RefKindMemory id for every fact it cites, not just
			// ones an operator has gotten around to backfilling — writing
			// this alongside summary_key_facts, under the same ciphertext
			// and key version (no re-encryption: it's the same plaintext,
			// not new content), keeps that id valid from the moment the
			// fact exists rather than only after a later batch job runs.
			// embedding/embedding_model are deliberately left unset here —
			// nothing queries memories by vector distance for a key fact
			// today (schema/0025's own doc comment), so embedding it a
			// second time would be pure cost with no reachable consumer.
			if _, err := tx.ExecContext(ctx, `
				insert into memories (id, scope_kind, scope_owner, summary_id, content, key_version, is_static, grounded, source_episode_ids, expires_at, expire_reason, source_count)
				values ($1, $2, $3, $4, $5, $6, false, $7, $8::text[], $9::date, $10, $11)
			`, memoryIDForKeyFact(factRowID), in.scope.Kind, in.scope.Owner, id, factCT, keyVersion, grounded[i], pgfmt.TextArray(citeIDs), expiresAt, expireReason, sourceCount); err != nil {
				return fmt.Errorf("mirror key fact %d for summary %s into memories: %w", i, id, err)
			}

			// Only grounded facts are ever retrieved (loadKeyFacts,
			// checkCrossPeriodContradictions), so only grounded facts need
			// an embedding — embedding an ungrounded fact would be pure
			// waste, never reachable by any query path.
			if grounded[i] {
				groundedFacts = append(groundedFacts, keyFactToEmbed{rowID: factRowID, text: kf.Fact})
			}
		}

		// asOf resolves in.level+in.period into the single real-world date
		// entities.first_seen/last_updated should reflect — see
		// periodAsOfDate's own doc comment for why this exists at all
		// (previously Postgres's own current_date, the real wall-clock
		// day, regardless of what date is actually being consolidated).
		// Falls back to real now() only if a period is somehow
		// unparseable, which shouldn't happen for a level this package
		// itself generated — never blocks the write over it, same
		// degrade-not-fail posture as groundingCheck's own fallback.
		asOf, ok := periodAsOfDate(in.level, in.period)
		if !ok {
			slog.Warn("consolidation: could not resolve an as-of date for entity timestamps, using real now instead", "level", in.level, "period", in.period)
			asOf = time.Now()
		}
		if err := r.upsertEntities(ctx, tx, in.scope, entities, in.replaceEntityAttrs, asOf); err != nil {
			return fmt.Errorf("upsert entities for summary %s: %w", id, err)
		}

		// Runs after upsertEntities, not before: entity_relationships'
		// subject_id/object_id foreign keys require the referenced entity
		// rows to already exist, and upsertEntities is what just created
		// or updated them in this same transaction.
		if err := r.upsertRelationships(ctx, tx, in.scope, in.output.Relationships, id); err != nil {
			return fmt.Errorf("upsert relationships for summary %s: %w", id, err)
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
		return "", err
	}

	// Embedding runs after commit, genuinely best-effort — a real,
	// confirmed bug this used to get wrong (docs/CODEBASE_SURVEY_AND_REVIEW.md
	// finding A6): a failed embed means this summary/these entities won't
	// surface via vector search until the next reindex, not that
	// consolidation itself failed — the summary row, its key facts, and
	// every touched entity are already durably stored. Returning an error
	// here used to propagate all the way to cmd/hupi-consolidate's scope
	// loop, making a transient embedding-provider hiccup register as a
	// failed consolidation run (a false alarm — everything that actually
	// matters already committed) *and*, because RunDaily returned
	// immediately on this error, silently skip the cross-period
	// contradiction check and the embedding backfills that would
	// otherwise have run next for this scope/day — real functional work
	// lost, not just a misleading metric. Logging and continuing (the
	// same pattern this package already uses for every other genuinely
	// best-effort step, e.g. checkCrossPeriodContradictions) fixes both.
	if err := r.embedSummary(ctx, in.scope, id, in.output.Summary); err != nil {
		slog.Warn("consolidation: embed summary failed, row committed without it", "summary", id, "error", err)
	}
	if err := r.embedEntities(ctx, in.scope, entityIDs); err != nil {
		slog.Warn("consolidation: embed entities failed, rows committed without it", "summary", id, "error", err)
	}
	if err := r.embedKeyFacts(ctx, in.scope, groundedFacts); err != nil {
		slog.Warn("consolidation: embed key facts failed, facts committed without embeddings (lexical ranking fallback)", "summary", id, "facts", len(groundedFacts), "error", err)
	}
	return id, nil
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
	// Two concurrent transactions computing a version for the same
	// scope+level+period under READ COMMITTED could both read the same
	// maxVersion before either commits and both attempt to insert the
	// identical id — summaries.id's primary key catches that deterministically
	// (a clean unique-violation error, not silent corruption), but it's still
	// a real, avoidable failure for whichever transaction loses the race
	// (review finding C5). pg_advisory_xact_lock, not the manual-unlock
	// session-held lock internal/rotate/demo use elsewhere: this call always
	// runs inside the one transaction that calls it (q is the tx from
	// storeSummary's own dbscope.Run), so a lock that releases automatically
	// on that transaction's commit/rollback is the exact right lifetime —
	// no separate connection or defer-unlock needed.
	lockKey := scope.Kind + ":" + scope.Owner + ":" + level + ":" + period
	if _, err := q.ExecContext(ctx, `select pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return "", fmt.Errorf("acquire next-version lock for %s %s: %w", level, period, err)
	}

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

// canonicalEntityID derives an entity's id from its kind and name instead
// of trusting whatever id string a caller supplied — found necessary via
// live testing: the same real-world entity ("Project Falcon") came back
// as both "project:falcon" and "project:project-falcon" from two separate
// consolidation runs, because the consolidation LLM (and, in principle, a
// hand-authored hupi-correct) picks an id slug freely each time rather
// than reusing a stable one. That fragmented one entity into two rows,
// both of which surfaced in retrieval side by side. kind+name is far more
// consistent across runs than a freely-chosen slug — an LLM restates a
// real thing's actual name the same way much more reliably than it
// reinvents a matching id — so it's what actually identifies "the same
// entity" here, not whatever id string happened to get generated this
// time.
//
// Trade-off, accepted deliberately: two genuinely distinct entities that
// happen to share both kind and name (two different people named "Alex",
// say) collapse into one row, with no disambiguation. That mirrors how
// the rest of the entity model already treats id as sole identity — this
// just computes that id from more stable inputs. Falls back to the
// caller-supplied id unchanged if name doesn't slugify to anything (e.g.
// empty, or all punctuation) rather than emit a bare "kind:" id.
func canonicalEntityID(kind, name, fallback string) string {
	slug := slugify(name)
	if slug == "" {
		return fallback
	}
	return kind + ":" + slug
}

// slugify lowercases s and collapses every run of characters outside
// a-z0-9 into a single hyphen, trimming any leading/trailing hyphen.
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := true // suppresses a leading hyphen without a separate trim pass
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// upsertEntities writes each touched entity's identity (kind/name/
// timestamps) and its attribute values into the unified `memories`
// table (schema/0025, schema/0027) — Phase 0 of
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md. entities.attributes is no
// longer written from here: an existing entity's legacy blob is left
// exactly as it was (read-only, for the rollback window the plan doc's
// own risk section calls for) and a new entity simply gets a null one.
//
// Every touched key gets a brand-new memories row every call —
// superseding whatever row was previously current for that (entity,
// key) pair, via the same supersedes-chain convention
// summaries.supersedes already established (schema/0027) — rather than
// the old flat per-key overlay (mergeAttributes) that silently
// discarded an attribute's prior value on every update
// (docs/DESIGN_VS_BUILT.md #1 wanted the *set* of known attributes to
// survive an update that only mentions one of them; it never said
// anything about keeping the *old value* around too, which attributes
// gain here for the first time). Deliberately not a decrypt-and-
// compare-then-skip-if-unchanged optimization: a consolidation run that
// reaffirms an unchanged value is itself a real signal ("still true as
// of today"), and skipping the write would mean decrypting every
// existing value on every single run just to maybe save one write.
//
// `for update` locks the entity identity row against another
// consolidation process running concurrently against the same entity —
// same guard the old implementation's own `for update` gave the
// attributes column, now protecting the identity row instead (no
// equivalent row-lock exists for the per-key memories rows themselves).
//
// replace flips the per-entity write to a wholesale reset: every
// currently-live key not restated in e.Attributes this call is
// tombstoned (a new row with empty content, grounded=false — "this key
// no longer applies," not "this key still holds its last known value"),
// matching the old replace=true branch's wholesale `merged =
// e.Attributes`. Used only by Runner.Correct: a correction's whole
// point is to fix a wrong fact, and the default additive behavior's
// "new keys win, old keys survive" means a correction that changes what
// an old run called concurrency_limit but writes it back under a
// differently-named key like concurrent_jobs_per_node would otherwise
// leave the stale key sitting right next to the corrected one, both
// still live — found by observing exactly that after a real
// correction. A human correction is expected to state the entity's
// full corrected set of touched attributes, not a partial patch, so
// resetting is the semantically correct behavior specifically for this
// caller.
func (r *Runner) upsertEntities(ctx context.Context, tx *sql.Tx, scope identity.Scope, updates []EntityUpdate, replace bool, asOf time.Time) error {
	enc, keyVersion, err := r.keys.GetOrCreate(ctx, scope)
	if err != nil {
		return fmt.Errorf("resolve encryption key for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	for _, e := range updates {
		if e.ID == "" {
			continue
		}

		exists, err := entityExists(ctx, tx, scope, e.ID)
		if err != nil {
			return fmt.Errorf("check existing entity %s: %w", e.ID, err)
		}
		if exists {
			var discard string
			if err := tx.QueryRowContext(ctx, `
				select id from entities where scope_kind = $1 and scope_owner = $2 and id = $3 for update
			`, scope.Kind, scope.Owner, e.ID).Scan(&discard); err != nil {
				return fmt.Errorf("lock existing entity %s: %w", e.ID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				update entities set name = $1, last_updated = $2
				where scope_kind = $3 and scope_owner = $4 and id = $5
			`, e.Name, asOf, scope.Kind, scope.Owner, e.ID); err != nil {
				return fmt.Errorf("update entity %s: %w", e.ID, err)
			}
		} else {
			if _, err := tx.ExecContext(ctx, `
				insert into entities (id, kind, name, first_seen, last_updated, scope_kind, scope_owner, key_version)
				values ($1, $2, $3, $4, $4, $5, $6, $7)
			`, e.ID, e.Kind, e.Name, asOf, scope.Kind, scope.Owner, keyVersion); err != nil {
				return fmt.Errorf("insert entity %s: %w", e.ID, err)
			}
		}

		current, err := entityattrs.CurrentRowIDs(ctx, tx, scope, e.ID)
		if err != nil {
			return fmt.Errorf("load current attribute rows for entity %s: %w", e.ID, err)
		}

		// replace mode: every currently-live key not restated this call is
		// gone (wholesale reset). Non-replace mode: only the keys this run
		// explicitly names via SupersedesKeys are gone (Phase C
		// sub-problem 1's renamed-key case) — every other existing key
		// survives untouched, needing no new row at all.
		var staleKeys []string
		if replace {
			for key := range current {
				if _, keep := e.Attributes[key]; !keep {
					staleKeys = append(staleKeys, key)
				}
			}
		} else {
			staleKeys = e.SupersedesKeys
			if len(staleKeys) > 0 {
				slog.Info("consolidation: entity attribute key superseded", "entity", e.ID, "superseded_keys", staleKeys)
			}
		}
		for _, staleKey := range staleKeys {
			oldID, ok := current[staleKey]
			if !ok {
				continue // already gone, or never existed — nothing to tombstone
			}
			tombstoneID, err := entityattrs.NewID()
			if err != nil {
				return fmt.Errorf("generate tombstone id for entity %s key %s: %w", e.ID, staleKey, err)
			}
			emptyCT, err := enc.Encrypt("")
			if err != nil {
				return fmt.Errorf("encrypt tombstone for entity %s key %s: %w", e.ID, staleKey, err)
			}
			if _, err := tx.ExecContext(ctx, `
				insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded, supersedes)
				values ($1, $2, $3, $4, $5, $6, $7, true, false, $8)
			`, tombstoneID, scope.Kind, scope.Owner, e.ID, staleKey, emptyCT, keyVersion, oldID); err != nil {
				return fmt.Errorf("tombstone entity %s key %s: %w", e.ID, staleKey, err)
			}
		}

		for key, value := range e.Attributes {
			ct, err := enc.Encrypt(value)
			if err != nil {
				return fmt.Errorf("encrypt attribute %s.%s: %w", e.ID, key, err)
			}
			rowID, err := entityattrs.NewID()
			if err != nil {
				return fmt.Errorf("generate id for entity %s key %s: %w", e.ID, key, err)
			}
			if _, err := tx.ExecContext(ctx, `
				insert into memories (id, scope_kind, scope_owner, entity_id, attribute_key, content, key_version, is_static, grounded, supersedes)
				values ($1, $2, $3, $4, $5, $6, $7, true, true, $8)
			`, rowID, scope.Kind, scope.Owner, e.ID, key, ct, keyVersion, pgfmt.Nullable(current[key])); err != nil {
				return fmt.Errorf("insert attribute for entity %s key %s: %w", e.ID, key, err)
			}
		}
	}
	return nil
}

// relationshipPredicateMaxLen bounds how long a predicate string can be
// — generous for a real short verb phrase ("works_at", "married_to"),
// tight enough to reject a model dumping a whole sentence into this
// field instead of a predicate.
const relationshipPredicateMaxLen = 64

// sanitizePredicate lightly normalizes a predicate rather than
// validating it against an enum — see
// schema/0015_entity_relationships.sql's own comment for why a predicate
// enum would very likely repeat the entity-kind-enum problem fixed this
// session, at a larger scale (many more plausible relationship-type
// strings than the 7-value entity kind enum). Returns "" — skip this
// relationship, same treatment as an invalid entity kind — for anything
// empty or implausibly long.
func sanitizePredicate(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > relationshipPredicateMaxLen {
		return ""
	}
	return s
}

// personPredicateAliases collapses a short, explicitly-curated list of
// known synonyms the consolidation prompt's own free-text predicate
// guidance ("a short verb phrase, e.g. works_at, married_to,
// friends_with, manages" — buildSummaryPrompt) gives the model no reason
// to pick consistently between. Found for real in production:
// person:sujith-samuel ended up with both lives_in and based_in edges to
// the same place, which upsertRelationships' dedup (exact-match and the
// bare-edge check) can't catch, since those checks compare predicate
// strings verbatim, not meaning.
//
// Scoped to subject kind "person" specifically, not applied
// unconditionally: "based_in" is also a natural, correct predicate for an
// organization/project's registered location ("organization:acme
// based_in place:nyc"), where "lives_in" would read oddly — an
// organization doesn't live anywhere. Keep this map short and
// subject-kind-scoped rather than a general synonym engine; add an entry
// only once a real duplicate like this one is actually observed, the
// same reasoned-not-speculative posture this package's other constants
// take.
var personPredicateAliases = map[string]string{
	"based_in": "lives_in",
}

// canonicalizePredicate applies personPredicateAliases when subjectKind
// is "person" — called after sanitizePredicate, not merged into it, so
// the two stay single-purpose (one normalizes form, the other normalizes
// meaning for a specific, known synonym set).
func canonicalizePredicate(subjectKind, predicate string) string {
	if subjectKind != "person" {
		return predicate
	}
	if canonical, ok := personPredicateAliases[predicate]; ok {
		return canonical
	}
	return predicate
}

// parseOptionalDate returns the date string unchanged if it parses as a
// real YYYY-MM-DD date, or nil (a real SQL NULL, "unknown/unstated") for
// anything empty or unparseable — the consolidation LLM's own
// valid_from/valid_until citations are just as prone to the same
// malformed-output edge cases entity attributes needed hardening for
// (see EntityUpdate's own UnmarshalJSON doc comment), and one
// relationship's unusable date shouldn't fail the whole day's
// consolidation any more than one entity's malformed attribute does.
func parseOptionalDate(s string) any {
	if s == "" {
		return nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return nil
	}
	return s
}

// memoryIDForKeyFact derives a key fact's citation id in the unified
// memories table — same deterministic scheme
// internal/backfillmemories.memoryIDForKeyFact and
// internal/store/retrieve.go's own copy use (duplicated across
// packages rather than exported and imported, same precedent as
// internal/gateway/attribution.go's extractJSON: a one-line,
// self-contained helper isn't worth a cross-package dependency).
// Package-level here (not inlined at each call site) since this
// package now has two call sites: storeSummary's own insert, and
// contradiction.go's recordUpdateRelations linking a correction's new
// fact back to the one it replaced.
func memoryIDForKeyFact(factID int64) string {
	return "mem_fact_" + strconv.FormatInt(factID, 10)
}

// newRelationshipID is a random opaque id, unlike entities' own
// canonicalEntityID scheme — a relationship has no natural stable name
// to canonicalize against the way an entity's kind+name does (the same
// subject+predicate+object can legitimately recur across separate,
// distinct time periods, e.g. two different employments at the same
// company), so identity here is arbitrary, and dedup (see
// upsertRelationships) is handled by an explicit existence check instead
// of relying on primary-key collision the way entities' upsert does.
func newRelationshipID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate relationship id: %w", err)
	}
	return "rel_" + hex.EncodeToString(buf), nil
}

// entityExists reports whether id already exists in entities for scope —
// entities' real primary key is the composite (scope_kind, scope_owner,
// id), not just id (two scopes can each have their own "project:hupi"),
// so this checks all three.
func entityExists(ctx context.Context, tx *sql.Tx, scope identity.Scope, id string) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		select exists(
			select 1 from entities
			where scope_kind = $1 and scope_owner = $2 and id = $3
		)
	`, scope.Kind, scope.Owner, id).Scan(&exists)
	return exists, err
}

// upsertRelationships writes the connections a consolidation run says
// this period touched — docs/ENTITY_RELATIONSHIPS_PLAN.md §§3-5 for the
// full design and the specific reasoning below. Must run after
// upsertEntities in the same transaction: subject_id/object_id are
// foreign keys into entities, which upsertEntities is what just
// created/updated.
func (r *Runner) upsertRelationships(ctx context.Context, tx *sql.Tx, scope identity.Scope, updates []RelationshipUpdate, sourceSummaryID string) error {
	for _, u := range updates {
		if !validEntityKinds[u.SubjectKind] || !validEntityKinds[u.ObjectKind] {
			slog.Warn("consolidation: skipping relationship with invalid subject/object kind",
				"subject_kind", u.SubjectKind, "object_kind", u.ObjectKind,
				"scope_kind", scope.Kind, "scope_owner", scope.Owner)
			continue
		}
		predicate := canonicalizePredicate(u.SubjectKind, sanitizePredicate(u.Predicate))
		if predicate == "" {
			slog.Warn("consolidation: skipping relationship with empty or too-long predicate",
				"raw_predicate", u.Predicate, "scope_kind", scope.Kind, "scope_owner", scope.Owner)
			continue
		}
		subjectID := canonicalEntityID(u.SubjectKind, u.SubjectName, "")
		objectID := canonicalEntityID(u.ObjectKind, u.ObjectName, "")
		if subjectID == "" || objectID == "" {
			slog.Warn("consolidation: skipping relationship with unresolvable subject/object name",
				"subject_name", u.SubjectName, "object_name", u.ObjectName,
				"scope_kind", scope.Kind, "scope_owner", scope.Owner)
			continue
		}

		// subject_id/object_id are hard foreign keys into entities (see
		// schema/0015_entity_relationships.sql) — but the consolidation
		// LLM's relationships[] list and its entities[] list are two
		// separate parts of the same JSON output, and nothing stops the
		// model from naming a relationship endpoint (e.g. a kind+name
		// pair) that its own entities[] list never actually declared, or
		// declared under a slightly different kind/name that canonicalizes
		// to a different id. upsertEntities has already run by this point
		// (this function's own doc comment), so any entity the model
		// meant to reference genuinely exists now if it's ever going to —
		// checking existence here and skipping (not failing the whole
		// day) is the same treatment already given to an invalid kind or
		// an unresolvable name above, for the same reason: one
		// malformed/inconsistent relationship shouldn't take down
		// everything else this run extracted.
		subjectExists, err := entityExists(ctx, tx, scope, subjectID)
		if err != nil {
			return fmt.Errorf("check subject entity exists %s: %w", subjectID, err)
		}
		objectExists, err := entityExists(ctx, tx, scope, objectID)
		if err != nil {
			return fmt.Errorf("check object entity exists %s: %w", objectID, err)
		}
		if !subjectExists || !objectExists {
			slog.Warn("consolidation: skipping relationship referencing an entity that was never extracted",
				"subject_id", subjectID, "subject_exists", subjectExists,
				"object_id", objectID, "object_exists", objectExists,
				"scope_kind", scope.Kind, "scope_owner", scope.Owner)
			continue
		}

		validFrom := parseOptionalDate(u.ValidFrom)
		validUntil := parseOptionalDate(u.ValidUntil)

		// Idempotent re-consolidation: an exact repeat of an
		// already-stored edge (same subject/predicate/object/validity) is
		// skipped rather than inserted again — RunDaily re-consolidating
		// an already-drafted day is expected to re-extract the same
		// relationship from the same source text every time, and
		// relationships have no natural primary key the way an entity's
		// canonicalized id gives it, so this existence check is what
		// upsertEntities gets for free from `on conflict` and this
		// doesn't.
		var exists bool
		err = tx.QueryRowContext(ctx, `
			select exists(
				select 1 from entity_relationships
				where scope_kind = $1 and scope_owner = $2
				  and subject_id = $3 and predicate = $4 and object_id = $5
				  and valid_from is not distinct from $6::date
				  and valid_until is not distinct from $7::date
			)
		`, scope.Kind, scope.Owner, subjectID, predicate, objectID, validFrom, validUntil).Scan(&exists)
		if err != nil {
			return fmt.Errorf("check existing relationship %s %s %s: %w", subjectID, predicate, objectID, err)
		}
		if exists {
			continue
		}

		// A bare edge (no valid_from, no valid_until — "this relationship
		// exists" with zero temporal information) is skipped if ANY row
		// already exists for this exact (subject, predicate, object)
		// triple, regardless of that existing row's own validity window.
		// Found for real in production: the exact-match check above only
		// catches a repeat of the *same* validity window, so a bare edge
		// extracted in the same consolidation run as (or after) a
		// dated one for the identical triple sailed straight through it —
		// cluster.go's mergeConsolidationOutputs concatenates every
		// cluster's relationships with no cross-cluster dedup, so one
		// cluster's dateless mention and another's dated one for the same
		// fact both reach here in the same run. A bare assertion adds
		// strictly no information beyond what any existing record of the
		// same triple already established, dated or not — unlike the
		// supersession logic below, skipping it here never closes or
		// alters an existing row, so it can't trigger the one-to-many
		// edge-loss concern docs/ENTITY_RELATIONSHIPS_PLAN.md §5 raises
		// about that separate mechanism.
		if validFrom == nil && validUntil == nil {
			var anyExists bool
			err = tx.QueryRowContext(ctx, `
				select exists(
					select 1 from entity_relationships
					where scope_kind = $1 and scope_owner = $2
					  and subject_id = $3 and predicate = $4 and object_id = $5
				)
			`, scope.Kind, scope.Owner, subjectID, predicate, objectID).Scan(&anyExists)
			if err != nil {
				return fmt.Errorf("check existing relationship (any validity) %s %s %s: %w", subjectID, predicate, objectID, err)
			}
			if anyExists {
				continue
			}
		}

		// Conservative supersession, deliberately narrower than "same
		// subject+predicate always supersedes the old object": that rule
		// is wrong for a genuinely one-to-many predicate (e.g.
		// friends_with — a person can have many, concurrently), and
		// there's no way to tell a one-to-one predicate apart from a
		// one-to-many one from the predicate string alone. Only closes an
		// existing open-ended edge (valid_until is null) for the same
		// (subject, predicate) but a *different* object when the *new*
		// edge gives an explicit valid_from — requiring a stated
		// transition date is the direction that risks leaving some stale
		// one-to-one edges open rather than the direction that risks
		// wrongly closing a valid one-to-many edge. See
		// docs/ENTITY_RELATIONSHIPS_PLAN.md §5 for the two approaches this
		// was weighed against.
		//
		// "and (valid_from is null or valid_from <= $1::date)" is a real,
		// confirmed fix, not defensive padding: consolidation doesn't
		// process periods in strict chronological order (cross-period
		// contradiction correction revisits earlier periods after later
		// ones are already stored), so the "existing" open-ended edge
		// being closed here can have its own valid_from *later* than the
		// new edge's — closing it at the new edge's (earlier) valid_from
		// would set valid_until before valid_from, violating
		// entity_relationships_valid_date_order_check
		// (schema/0019_entity_relationships_valid_date_order.sql). A real
		// LongMemEval run reproduced this for real data (multiple scopes,
		// multiple predicates, e.g. person:user listens_to). Skipping
		// such a row rather than closing it wrongly is the same
		// conservative direction this function already takes everywhere
		// else — leaving a stale edge open a little longer beats
		// corrupting one into an invalid window.
		if validFrom != nil {
			if _, err := tx.ExecContext(ctx, `
				update entity_relationships
				set valid_until = $1::date
				where scope_kind = $2 and scope_owner = $3
				  and subject_id = $4 and predicate = $5
				  and object_id != $6
				  and valid_until is null
				  and (valid_from is null or valid_from <= $1::date)
			`, validFrom, scope.Kind, scope.Owner, subjectID, predicate, objectID); err != nil {
				return fmt.Errorf("close superseded relationship edges for %s %s: %w", subjectID, predicate, err)
			}
		}

		relID, err := newRelationshipID()
		if err != nil {
			return err
		}
		// pgfmt.Nullable(sourceSummaryID), not the raw string: an empty
		// string isn't a valid summaries(id) reference (a real error this
		// package's own tests caught, calling upsertRelationships in
		// isolation, without a real summary row yet — the exact same
		// reason summaries.supersedes uses this helper for its own
		// optional self-reference).
		if _, err := tx.ExecContext(ctx, `
			insert into entity_relationships
				(id, scope_kind, scope_owner, subject_id, predicate, object_id, valid_from, valid_until, source_summary_id)
			values ($1, $2, $3, $4, $5, $6, $7::date, $8::date, $9)
		`, relID, scope.Kind, scope.Owner, subjectID, predicate, objectID, validFrom, validUntil, pgfmt.Nullable(sourceSummaryID)); err != nil {
			return fmt.Errorf("insert relationship %s %s %s: %w", subjectID, predicate, objectID, err)
		}
	}
	return nil
}

// EmbedderIdentity is what gets recorded in embedding_model whenever
// something is embedded — see schema/0013_embedding_model_tracking.sql
// for why: a vector from one embedding model isn't comparable to a
// vector from a different one (cosine similarity between them is closer
// to noise than a real signal), so every embed needs to record which
// model produced it, or a later provider switch has no way to tell old,
// now-incomparable vectors apart from current ones.
func EmbedderIdentity(p provider.Provider) string {
	return p.Vendor() + ":" + p.Model()
}

func (r *Runner) embedSummary(ctx context.Context, scope identity.Scope, id, text string) error {
	resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(text)}})
	if err != nil {
		return err
	}
	if len(resp.Vectors) == 0 {
		return errors.New("embedder returned no vectors")
	}
	vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
	model := EmbedderIdentity(r.embedder)
	return dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `update summaries set embedding = $1::vector, embedding_model = $2 where id = $3`, vectorLiteral, model, id)
		return err
	})
}

// embedEntities embeds each id's *current* name+attributes and stores the
// result, so entities become reachable by internal/store's
// vectorSearchEntities in addition to stage1EntityMatches' substring
// check — see schema/0012_entity_embeddings.sql for why: a paraphrase
// that doesn't literally contain an entity's name currently misses it
// outright, even when it's exactly the answer.
//
// Reads each entity's attributes back from the database rather than
// trusting the EntityUpdate the caller just wrote, deliberately: under
// merge mode (normal consolidation) the caller's own update is often a
// partial patch, and the stored row reflects the full merged result,
// which is what should actually be embedded.
//
// Best-effort per entity — one embedding call and write failing doesn't
// stop the rest, same posture as embedSummary and
// embedHighImportanceEpisodes: an entity whose embedding fails is still
// fully usable via the existing substring-match path, just not yet
// reachable by similarity. Failures are still collected and returned
// (not silently swallowed), matching embedSummary's own "row committed,
// embedding not" error wrapping at the call site — worth surfacing to
// whoever's watching cron output, not worth failing the whole
// consolidation run over a supplementary index.
func (r *Runner) embedEntities(ctx context.Context, scope identity.Scope, ids []string) error {
	var errs []error
	for _, id := range ids {
		if id == "" {
			continue
		}
		var kind, name string
		var attrs map[string]string
		err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			if err := tx.QueryRowContext(ctx, `
				select kind, name from entities
				where id = $1 and scope_kind = $2 and scope_owner = $3
			`, id, scope.Kind, scope.Owner).Scan(&kind, &name); err != nil {
				return err
			}
			var err error
			attrs, err = entityattrs.Current(ctx, tx, r.keys, scope, id)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: load current attributes: %w", id, err))
			continue
		}

		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{provider.TruncateForEmbedding(EntityEmbedText(kind, name, attrs))}})
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: embed call: %w", id, err))
			continue
		}
		if len(resp.Vectors) == 0 {
			errs = append(errs, fmt.Errorf("entity %s: embedder returned no vectors", id))
			continue
		}
		vectorLiteral := pgfmt.VectorLiteral(resp.Vectors[0])
		model := EmbedderIdentity(r.embedder)
		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
				update entities set embedding = $1::vector, embedding_model = $2
				where id = $3 and scope_kind = $4 and scope_owner = $5
			`, vectorLiteral, model, id, scope.Kind, scope.Owner)
			return err
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: write embedding: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// keyFactToEmbed is the slice of a just-inserted summary_key_facts row
// embedKeyFacts actually needs — storeSummary collects these from its own
// insert loop (via `returning id`) rather than re-querying after commit.
type keyFactToEmbed struct {
	rowID int64
	text  string
}

// keyFactEmbedBatchSize bounds how many facts go into one Embed call.
// Every real busy-day summary count seen so far (81, 85, 112, 120, 125
// facts) fits in a single batch at this size, which matters because
// rankKeyFacts (internal/store/retrieve.go) only ranks a summary
// semantically when *all* of its facts carry a valid embedding — one call
// per summary keeps that the common case instead of the exception.
const keyFactEmbedBatchSize = 256

// keyFactEmbedMaxChars truncates any single runaway fact before it's sent
// for embedding — well under any provider's token limit, so one
// oversized input can't fail an entire batch and leave every other fact
// in it unembedded (a real failure mode: OpenAI rejects a whole request
// over one input exceeding its token cap).
const keyFactEmbedMaxChars = 4000

// embedKeyFacts embeds every grounded key fact just written for one
// summary, batched into as few Embed calls as possible (see
// keyFactEmbedBatchSize), and writes the resulting vectors back in one
// transaction. Best-effort, called after storeSummary's own transaction
// has already committed — same posture as embedSummary/embedEntities: a
// failed or partial embed leaves the facts fully usable via the existing
// lexical ranking fallback (internal/store/retrieve.go's rankKeyFacts),
// never blocks or retries consolidation itself.
func (r *Runner) embedKeyFacts(ctx context.Context, scope identity.Scope, facts []keyFactToEmbed) error {
	if len(facts) == 0 {
		return nil
	}
	model := EmbedderIdentity(r.embedder)
	var errs []error
	for start := 0; start < len(facts); start += keyFactEmbedBatchSize {
		end := min(start+keyFactEmbedBatchSize, len(facts))
		chunk := facts[start:end]

		texts := make([]string, len(chunk))
		for i, f := range chunk {
			runes := []rune(f.text)
			if len(runes) > keyFactEmbedMaxChars {
				// Rune-safe, not a byte slice — a byte index can land
				// mid-character on multi-byte UTF-8 (smart quotes,
				// accented names), the same class of bug fixed in
				// centeredExcerpt (internal/store/retrieve.go).
				runes = runes[:keyFactEmbedMaxChars]
			}
			texts[i] = provider.TruncateForEmbedding(string(runes))
		}

		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: texts})
		if err != nil {
			errs = append(errs, fmt.Errorf("embed key facts batch (rows %d-%d): %w", chunk[0].rowID, chunk[len(chunk)-1].rowID, err))
			continue
		}
		if len(resp.Vectors) != len(chunk) {
			errs = append(errs, fmt.Errorf("embed key facts batch (rows %d-%d): embedder returned %d vectors, want %d",
				chunk[0].rowID, chunk[len(chunk)-1].rowID, len(resp.Vectors), len(chunk)))
			continue
		}

		err = dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			for i, f := range chunk {
				if _, err := tx.ExecContext(ctx, `
					update summary_key_facts set embedding = $1::vector, embedding_model = $2 where id = $3
				`, pgfmt.VectorLiteral(resp.Vectors[i]), model, f.rowID); err != nil {
					return fmt.Errorf("write embedding for key fact %d: %w", f.rowID, err)
				}
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EntityEmbedText renders an entity as text worth embedding — name and
// kind up front since those carry the most semantic weight for a query
// like "what's my X", followed by attributes in a stable (sorted) key
// order so the same entity content always embeds to the same text
// regardless of Go's randomized map iteration order.
func EntityEmbedText(kind, name string, attrs map[string]string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%s)", name, kind)
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&sb, "\n%s: %s", k, attrs[k])
	}
	return sb.String()
}
