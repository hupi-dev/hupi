package consolidation

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
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

		if err := r.upsertEntities(ctx, tx, in.scope, entities, in.replaceEntityAttrs); err != nil {
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
		return err
	}

	// Embedding runs after commit, best-effort: a failed embed means this
	// summary won't surface via vector search until the next reindex, not
	// that consolidation itself failed — the summary row is already
	// durably stored.
	if err := r.embedSummary(ctx, in.scope, id, in.output.Summary); err != nil {
		return fmt.Errorf("embed summary %s (row committed, embedding not): %w", id, err)
	}
	if err := r.embedEntities(ctx, in.scope, entityIDs); err != nil {
		return fmt.Errorf("embed entities touched by summary %s (rows committed, embeddings not): %w", id, err)
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

// upsertEntities merges each entity's new attributes over its existing
// ones rather than replacing them wholesale (docs/DESIGN_VS_BUILT.md #1):
// a consolidation run that only mentions one attribute of a previously
// richer entity must not erase everything else that was known about it.
// `for update` guards the read against another consolidation process
// running concurrently against the same entity. Entities are looked up and
// written within scope — the same "project:hupi" id can exist once per
// scope since entities' primary key is (scope_kind, scope_owner, id).
//
// replace flips that to a wholesale overwrite of each touched entity's
// attributes, used only by Runner.Correct: a correction's whole point is
// to fix a wrong fact, and merge's "new keys win, old keys survive"
// behavior means a correction that changes what an old run called
// concurrency_limit but writes it back under a differently-named key like
// concurrent_jobs_per_node leaves the stale key sitting right next to the
// corrected one, both visible to retrieval — found by observing exactly
// that after a real correction. A human correction is expected to state
// the entity's full corrected set of touched attributes, not a partial
// patch, so replacing is the semantically correct behavior specifically
// for this caller.
func (r *Runner) upsertEntities(ctx context.Context, tx *sql.Tx, scope identity.Scope, updates []EntityUpdate, replace bool) error {
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
			// new entity — nothing to merge or replace, e.Attributes as given
		case err != nil:
			return fmt.Errorf("load existing entity %s for merge: %w", e.ID, err)
		case replace:
			// `for update` above still ran, so this row stays locked
			// against a concurrent writer until this transaction commits
			// — merged is already e.Attributes, nothing else to do.
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
		predicate := sanitizePredicate(u.Predicate)
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
		err := tx.QueryRowContext(ctx, `
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
		if validFrom != nil {
			if _, err := tx.ExecContext(ctx, `
				update entity_relationships
				set valid_until = $1::date
				where scope_kind = $2 and scope_owner = $3
				  and subject_id = $4 and predicate = $5
				  and object_id != $6
				  and valid_until is null
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
	resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{text}})
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
		var attrsCT []byte
		var keyVersion int
		err := dbscope.Run(ctx, r.db, scope, scope, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `
				select kind, name, attributes, key_version from entities
				where id = $1 and scope_kind = $2 and scope_owner = $3
			`, id, scope.Kind, scope.Owner).Scan(&kind, &name, &attrsCT, &keyVersion)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: load current attributes: %w", id, err))
			continue
		}

		enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: resolve encryption key: %w", id, err))
			continue
		}
		attrsJSON, err := enc.Decrypt(attrsCT)
		if err != nil {
			errs = append(errs, fmt.Errorf("entity %s: decrypt attributes: %w", id, err))
			continue
		}
		var attrs map[string]string
		if attrsJSON != "" {
			if err := json.Unmarshal([]byte(attrsJSON), &attrs); err != nil {
				errs = append(errs, fmt.Errorf("entity %s: parse attributes: %w", id, err))
				continue
			}
		}

		resp, err := r.embedder.Embed(ctx, provider.EmbedRequest{Input: []string{EntityEmbedText(kind, name, attrs)}})
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
