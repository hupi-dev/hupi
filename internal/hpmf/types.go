// Package hpmf implements the HUPI Portable Memory Format (MEMORY_FORMAT.md)
// read/write side — the decrypted directory tree hupi-export produces and
// hupi-import consumes, plus the age-encrypted single-file packaging
// around it. See docs/GAP_CLOSURE_PLAN.md §4.2 for the multi-tenant
// extensions (per-scope vs. whole-deployment, merge-capable import) this
// package adds on top of the original single-owner design.
package hpmf

import (
	"encoding/json"
	"time"

	"hupi/internal/identity"
)

// HPMFVersion is written to every manifest.json and checked on import —
// hupi-import only ever loads a bundle produced by the same major version
// of hupi-export it's running against (docs/GAP_CLOSURE_PLAN.md §2).
const HPMFVersion = "1.0"

// Manifest is manifest.json — one per bundle, whether it holds one scope
// or every scope in the deployment.
type Manifest struct {
	HPMFVersion string          `json:"hpmf_version"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by"` // actor, see cmd/hupi-export's -actor flag
	Scopes      []ScopeManifest `json:"scopes"`
}

// ScopeManifest is one scope's stats within a Manifest, and — via Dir —
// where that scope's episodes/summaries/entities live inside the bundle.
// Dir is "." for a single-scope export (flat, matching the original
// single-owner MEMORY_FORMAT.md layout) or "scopes/<kind>-<owner>" for a
// whole-deployment export — hupi-import reads Dir rather than inferring
// the layout, so the two shapes never need separate detection logic.
type ScopeManifest struct {
	ScopeKind         string     `json:"scope_kind"`
	ScopeOwner        string     `json:"scope_owner"`
	Dir               string     `json:"dir"`
	EpisodeCount      int        `json:"episode_count"`
	SummaryCount      int        `json:"summary_count"`
	EntityCount       int        `json:"entity_count"`
	EarliestEpisodeAt *time.Time `json:"earliest_episode_at,omitempty"`
	LatestEpisodeAt   *time.Time `json:"latest_episode_at,omitempty"`
}

func (m ScopeManifest) Scope() identity.Scope {
	return identity.Scope{Kind: m.ScopeKind, Owner: m.ScopeOwner}
}

// dirName turns a scope into a filesystem-safe directory name — team ids
// and user ids both look like "kind:name", and ":" is an awkward path
// component on some filesystems, so it's replaced with "_" here only
// (the manifest's ScopeKind/ScopeOwner fields keep the real values;
// Dir is purely a filesystem path).
func dirName(scope identity.Scope) string {
	owner := make([]byte, 0, len(scope.Owner))
	for _, r := range scope.Owner {
		if r == ':' {
			owner = append(owner, '_')
			continue
		}
		owner = append(owner, byte(r))
	}
	return "scopes/" + scope.Kind + "-" + string(owner)
}

// EpisodeRecord is one line of episodes/YYYY/MM/YYYY-MM-DD.jsonl —
// plaintext (decrypted), matching MEMORY_FORMAT.md's episode record 1:1
// with the live episodes table, minus embedding (never exported — see
// package doc comment) and the columns the application never actually
// writes (context_tags, provider_endpoint, externalized, episodes.supersedes).
type EpisodeRecord struct {
	ID             string         `json:"id"`
	TS             time.Time      `json:"ts"`
	Type           string         `json:"type"`
	ProviderVendor string         `json:"provider_vendor,omitempty"`
	ProviderModel  string         `json:"provider_model,omitempty"`
	InputText      string         `json:"input_text,omitempty"`
	OutputText     string         `json:"output_text,omitempty"`
	Importance     *float64       `json:"importance,omitempty"`
	Hash           string         `json:"hash"`
	Truncated      bool           `json:"truncated,omitempty"`
	MemoryGate     string         `json:"memory_gate,omitempty"`
	RetrievedRefs  []identity.Ref `json:"retrieved_refs,omitempty"`
	RefersTo       string         `json:"refers_to,omitempty"`
	Rating         string         `json:"rating,omitempty"`
	Note           string         `json:"note,omitempty"`
}

// SummaryRecord is one summaries/<level>/<period>.json — the summary
// itself plus its key facts inline (summary_key_facts is a child table
// live, but has no independent identity worth a separate file here).
type SummaryRecord struct {
	ID                   string          `json:"id"`
	Period               string          `json:"period"`
	Level                string          `json:"level"`
	Status               string          `json:"status"`
	GeneratedByVendor    string          `json:"generated_by_vendor,omitempty"`
	GeneratedByModel     string          `json:"generated_by_model,omitempty"`
	SourceEpisodeIDs     []string        `json:"source_episode_ids,omitempty"`
	SourceSummaryPeriods []string        `json:"source_summary_periods,omitempty"`
	Summary              string          `json:"summary,omitempty"`
	EntitiesTouched      []string        `json:"entities_touched,omitempty"`
	GroundingChecked     bool            `json:"grounding_checked"`
	Supersedes           string          `json:"supersedes,omitempty"`
	CorrectionReason     string          `json:"correction_reason,omitempty"`
	CreatedAt            time.Time       `json:"created_at"`
	KeyFacts             []KeyFactRecord `json:"key_facts,omitempty"`
}

type KeyFactRecord struct {
	Fact             string   `json:"fact"`
	SourceEpisodeIDs []string `json:"source_episode_ids,omitempty"`
	Grounded         bool     `json:"grounded"`
}

// EntityRecord is one line of entities.jsonl. MEMORY_FORMAT.md's
// illustrative layout splits entities into per-kind files
// (people.jsonl, projects.jsonl, ...); this implementation uses one file
// with `kind` inline instead — simpler, and no file's existence or
// naming depends on which kind values happen to be in use. Documented as
// a deliberate deviation, not drift, in MEMORY_FORMAT.md.
type EntityRecord struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	FirstSeen   time.Time `json:"first_seen"`
	LastUpdated time.Time `json:"last_updated"`
	// Attributes is json.RawMessage, not string: the live column is
	// already JSON text once decrypted, and embedding it as a real nested
	// object here (rather than a JSON string holding escaped JSON) is
	// what keeps an export human-readable and diffable, per this
	// package's design principles.
	Attributes json.RawMessage `json:"attributes,omitempty"`
}
