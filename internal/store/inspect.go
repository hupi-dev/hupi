package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"hupi/internal/dbscope"
	"hupi/internal/entityattrs"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// ReadAudit labels a memory read in its audit row (e.g. Via "mcp", Tool "get_entity").
type ReadAudit struct {
	Via  string
	Tool string
}

func (a ReadAudit) detail(extra map[string]any) map[string]any {
	d := map[string]any{}
	if a.Via != "" {
		d["via"] = a.Via
	}
	if a.Tool != "" {
		d["tool"] = a.Tool
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// EntityRow is an entity's plaintext identity columns.
type EntityRow struct {
	ID          string
	Kind        string
	Name        string
	FirstSeen   time.Time
	LastUpdated time.Time
}

// EntityRelationship is one entity_relationships row with both ends' names.
type EntityRelationship struct {
	SubjectID, SubjectName, SubjectKind string
	Predicate                           string
	ObjectID, ObjectName, ObjectKind    string
	ValidFrom, ValidUntil               *string // YYYY-MM-DD; nil ValidUntil = still current
}

// EntityLookup is LookupEntity's result: either a resolved Entity (with
// its current grounded attributes and relationships) or, when the name
// was ambiguous, Candidates to choose from. Neither set means not found.
type EntityLookup struct {
	Entity        *EntityRow
	Attributes    []entityattrs.Attribute
	Relationships []EntityRelationship
	Candidates    []EntityRow
}

const (
	maxEntityCandidates    = 6
	maxEntityRelationships = 50
)

// LookupEntity resolves nameOrID — an exact entity id, then a
// case-insensitive exact name, then a name/id substring — within
// workspace. self_model is never returned: it's the profile anchor, not a
// lookup-able entity.
func (s *Store) LookupEntity(ctx context.Context, actingUser, workspace identity.Scope, nameOrID string, ra ReadAudit) (EntityLookup, error) {
	var out EntityLookup
	nameOrID = strings.TrimSpace(nameOrID)
	if nameOrID == "" {
		return out, nil
	}
	err := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		matches, err := queryEntityRows(ctx, tx, `
			select id, kind, name, first_seen, last_updated from entities
			where scope_kind = $1 and scope_owner = $2 and kind <> 'self_model' and id = $3
		`, workspace.Kind, workspace.Owner, nameOrID)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			matches, err = queryEntityRows(ctx, tx, `
				select id, kind, name, first_seen, last_updated from entities
				where scope_kind = $1 and scope_owner = $2 and kind <> 'self_model' and lower(name) = lower($3)
				order by last_updated desc, id limit $4
			`, workspace.Kind, workspace.Owner, nameOrID, maxEntityCandidates)
			if err != nil {
				return err
			}
		}
		if len(matches) == 0 {
			matches, err = queryEntityRows(ctx, tx, `
				select id, kind, name, first_seen, last_updated from entities
				where scope_kind = $1 and scope_owner = $2 and kind <> 'self_model'
				  and (name ilike $3 escape '\' or id ilike $3 escape '\')
				order by last_updated desc, id limit $4
			`, workspace.Kind, workspace.Owner, "%"+escapeLike(nameOrID)+"%", maxEntityCandidates)
			if err != nil {
				return err
			}
		}
		switch {
		case len(matches) == 0:
			return nil
		case len(matches) > 1:
			out.Candidates = matches
			return nil
		}

		entity := matches[0]
		out.Entity = &entity
		attrs, err := entityattrs.CurrentWithIDsForEntities(ctx, tx, s.keys, workspace, []string{entity.ID})
		if err != nil {
			return fmt.Errorf("load attributes for %s: %w", entity.ID, err)
		}
		out.Attributes = attrs[entity.ID]
		out.Relationships, err = entityRelationshipsFor(ctx, tx, workspace, entity.ID)
		return err
	})
	if err != nil {
		return EntityLookup{}, fmt.Errorf("store: lookup entity: %w", err)
	}
	resolved := ""
	if out.Entity != nil {
		resolved = out.Entity.ID
	}
	s.auditRead(ctx, actingUser, workspace, ra.detail(map[string]any{
		"read":       "lookup_entity",
		"resolved":   resolved,
		"candidates": len(out.Candidates),
	}))
	return out, nil
}

func queryEntityRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]EntityRow, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntityRow
	for rows.Next() {
		var e EntityRow
		if err := rows.Scan(&e.ID, &e.Kind, &e.Name, &e.FirstSeen, &e.LastUpdated); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// entityRelationshipsFor returns relationships where entityID is either
// end, still-current ones first.
func entityRelationshipsFor(ctx context.Context, tx *sql.Tx, workspace identity.Scope, entityID string) ([]EntityRelationship, error) {
	rows, err := tx.QueryContext(ctx, `
		select er.subject_id, se.name, se.kind, er.predicate,
		       er.object_id, oe.name, oe.kind,
		       to_char(er.valid_from, 'YYYY-MM-DD'), to_char(er.valid_until, 'YYYY-MM-DD')
		from entity_relationships er
		join entities se on se.scope_kind = er.scope_kind and se.scope_owner = er.scope_owner and se.id = er.subject_id
		join entities oe on oe.scope_kind = er.scope_kind and oe.scope_owner = er.scope_owner and oe.id = er.object_id
		where er.scope_kind = $1 and er.scope_owner = $2
		  and (er.subject_id = $3 or er.object_id = $3)
		order by (er.valid_until is null) desc, er.created_at desc
		limit $4
	`, workspace.Kind, workspace.Owner, entityID, maxEntityRelationships)
	if err != nil {
		return nil, fmt.Errorf("load relationships for %s: %w", entityID, err)
	}
	defer rows.Close()
	var out []EntityRelationship
	for rows.Next() {
		var r EntityRelationship
		var from, until sql.NullString
		if err := rows.Scan(&r.SubjectID, &r.SubjectName, &r.SubjectKind, &r.Predicate,
			&r.ObjectID, &r.ObjectName, &r.ObjectKind, &from, &until); err != nil {
			return nil, err
		}
		if from.Valid {
			r.ValidFrom = &from.String
		}
		if until.Valid {
			r.ValidUntil = &until.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// escapeLike escapes LIKE/ILIKE metacharacters so user input matches literally.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// EntityFilter narrows ListEntities. Empty fields don't filter.
type EntityFilter struct {
	Kind         string
	NameContains string
	Limit        int
}

// ListEntities returns workspace's entities, most recently updated first.
// Plaintext columns only — no decryption. Returns up to Limit+1 rows so a
// caller can tell whether more exist.
func (s *Store) ListEntities(ctx context.Context, actingUser, workspace identity.Scope, f EntityFilter, ra ReadAudit) ([]EntityRow, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	name := ""
	if strings.TrimSpace(f.NameContains) != "" {
		name = "%" + escapeLike(strings.TrimSpace(f.NameContains)) + "%"
	}
	var out []EntityRow
	err := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		var err error
		out, err = queryEntityRows(ctx, tx, `
			select id, kind, name, first_seen, last_updated from entities
			where scope_kind = $1 and scope_owner = $2 and kind <> 'self_model'
			  and ($3 = '' or kind = $3)
			  and ($4 = '' or name ilike $4 escape '\')
			order by last_updated desc, name
			limit $5
		`, workspace.Kind, workspace.Owner, f.Kind, name, f.Limit+1)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list entities: %w", err)
	}
	s.auditRead(ctx, actingUser, workspace, ra.detail(map[string]any{"read": "list_entities", "count": len(out)}))
	return out, nil
}

// RecentQuery bounds RecentSummaries.
type RecentQuery struct {
	Since time.Time
	Limit int
}

// KeyFactRecord is one grounded, unexpired key fact of a summary.
type KeyFactRecord struct {
	MemoryID    string
	Text        string
	IsInference bool
}

// SummaryRecord is one current daily summary, decrypted.
type SummaryRecord struct {
	ID              string
	Period          string
	Status          string
	Summary         string
	EntitiesTouched []string
	KeyFacts        []KeyFactRecord
}

// RecentResult is RecentSummaries' result. UnconsolidatedExchanges counts
// interaction episodes newer than the latest daily summary's period —
// captured, but not yet folded into any summary by consolidation.
type RecentResult struct {
	Summaries               []SummaryRecord
	UnconsolidatedExchanges int
}

// RecentSummaries returns workspace's current (not superseded) daily
// summaries from q.Since onward, newest first, with their grounded,
// unexpired key facts. Daily level only: rollup periods aren't dates.
func (s *Store) RecentSummaries(ctx context.Context, actingUser, workspace identity.Scope, q RecentQuery, now time.Time, ra ReadAudit) (RecentResult, error) {
	if q.Limit <= 0 {
		q.Limit = 7
	}
	var out RecentResult
	err := dbscope.Run(ctx, s.db, actingUser, workspace, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			select s.id, s.period, s.status, s.summary, s.key_version, s.entities_touched
			from summaries s
			where s.scope_kind = $1 and s.scope_owner = $2 and s.level = 'daily' and s.period >= $3
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
			order by s.period desc, s.id desc
			limit $4
		`, workspace.Kind, workspace.Owner, q.Since.Format("2006-01-02"), q.Limit)
		if err != nil {
			return fmt.Errorf("load recent summaries: %w", err)
		}
		type encSummary struct {
			rec        SummaryRecord
			ciphertext []byte
			keyVersion int
		}
		var encrypted []encSummary
		for rows.Next() {
			var e encSummary
			var touched string
			if err := rows.Scan(&e.rec.ID, &e.rec.Period, &e.rec.Status, &e.ciphertext, &e.keyVersion, &touched); err != nil {
				rows.Close()
				return err
			}
			e.rec.EntitiesTouched = pgfmt.ParseTextArray(touched)
			encrypted = append(encrypted, e)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		ids := make([]string, len(encrypted))
		for i, e := range encrypted {
			ids[i] = e.rec.ID
			if e.ciphertext == nil {
				continue
			}
			enc, err := s.keys.GetVersion(ctx, workspace, e.keyVersion)
			if err != nil {
				return fmt.Errorf("resolve key for summary %s: %w", e.rec.ID, err)
			}
			text, err := enc.Decrypt(e.ciphertext)
			if err != nil {
				return fmt.Errorf("decrypt summary %s: %w", e.rec.ID, err)
			}
			encrypted[i].rec.Summary = text
		}

		facts, err := s.keyFactsForSummaries(ctx, tx, workspace, ids, now)
		if err != nil {
			return err
		}
		for _, e := range encrypted {
			e.rec.KeyFacts = facts[e.rec.ID]
			out.Summaries = append(out.Summaries, e.rec)
		}

		var latest sql.NullString
		if err := tx.QueryRowContext(ctx, `
			select max(period) from summaries
			where scope_kind = $1 and scope_owner = $2 and level = 'daily'
		`, workspace.Kind, workspace.Owner).Scan(&latest); err != nil {
			return fmt.Errorf("load latest daily period: %w", err)
		}
		if latest.Valid {
			err = tx.QueryRowContext(ctx, `
				select count(*) from episodes
				where type = 'interaction' and scope_kind = $1 and scope_owner = $2
				  and ts >= ($3::date + 1)
			`, workspace.Kind, workspace.Owner, latest.String).Scan(&out.UnconsolidatedExchanges)
		} else {
			err = tx.QueryRowContext(ctx, `
				select count(*) from episodes
				where type = 'interaction' and scope_kind = $1 and scope_owner = $2
			`, workspace.Kind, workspace.Owner).Scan(&out.UnconsolidatedExchanges)
		}
		if err != nil {
			return fmt.Errorf("count unconsolidated exchanges: %w", err)
		}
		return nil
	})
	if err != nil {
		return RecentResult{}, fmt.Errorf("store: recent summaries: %w", err)
	}
	s.auditRead(ctx, actingUser, workspace, ra.detail(map[string]any{"read": "recent_summaries", "count": len(out.Summaries)}))
	return out, nil
}

// keyFactsForSummaries loads grounded, unexpired key facts for the given
// summaries — the same filters retrieval's loadKeyFacts applies — each
// decrypted with its own key_version.
func (s *Store) keyFactsForSummaries(ctx context.Context, tx *sql.Tx, workspace identity.Scope, summaryIDs []string, now time.Time) (map[string][]KeyFactRecord, error) {
	out := map[string][]KeyFactRecord{}
	if len(summaryIDs) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `
		select id, summary_id, fact, key_version, is_inference
		from summary_key_facts
		where summary_id = any($1::text[]) and grounded = true
		  and (expires_at is null or expires_at >= $2::date)
		order by summary_id, id
	`, pgfmt.TextArray(summaryIDs), now.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("load key facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var summaryID string
		var ciphertext []byte
		var keyVersion int
		var isInference bool
		if err := rows.Scan(&id, &summaryID, &ciphertext, &keyVersion, &isInference); err != nil {
			return nil, err
		}
		enc, err := s.keys.GetVersion(ctx, workspace, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve key for key fact %d: %w", id, err)
		}
		text, err := enc.Decrypt(ciphertext)
		if err != nil {
			return nil, fmt.Errorf("decrypt key fact %d: %w", id, err)
		}
		out[summaryID] = append(out[summaryID], KeyFactRecord{MemoryID: memoryIDForKeyFact(id), Text: text, IsInference: isInference})
	}
	return out, rows.Err()
}
