package store

import (
	"context"
	"database/sql"
	"fmt"

	"hupi/internal/audit"
	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// MemoryExport is the decrypted, complete contents of a scope's memory
// store — every entity, every *current* (non-superseded) summary with
// its key facts, and every relationship. This exists specifically for
// external diagnostic tooling (see docs/EVALMEM_INTEGRATION_PLAN.md)
// that needs to answer "is this information anywhere in the memory
// store at all" independent of whatever a single Retrieve() call's
// ranking happens to surface — Trace (trace.go) answers "what did this
// one turn see"; ExportMemory answers "what does this scope know,
// full stop."
type MemoryExport struct {
	Scope         identity.Scope         `json:"scope"`
	Entities      []ExportedEntity       `json:"entities"`
	Summaries     []ExportedSummary      `json:"summaries"`
	Relationships []ExportedRelationship `json:"relationships"`
}

type ExportedEntity struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Attributes string `json:"attributes"`
}

// ExportedKeyFact's fields beyond Fact/Grounded were missing until this
// was found as a diagnostic-tooling gap alongside internal/hpmf's own
// identical one (docs/DESIGN_VS_BUILT.md #8): a scope's full memory dump
// silently omitted source_count/expires_at/expire_reason/is_inference —
// everything docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md added to a key
// fact's record.
type ExportedKeyFact struct {
	Fact         string `json:"fact"`
	Grounded     bool   `json:"grounded"`
	SourceCount  int    `json:"source_count"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	ExpireReason string `json:"expire_reason,omitempty"`
	IsInference  bool   `json:"is_inference"`
}

type ExportedSummary struct {
	ID               string            `json:"id"`
	Period           string            `json:"period"`
	Level            string            `json:"level"`
	GroundingChecked bool              `json:"grounding_checked"`
	Text             string            `json:"text"`
	KeyFacts         []ExportedKeyFact `json:"key_facts"`
}

type ExportedRelationship struct {
	ID         string `json:"id"`
	SubjectID  string `json:"subject_id"`
	Predicate  string `json:"predicate"`
	ObjectID   string `json:"object_id"`
	ValidFrom  string `json:"valid_from"`  // "" if never stated
	ValidUntil string `json:"valid_until"` // "" if still current
}

// ExportMemory decrypts and returns everything a scope's memory store
// currently holds. "Current" for summaries and matches Retrieve()'s own
// definition — not exists(select 1 from summaries newer where
// newer.supersedes = s.id) — so this reports the same live state
// Retrieve() itself can ever reach, not stale corrected-away drafts.
// Relationships are returned in full, validity windows included, rather
// than pre-filtered to "still open" — a caller diagnosing temporal
// reasoning needs the closed edges too, not just the current ones.
//
// actor is recorded on the audit log the same way Trace's is — exporting
// a scope's entire memory content is at least as sensitive an access as
// tracing one episode.
func (s *Store) ExportMemory(ctx context.Context, scope identity.Scope, actor string) (MemoryExport, error) {
	export := MemoryExport{Scope: scope}

	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		return audit.Write(ctx, tx, audit.Entry{
			EventType:      audit.EventTrace,
			Actor:          actor,
			ActingScope:    scope,
			WorkspaceScope: scope,
			TargetRef:      &identity.Ref{Kind: identity.RefKindEntity, Scope: scope, ID: "*"},
		})
	})
	if err != nil {
		return MemoryExport{}, fmt.Errorf("store: audit memory export for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	entities, err := s.exportEntities(ctx, scope)
	if err != nil {
		return MemoryExport{}, err
	}
	export.Entities = entities

	summaries, err := s.exportSummaries(ctx, scope)
	if err != nil {
		return MemoryExport{}, err
	}
	export.Summaries = summaries

	relationships, err := s.exportRelationships(ctx, scope)
	if err != nil {
		return MemoryExport{}, err
	}
	export.Relationships = relationships

	return export, nil
}

func (s *Store) exportEntities(ctx context.Context, scope identity.Scope) ([]ExportedEntity, error) {
	type row struct {
		id, kind, name string
		attrsCT        []byte
		keyVersion     int
	}
	var rows []row
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		res, err := tx.QueryContext(ctx, `
			select id, kind, name, attributes, key_version
			from entities where scope_kind = $1 and scope_owner = $2
			order by id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var r row
			if err := res.Scan(&r.id, &r.kind, &r.name, &r.attrsCT, &r.keyVersion); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: export entities for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	out := make([]ExportedEntity, 0, len(rows))
	for _, r := range rows {
		enc, err := s.keys.GetVersion(ctx, scope, r.keyVersion)
		if err != nil {
			return nil, fmt.Errorf("store: resolve encryption key for entity %s: %w", r.id, err)
		}
		attrs, err := enc.Decrypt(r.attrsCT)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt entity %s attributes: %w", r.id, err)
		}
		out = append(out, ExportedEntity{ID: r.id, Kind: r.kind, Name: r.name, Attributes: attrs})
	}
	return out, nil
}

func (s *Store) exportSummaries(ctx context.Context, scope identity.Scope) ([]ExportedSummary, error) {
	type row struct {
		id, period, level string
		groundingChecked  bool
		summaryCT         []byte
		keyVersion        int
	}
	var rows []row
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		res, err := tx.QueryContext(ctx, `
			select id, period, level, grounding_checked, summary, key_version
			from summaries s
			where scope_kind = $1 and scope_owner = $2
			  and not exists (select 1 from summaries newer where newer.supersedes = s.id)
			order by period
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var r row
			if err := res.Scan(&r.id, &r.period, &r.level, &r.groundingChecked, &r.summaryCT, &r.keyVersion); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: export summaries for %s:%s: %w", scope.Kind, scope.Owner, err)
	}

	out := make([]ExportedSummary, 0, len(rows))
	for _, r := range rows {
		enc, err := s.keys.GetVersion(ctx, scope, r.keyVersion)
		if err != nil {
			return nil, fmt.Errorf("store: resolve encryption key for summary %s: %w", r.id, err)
		}
		text, err := enc.Decrypt(r.summaryCT)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt summary %s: %w", r.id, err)
		}
		keyFacts, err := s.exportKeyFacts(ctx, scope, r.id, enc)
		if err != nil {
			return nil, err
		}
		out = append(out, ExportedSummary{
			ID: r.id, Period: r.period, Level: r.level,
			GroundingChecked: r.groundingChecked, Text: text, KeyFacts: keyFacts,
		})
	}
	return out, nil
}

func (s *Store) exportKeyFacts(ctx context.Context, scope identity.Scope, summaryID string, enc *crypto.Encryptor) ([]ExportedKeyFact, error) {
	type row struct {
		factCT                  []byte
		grounded, isInference   bool
		sourceCount             int
		expiresAt, expireReason sql.NullString
	}
	var rows []row
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		res, err := tx.QueryContext(ctx, `
			select fact, grounded, source_count, expires_at::text, expire_reason, is_inference
			from summary_key_facts
			where summary_id = $1
			order by id
		`, summaryID)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var r row
			if err := res.Scan(&r.factCT, &r.grounded, &r.sourceCount, &r.expiresAt, &r.expireReason, &r.isInference); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: export key facts for summary %s: %w", summaryID, err)
	}

	out := make([]ExportedKeyFact, 0, len(rows))
	for _, r := range rows {
		fact, err := enc.Decrypt(r.factCT)
		if err != nil {
			return nil, fmt.Errorf("store: decrypt key fact for summary %s: %w", summaryID, err)
		}
		out = append(out, ExportedKeyFact{
			Fact:         fact,
			Grounded:     r.grounded,
			SourceCount:  r.sourceCount,
			ExpiresAt:    r.expiresAt.String,
			ExpireReason: r.expireReason.String,
			IsInference:  r.isInference,
		})
	}
	return out, nil
}

func (s *Store) exportRelationships(ctx context.Context, scope identity.Scope) ([]ExportedRelationship, error) {
	var out []ExportedRelationship
	err := dbscope.Run(ctx, s.db, scope, scope, func(tx *sql.Tx) error {
		res, err := tx.QueryContext(ctx, `
			select id, subject_id, predicate, object_id,
			       coalesce(valid_from::text, ''), coalesce(valid_until::text, '')
			from entity_relationships
			where scope_kind = $1 and scope_owner = $2
			order by id
		`, scope.Kind, scope.Owner)
		if err != nil {
			return err
		}
		defer res.Close()
		for res.Next() {
			var r ExportedRelationship
			if err := res.Scan(&r.ID, &r.SubjectID, &r.Predicate, &r.ObjectID, &r.ValidFrom, &r.ValidUntil); err != nil {
				return err
			}
			out = append(out, r)
		}
		return res.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: export relationships for %s:%s: %w", scope.Kind, scope.Owner, err)
	}
	return out, nil
}
