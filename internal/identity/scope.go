// Package identity holds the Tier 3 (Professional Shared) concepts that
// don't belong to any single existing package: which scope a record or
// request belongs to, and (from Phase 3 onward) who's making the request.
// See docs/TIER3_PLAN.md.
package identity

const (
	ScopeKindPrivate = "private"
	ScopeKindShared  = "shared"
)

// Scope identifies which owner's memory a request or record belongs to
// (docs/TIER3_PLAN.md D1) — Kind is "private" (Owner is a user id) or
// "shared" (Owner is a team id).
type Scope struct {
	Kind  string `json:"kind"`
	Owner string `json:"owner"`
}

// DefaultUserID/DefaultScope are what every pre-Tier-3 row was backfilled
// to (schema/0002_tier3_phase1_identity.sql) and what every caller uses
// until Phase 3 adds real authentication — a Phase 1/2 deployment with no
// auth middleware is, in effect, a single-user deployment where this is
// always the answer.
const DefaultUserID = "user:default"

var DefaultScope = Scope{Kind: ScopeKindPrivate, Owner: DefaultUserID}

const (
	RefKindSummary = "summary"
	// RefKindEntity no longer appears in any gateway.Citation —
	// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's citation model retired
	// it from citation-reporting entirely in favor of RefKindMemory, one
	// per live attribute (an attribute is its own memories row now, not
	// a single bundled entities.attributes blob a citation could only
	// point at as a whole). It still appears in RetrievalResult.Refs,
	// unrelated to citations: internal/store/retrieve.go's
	// refIDsOfKind reads a plain RefKindEntity ref per matched entity to
	// seed graphWalkRelationships and to exclude already-matched
	// entities from later search passes, independent of however many
	// (or how few — zero, for an entity with no current attributes)
	// RefKindMemory citations that entity actually produced.
	RefKindEntity  = "entity"
	RefKindEpisode = "episode"
	// RefKindMemory is the fact-level citation type —
	// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's "Decided: citations
	// move to fact granularity." ID is a memories.id (schema/0025). It
	// replaces RefKindEntity entirely for entity-attribute citations
	// (an attribute is a memories row now, not a bundled entities.attributes
	// blob) and replaces RefKindSummary's old fact-bundling role — a
	// summary's key facts are now cited one RefKindMemory per fact, not
	// folded into a single RefKindSummary citation. RefKindSummary
	// itself stays for a summary's *prose* paragraph, a separate citation
	// from any individual fact within it, not replaced by this.
	RefKindMemory = "memory"
)

// Ref is a scope-qualified reference to a summary, entity, or episode —
// what's recorded in episodes.retrieved_refs. A bare id string isn't
// enough once ids are only unique *within* a scope: a private episode can
// retrieve entities from multiple teams in one turn, and two different
// teams can each have their own "project:hupi" (see docs/TIER3_PLAN.md
// D2 — this replaces the old retrieved_summary_ids/retrieved_entity_ids/
// retrieved_episode_ids text[] columns).
type Ref struct {
	Kind  string `json:"kind"` // RefKindSummary | RefKindEntity | RefKindEpisode
	Scope Scope  `json:"scope"`
	ID    string `json:"id"`
}
