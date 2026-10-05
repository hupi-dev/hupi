// Package entityattrs reconstructs an entity's current attribute set
// from the unified `memories` table (schema/0025, schema/0027) — the
// replacement for decrypting entities.attributes' flat JSON blob
// directly. Shared by internal/consolidation (which writes attribute
// rows via upsertEntities) and internal/store (which reads them at
// retrieval time), since both packages need the identical "which row
// is current for this entity's attribute" query — see
// docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md Phase 0.
//
// "Current" follows summaries.supersedes' own convention
// (schema/0001's summaries_current_idx comment): a row is current when
// no other row's `supersedes` column points at it, not when its own
// `supersedes` is null — every row, including the very first version of
// an attribute, has `supersedes` null, so that would describe every
// version rather than only the live one.
package entityattrs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"hupi/internal/crypto"
	"hupi/internal/dbscope"
	"hupi/internal/identity"
	"hupi/internal/pgfmt"
)

// Current returns entityID's live attribute values, decrypted, keyed by
// attribute name. Excludes the empty-attributes placeholder row
// internal/backfillmemories writes for an entity whose legacy
// attributes blob held no keys at all (attribute_key is null there,
// same as every other non-attribute memories row).
func Current(ctx context.Context, q dbscope.Querier, keys *crypto.KeyStore, scope identity.Scope, entityID string) (map[string]string, error) {
	m, err := CurrentForEntities(ctx, q, keys, scope, []string{entityID})
	if err != nil {
		return nil, err
	}
	return m[entityID], nil
}

// CurrentForEntities batches Current across multiple entities in one
// query — internal/store/retrieve.go's vectorSearchEntities and BM25
// entity-document builder each load many entities per call; batching
// avoids turning that into one query (and one key-resolve/decrypt) per
// entity. Entities with no current attributes (or not found at all)
// simply have no key in the returned map, same as a nil map lookup.
//
// grounded = true excludes a tombstoned key (upsertEntities' replace
// mode, or an explicit SupersedesKeys deletion) from the result
// entirely — such a row is still the current (non-superseded) row for
// that (entity, key) pair by CurrentRowIDs' own definition below, since
// something has to occupy that position so a later write can correctly
// supersede it again, but it represents "this key no longer applies,"
// not a real value callers should ever see.
func CurrentForEntities(ctx context.Context, q dbscope.Querier, keys *crypto.KeyStore, scope identity.Scope, entityIDs []string) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(entityIDs))
	if len(entityIDs) == 0 {
		return out, nil
	}
	rows, err := q.QueryContext(ctx, `
		select m.entity_id, m.attribute_key, m.content, m.key_version
		from memories m
		where m.scope_kind = $1 and m.scope_owner = $2
		  and m.entity_id = any($3::text[]) and m.is_static = true
		  and m.attribute_key is not null and m.grounded = true
		  and not exists (select 1 from memories newer where newer.supersedes = m.id)
	`, scope.Kind, scope.Owner, pgfmt.TextArray(entityIDs))
	if err != nil {
		return nil, fmt.Errorf("query current attribute rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entityID, key string
		var ct []byte
		var keyVersion int
		if err := rows.Scan(&entityID, &key, &ct, &keyVersion); err != nil {
			return nil, fmt.Errorf("scan attribute row: %w", err)
		}
		enc, err := keys.GetVersion(ctx, scope, keyVersion)
		if err != nil {
			return nil, fmt.Errorf("resolve encryption key for entity %s attribute %s: %w", entityID, key, err)
		}
		plain, err := enc.Decrypt(ct)
		if err != nil {
			return nil, fmt.Errorf("decrypt entity %s attribute %s: %w", entityID, key, err)
		}
		if out[entityID] == nil {
			out[entityID] = make(map[string]string)
		}
		out[entityID][key] = plain
	}
	return out, rows.Err()
}

// CurrentRowIDs returns entityID's live attribute rows' own ids, keyed
// by attribute name — not the decrypted values. The write path
// (internal/consolidation/store.go's upsertEntities) needs this, not
// Current: to overlay a changed value it must supersede the *row*
// currently holding the old one, which requires that row's id, not its
// plaintext (which the write path never needs to read, since every
// touched key gets a fresh row regardless of whether the value actually
// changed — see upsertEntities' own doc comment for why that's the
// simpler, intentional choice over a decrypt-and-compare check).
//
// Deliberately includes tombstoned (grounded = false) rows, unlike
// Current/CurrentForEntities above: a tombstone is still the live,
// current occupant of that (entity, key) slot, and a later write
// reintroducing the same key needs its id to correctly extend the same
// supersede chain rather than forking a second "current" row for one
// key.
func CurrentRowIDs(ctx context.Context, q dbscope.Querier, scope identity.Scope, entityID string) (map[string]string, error) {
	rows, err := q.QueryContext(ctx, `
		select m.id, m.attribute_key from memories m
		where m.scope_kind = $1 and m.scope_owner = $2 and m.entity_id = $3
		  and m.is_static = true and m.attribute_key is not null
		  and not exists (select 1 from memories newer where newer.supersedes = m.id)
	`, scope.Kind, scope.Owner, entityID)
	if err != nil {
		return nil, fmt.Errorf("query current attribute row ids: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return nil, fmt.Errorf("scan attribute row id: %w", err)
		}
		out[key] = id
	}
	return out, rows.Err()
}

// NewID is a random opaque memories.id for a live attribute row —
// mirrors internal/consolidation/store.go's own newRelationshipID
// (same reasoning: an attribute row, like a relationship edge, has no
// natural stable name to derive an id from the way an entity's
// kind+name does). Deliberately distinct in shape from
// internal/backfillmemories's deterministic mem_attr_<hash> ids: those
// exist specifically so a one-time backfill can't double-insert on
// retry, a concern that doesn't apply here since every call to
// upsertEntities is already wrapped in its own transaction with no
// retry-on-partial-failure path.
func NewID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate memory id: %w", err)
	}
	return "mem_" + hex.EncodeToString(buf), nil
}
