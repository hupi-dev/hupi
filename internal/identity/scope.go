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
	RefKindEntity  = "entity"
	RefKindEpisode = "episode"
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
