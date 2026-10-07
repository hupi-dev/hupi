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
	"hupi/internal/pgfmt"
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

// EntityRelationshipWithNames is entityRelationshipGraph's data joined
// to entities for display names/kinds, plus a date-range filter — built
// for the memory-map feature (memory_map.go), which needs to show
// "Alex" not "person:alex" and needs to scope the graph to whatever
// window the user picked. Kept as a separate function rather than
// changing entityRelationshipGraph itself: that function's shape is the
// live, committed /api/entity-relationships response contract
// (web/src/lib/api.ts mirrors it exactly) — changing it would silently
// break that existing panel.
type EntityRelationshipWithNames struct {
	SubjectID   string    `json:"subject_id"`
	SubjectName string    `json:"subject_name"`
	SubjectKind string    `json:"subject_kind"`
	Predicate   string    `json:"predicate"`
	ObjectID    string    `json:"object_id"`
	ObjectName  string    `json:"object_name"`
	ObjectKind  string    `json:"object_kind"`
	ValidFrom   *string   `json:"valid_from,omitempty"`
	ValidUntil  *string   `json:"valid_until,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// entityRelationshipGraphInRange filters by the relationship's own
// bi-temporal validity window (valid_from/valid_until), not by when it
// was recorded (source_summary_id -> summaries.created_at) —
// deliberately: a relationship that's still true, or was true with an
// unknown start date, shouldn't disappear just because the view is
// zoomed into a narrow recent range. coalesce to -infinity/infinity so
// a null (unknown-start or still-current) bound always overlaps any
// requested window.
func entityRelationshipGraphInRange(ctx context.Context, db *sql.DB, scope identity.Scope, from, to time.Time, limit int) ([]EntityRelationshipWithNames, error) {
	var rels []EntityRelationshipWithNames
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select er.subject_id, se.name, se.kind, er.predicate,
			       er.object_id, oe.name, oe.kind,
			       to_char(er.valid_from, 'YYYY-MM-DD'), to_char(er.valid_until, 'YYYY-MM-DD'), er.created_at
			from entity_relationships er
			join entities se on se.scope_kind = er.scope_kind and se.scope_owner = er.scope_owner and se.id = er.subject_id
			join entities oe on oe.scope_kind = er.scope_kind and oe.scope_owner = er.scope_owner and oe.id = er.object_id
			where er.scope_kind = $1 and er.scope_owner = $2
			  and coalesce(er.valid_from, '-infinity'::date) <= $3::date
			  and coalesce(er.valid_until, 'infinity'::date) >= $4::date
			order by er.created_at desc
			limit $5
		`, scope.Kind, scope.Owner, to, from, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rel EntityRelationshipWithNames
			var validFrom, validUntil sql.NullString
			if err := rows.Scan(&rel.SubjectID, &rel.SubjectName, &rel.SubjectKind, &rel.Predicate,
				&rel.ObjectID, &rel.ObjectName, &rel.ObjectKind, &validFrom, &validUntil, &rel.CreatedAt); err != nil {
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

// ConversationNode is one interaction episode as the memory map's
// "conversation" node — plaintext only (id/timestamp/importance); the
// decrypted excerpt/topics overlay is a separate, decrypt-gated
// endpoint (content_analysis.go's decryptConversations, wired up in
// memory_map.go).
type ConversationNode struct {
	EpisodeID  string    `json:"episode_id"`
	TS         time.Time `json:"ts"`
	Importance *float64  `json:"importance,omitempty"`
}

// conversationsInRange is the memory map's conversation-node source —
// same filter shape conversationVolume already uses, ordered by
// recency and capped so a wide-open "all time" window can't return an
// unbounded result.
func conversationsInRange(ctx context.Context, db *sql.DB, scope identity.Scope, from, to time.Time, limit int) ([]ConversationNode, error) {
	var nodes []ConversationNode
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select id, ts, importance from episodes
			where type = 'interaction' and scope_kind = $1 and scope_owner = $2
			  and ts >= $3 and ts < $4
			order by ts desc
			limit $5
		`, scope.Kind, scope.Owner, from, to, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n ConversationNode
			var importance sql.NullFloat64
			if err := rows.Scan(&n.EpisodeID, &n.TS, &importance); err != nil {
				return err
			}
			if importance.Valid {
				n.Importance = &importance.Float64
			}
			nodes = append(nodes, n)
		}
		return rows.Err()
	})
	return nodes, err
}

// ConversationEntityMention links one conversation to one entity it
// mentions — the memory map's conversation->entity edge source.
type ConversationEntityMention struct {
	EpisodeID  string `json:"episode_id"`
	EntityID   string `json:"entity_id"`
	EntityName string `json:"entity_name"`
	EntityKind string `json:"entity_kind"`
}

// conversationEntityMentions is the only plaintext link from an
// individual episode to the entities it touched: episodes have no
// direct entity FK of their own, only the owning DAILY summary's
// entities_touched carries that (weekly/monthly/yearly summaries roll
// up via source_summary_periods instead and never carry
// source_episode_ids — see internal/consolidation/runner.go). Takes the
// exact episode id list conversationsInRange just returned, not an
// independent query, so every mention edge this produces targets a
// conversation node actually present in the same response. Reuses
// themeWordCloud's own unnest(entities_touched) + "current summary
// only" technique, just scoped to specific episode ids instead of the
// whole scope; the entities join has no explicit scope condition for
// the same reason themeWordCloud's doesn't — RLS already restricts
// every visible row to this scope.
func conversationEntityMentions(ctx context.Context, db *sql.DB, scope identity.Scope, episodeIDs []string) ([]ConversationEntityMention, error) {
	if len(episodeIDs) == 0 {
		return nil, nil
	}
	var mentions []ConversationEntityMention
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select ep.id, e.id, e.name, e.kind
			from episodes ep
			join summaries s
			  on s.scope_kind = ep.scope_kind and s.scope_owner = ep.scope_owner
			 and s.level = 'daily' and ep.id = any(s.source_episode_ids)
			 and not exists (select 1 from summaries newer where newer.supersedes = s.id)
			join lateral unnest(s.entities_touched) as touched(entity_id) on true
			join entities e on e.id = touched.entity_id
			where ep.scope_kind = $1 and ep.scope_owner = $2 and ep.id = any($3::text[])
		`, scope.Kind, scope.Owner, pgfmt.TextArray(episodeIDs))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m ConversationEntityMention
			if err := rows.Scan(&m.EpisodeID, &m.EntityID, &m.EntityName, &m.EntityKind); err != nil {
				return err
			}
			mentions = append(mentions, m)
		}
		return rows.Err()
	})
	return mentions, err
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

// defaultForgottenMinImportance/-StaleDays are reasoned, not measured —
// same honest status as this file's other UI-facing defaults
// (summaryMaxResults-style constants in internal/store). This is a
// display panel, not a retrieval-affecting decision, so unlike
// keywordSearchNarrowThreshold these aren't env-var overridable — a
// caller that wants different values just passes different query
// params (handlers.go's queryFloat/queryInt already support that).
const (
	defaultForgottenMinImportance = 0.7
	defaultForgottenStaleDays     = 30
)

// ForgottenEpisode is a high-importance episode nothing recent has
// surfaced again — importance alone (episodes.importance, plaintext)
// plus how long ago it happened.
type ForgottenEpisode struct {
	ID         string    `json:"id"`
	Importance float64   `json:"importance"`
	TS         time.Time `json:"ts"`
}

// StaleEntity is an entity nothing has touched in a while — last_updated
// (plaintext, bumped every time storeSummary's upsertEntities touches
// it) is the only "still relevant" signal entities carry; there's no
// importance score on entities the way there is on episodes.
type StaleEntity struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	LastUpdated string `json:"last_updated"`
}

type ForgottenButImportant struct {
	Episodes []ForgottenEpisode `json:"episodes"`
	Entities []StaleEntity      `json:"entities"`
}

// forgottenButImportant surfaces exactly what its name says: episodes
// whose captured importance cleared minImportance but haven't happened
// again in staleDays, and entities nothing has touched in that same
// window — the "you flagged this as important, are you sure it's not
// still relevant" nudge the plan's own design called "what the future
// looks like."
func forgottenButImportant(ctx context.Context, db *sql.DB, scope identity.Scope, minImportance float64, staleDays, limit int) (ForgottenButImportant, error) {
	var result ForgottenButImportant
	err := dbscope.Run(ctx, db, scope, scope, func(tx *sql.Tx) error {
		epRows, err := tx.QueryContext(ctx, `
			select id, importance, ts from episodes
			where type = 'interaction' and scope_kind = $1 and scope_owner = $2
			  and importance >= $3 and ts < now() - make_interval(days => $4)
			order by importance desc, ts desc
			limit $5
		`, scope.Kind, scope.Owner, minImportance, staleDays, limit)
		if err != nil {
			return err
		}
		defer epRows.Close()
		for epRows.Next() {
			var e ForgottenEpisode
			if err := epRows.Scan(&e.ID, &e.Importance, &e.TS); err != nil {
				return err
			}
			result.Episodes = append(result.Episodes, e)
		}
		if err := epRows.Err(); err != nil {
			return err
		}

		entRows, err := tx.QueryContext(ctx, `
			select id, name, kind, to_char(last_updated, 'YYYY-MM-DD') from entities
			where scope_kind = $1 and scope_owner = $2
			  and last_updated < current_date - $3::int
			order by last_updated asc
			limit $4
		`, scope.Kind, scope.Owner, staleDays, limit)
		if err != nil {
			return err
		}
		defer entRows.Close()
		for entRows.Next() {
			var e StaleEntity
			if err := entRows.Scan(&e.ID, &e.Name, &e.Kind, &e.LastUpdated); err != nil {
				return err
			}
			result.Entities = append(result.Entities, e)
		}
		return entRows.Err()
	})
	return result, err
}

// KeywordSearchGovernance surfaces internal/store/retrieve.go's own
// keywordSearchTierForScope decision for this scope — the same query and
// thresholds that file uses, duplicated here rather than imported because
// keywordSearchTierForScope is unexported (internal/store's own
// package). handleExport does construct a real *store.Store (it has a
// provider.Embedder available via server.registry), but this function
// still doesn't reuse it — keywordSearchTierForScope isn't exported
// regardless of whether a Store is in hand. Keep these two in sync if
// the thresholds or table ever change.
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
