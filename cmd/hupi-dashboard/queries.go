// Panel data for the dashboard — every query here is read-only and uses
// only plaintext columns (see docs/DASHBOARD.md's schema inventory): no
// decryption happens anywhere in this file. Phase 2's decrypt-on-view
// theme extraction (content_analysis.go) is a deliberately separate,
// opt-in file for exactly that reason.
package main

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"time"

	"hupi/internal/audit"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// ConversationVolumePoint is one day's interaction-episode count.
type ConversationVolumePoint struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

// conversationVolume buckets interaction episodes by day over the last
// days window — the simplest possible "how much are you using this"
// signal, straight off episodes.ts (plaintext).
func conversationVolume(ctx context.Context, db *sql.DB, scope identity.Scope, days int) ([]ConversationVolumePoint, error) {
	var points []ConversationVolumePoint
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select to_char(date_trunc('day', ts), 'YYYY-MM-DD') as day, count(*)
			from episodes
			where type = 'interaction' and scope_kind = $1 and scope_owner = $2
			  and ts >= now() - make_interval(days => $3)
			group by 1
			order by 1
		`, scope.Kind, scope.Owner, days)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p ConversationVolumePoint
			if err := rows.Scan(&p.Day, &p.Count); err != nil {
				return err
			}
			points = append(points, p)
		}
		return rows.Err()
	})
	return points, err
}

// ThemeWordCloudEntry is one entity's mention frequency across current
// summaries — the word cloud/theme signal, built entirely from
// entities.name (plaintext) and summaries.entities_touched (plaintext
// text[]), never from decrypted summary prose or episode text.
type ThemeWordCloudEntry struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Mentions int    `json:"mentions"`
}

// themeWordCloud is the read-only counterpart to
// internal/consolidation/contradiction.go's findRelatedSummaries, which
// already uses this exact unnest(entities_touched) technique to find
// shared entities between summaries — here used to count mentions
// instead of matching them. The entities join has no explicit
// scope_kind/scope_owner condition: entities' own row-level security
// (dbscope.Run's session vars) already restricts every visible row to
// this exact scope, and a comma-join's set-returning FROM item can't
// reference an outer table from inside a later explicit JOIN's ON
// clause anyway (Postgres FROM-list scoping, not an RLS workaround).
func themeWordCloud(ctx context.Context, db *sql.DB, scope identity.Scope, limit int) ([]ThemeWordCloudEntry, error) {
	var entries []ThemeWordCloudEntry
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select e.id, e.name, e.kind, count(*) as mentions
			from summaries s, unnest(s.entities_touched) as touched(entity_id)
			join entities e on e.id = touched.entity_id
			where s.scope_kind = $1 and s.scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
			group by e.id, e.name, e.kind
			order by mentions desc, e.name
			limit $3
		`, scope.Kind, scope.Owner, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e ThemeWordCloudEntry
			if err := rows.Scan(&e.EntityID, &e.Name, &e.Kind, &e.Mentions); err != nil {
				return err
			}
			entries = append(entries, e)
		}
		return rows.Err()
	})
	return entries, err
}

// EntityRelationship mirrors entity_relationships' plaintext columns
// directly (schema/0015_entity_relationships.sql) — nothing in this
// table is encrypted, see that migration's own doc comment.
type EntityRelationship struct {
	SubjectID  string    `json:"subject_id"`
	Predicate  string    `json:"predicate"`
	ObjectID   string    `json:"object_id"`
	ValidFrom  *string   `json:"valid_from,omitempty"`
	ValidUntil *string   `json:"valid_until,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

func entityRelationshipGraph(ctx context.Context, db *sql.DB, scope identity.Scope, limit int) ([]EntityRelationship, error) {
	var rels []EntityRelationship
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select subject_id, predicate, object_id,
			       to_char(valid_from, 'YYYY-MM-DD'), to_char(valid_until, 'YYYY-MM-DD'), created_at
			from entity_relationships
			where scope_kind = $1 and scope_owner = $2
			order by created_at desc
			limit $3
		`, scope.Kind, scope.Owner, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rel EntityRelationship
			var validFrom, validUntil sql.NullString
			if err := rows.Scan(&rel.SubjectID, &rel.Predicate, &rel.ObjectID, &validFrom, &validUntil, &rel.CreatedAt); err != nil {
				return err
			}
			if validFrom.Valid {
				rel.ValidFrom = &validFrom.String
			}
			if validUntil.Valid {
				rel.ValidUntil = &validUntil.String
			}
			rels = append(rels, rel)
		}
		return rows.Err()
	})
	return rels, err
}

// MemoryHealth summarizes how much of a scope's history exists and how
// current it is — scope_corpus_size (updated daily by consolidation,
// internal/consolidation/runner.go's updateScopeCorpusSize) plus a direct
// count of auto-corrected contradictions.
type MemoryHealth struct {
	EpisodeCount        int        `json:"episode_count"`
	SummaryCount        int        `json:"summary_count"`
	CorpusSizeUpdatedAt *time.Time `json:"corpus_size_updated_at,omitempty"`
	LastSummaryAt       *time.Time `json:"last_summary_at,omitempty"`
	CorrectionsCount    int        `json:"corrections_count"`
}

func memoryHealth(ctx context.Context, db *sql.DB, scope identity.Scope) (MemoryHealth, error) {
	var h MemoryHealth
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var updatedAt sql.NullTime
		err := tx.QueryRowContext(ctx, `
			select episode_count, summary_count, updated_at from scope_corpus_size
			where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&h.EpisodeCount, &h.SummaryCount, &updatedAt)
		// A brand-new scope with no consolidation run yet has no row at
		// all — zero counts, not an error, same default
		// internal/store/retrieve.go's keywordSearchTierForScope uses for
		// the identical "no row yet" case.
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if updatedAt.Valid {
			h.CorpusSizeUpdatedAt = &updatedAt.Time
		}

		var lastSummary sql.NullTime
		if err := tx.QueryRowContext(ctx, `
			select max(created_at) from summaries where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&lastSummary); err != nil {
			return err
		}
		if lastSummary.Valid {
			h.LastSummaryAt = &lastSummary.Time
		}

		return tx.QueryRowContext(ctx, `
			select count(*) from summaries
			where scope_kind = $1 and scope_owner = $2 and correction_reason is not null
		`, scope.Kind, scope.Owner).Scan(&h.CorrectionsCount)
	})
	return h, err
}

// KeywordSearchGovernance surfaces internal/store/retrieve.go's own
// keywordSearchTierForScope decision for this scope — the same query and
// thresholds that file uses, duplicated here rather than imported
// because keywordSearchTierForScope is unexported (internal/store's own
// package) and this dashboard has no other reason to depend on
// internal/store (which also requires a provider.Embedder to construct a
// Store, which this read-only dashboard doesn't have). Keep these two in
// sync if the thresholds or table ever change.
type KeywordSearchGovernance struct {
	Tier             string `json:"tier"`
	TotalCorpusSize  int    `json:"total_corpus_size"`
	NarrowThreshold  int    `json:"narrow_threshold"`
	DisableThreshold int    `json:"disable_threshold"`
}

func keywordSearchGovernance(ctx context.Context, db *sql.DB, scope identity.Scope) (KeywordSearchGovernance, error) {
	g := KeywordSearchGovernance{
		NarrowThreshold:  envInt("HUPI_KEYWORD_SEARCH_NARROW_THRESHOLD", 500),
		DisableThreshold: envInt("HUPI_KEYWORD_SEARCH_DISABLE_THRESHOLD", 3000),
	}
	if os.Getenv("HUPI_ENABLE_KEYWORD_SEARCH") == "false" {
		g.Tier = "disabled"
		return g, nil
	}
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var episodeCount, summaryCount int
		err := tx.QueryRowContext(ctx, `
			select episode_count, summary_count from scope_corpus_size
			where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&episodeCount, &summaryCount)
		if err == sql.ErrNoRows {
			g.Tier = "full"
			return nil
		}
		if err != nil {
			return err
		}
		g.TotalCorpusSize = episodeCount + summaryCount
		switch {
		case g.TotalCorpusSize > g.DisableThreshold:
			g.Tier = "disabled"
		case g.TotalCorpusSize > g.NarrowThreshold:
			g.Tier = "narrowed"
		default:
			g.Tier = "full"
		}
		return nil
	})
	return g, err
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// SecurityEvent is one audit_log row relevant to this scope's security
// posture — key rotations, exports, imports, corrections.
type SecurityEvent struct {
	TS        time.Time `json:"ts"`
	EventType string    `json:"event_type"`
	Actor     string    `json:"actor"`
}

// KeyRotationStatus mirrors key_rotations' plaintext columns.
type KeyRotationStatus struct {
	FromVersion int        `json:"from_version"`
	ToVersion   int        `json:"to_version"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type SecurityPosture struct {
	KeyRotation  *KeyRotationStatus `json:"key_rotation,omitempty"`
	RecentEvents []SecurityEvent    `json:"recent_events"`
}

// securityPosture answers "are users using it securely": real key
// rotation status plus recent security-relevant audit events for this
// scope. audit_log has NO row-level security restricting select (its own
// migration's comment: "SELECT has NO scope-restricting policy — audit_log's
// entire purpose is cross-scope visibility for admin tools") — every
// caller is responsible for its own scope filter. audit.Query's
// ScopeKind/ScopeOwner filter is exactly that filter; never call it here
// without both set, or a Tier 3 user would see every other user's audit
// trail.
func securityPosture(ctx context.Context, db *sql.DB, scope identity.Scope, limit int) (SecurityPosture, error) {
	var p SecurityPosture

	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		var k KeyRotationStatus
		var completedAt sql.NullTime
		err := tx.QueryRowContext(ctx, `
			select from_version, to_version, status, started_at, completed_at
			from key_rotations where scope_kind = $1 and scope_owner = $2
		`, scope.Kind, scope.Owner).Scan(&k.FromVersion, &k.ToVersion, &k.Status, &k.StartedAt, &completedAt)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if completedAt.Valid {
			k.CompletedAt = &completedAt.Time
		}
		p.KeyRotation = &k
		return nil
	})
	if err != nil {
		return p, err
	}

	entries, err := audit.Query(ctx, db, audit.QueryFilter{
		ScopeKind:  scope.Kind,
		ScopeOwner: scope.Owner,
		Limit:      limit,
	})
	if err != nil {
		return p, err
	}
	for _, e := range entries {
		if !securityRelevantEvent(e.EventType) {
			continue
		}
		p.RecentEvents = append(p.RecentEvents, SecurityEvent{TS: e.TS, EventType: e.EventType, Actor: e.Actor})
	}
	return p, nil
}

// securityRelevantEvent is this panel's own allowlist — audit_log also
// carries "retrieve"/"trace" events, which are about normal usage, not
// security posture.
func securityRelevantEvent(eventType string) bool {
	switch eventType {
	case "export", "import", "key_rotation", "correct", "dashboard_login":
		return true
	default:
		return false
	}
}
