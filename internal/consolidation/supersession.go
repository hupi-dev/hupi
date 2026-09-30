package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"hupi/internal/dbscope"
	"hupi/internal/identity"
)

// knownEntityContext is one existing entity whose name appears
// somewhere in a day's own raw source text, given to the consolidation
// LLM alongside those sources — docs/CONSOLIDATION_COMPLETENESS_PLAN.md
// Phase C sub-problem 1. Mirrors internal/store/retrieve.go's
// stage1EntityMatches pattern (a cheap, all-entity substring scan
// against the scope's unencrypted id/name column), just applied at
// consolidation time instead of retrieval time, and in the opposite
// direction: does an existing entity's name appear in today's source
// text, not does the query mention a known entity.
type knownEntityContext struct {
	id         string
	name       string
	attributes map[string]string
}

// findKnownEntities returns every entity (in scope, excluding
// self_model — handled separately by buildAnchor, never something a
// consolidation run should be reconciling) whose name appears as a
// substring anywhere in combinedSourceText, with its current decrypted
// attributes — the context a consolidation run needs to recognize "this
// new attribute updates that existing one under a different name"
// (EntityUpdate.SupersedesKeys) instead of the two sitting side by side
// forever, the real Wells Fargo failure mode this fixes. Attributes are
// decrypted only for entities that actually match, not the whole scope,
// bounding real decrypt cost to genuine candidates. Read-only, meant to
// run inside the same short transaction RunDaily already opens for its
// other quick reads (loadDailyEpisodes, currentSummaryID) — this
// function makes no network calls itself, only the consolidation LLM
// call downstream does, kept outside any transaction per storeSummary's
// own established pattern.
func (r *Runner) findKnownEntities(ctx context.Context, q dbscope.Querier, scope identity.Scope, combinedSourceText string) ([]knownEntityContext, error) {
	rows, err := q.QueryContext(ctx, `
		select id, name, attributes, key_version from entities
		where kind != 'self_model' and scope_kind = $1 and scope_owner = $2
	`, scope.Kind, scope.Owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	lowerText := strings.ToLower(combinedSourceText)
	var out []knownEntityContext
	for rows.Next() {
		var id, name string
		var attrsCT []byte
		var keyVersion int
		if err := rows.Scan(&id, &name, &attrsCT, &keyVersion); err != nil {
			return nil, err
		}
		if name == "" || !strings.Contains(lowerText, strings.ToLower(name)) {
			continue
		}

		entry := knownEntityContext{id: id, name: name}
		if len(attrsCT) > 0 {
			enc, err := r.keys.GetVersion(ctx, scope, keyVersion)
			if err != nil {
				return nil, fmt.Errorf("resolve encryption key for entity %s: %w", id, err)
			}
			attrsJSON, err := enc.Decrypt(attrsCT)
			if err != nil {
				return nil, fmt.Errorf("decrypt attributes for entity %s: %w", id, err)
			}
			if attrsJSON != "" {
				if err := json.Unmarshal([]byte(attrsJSON), &entry.attributes); err != nil {
					return nil, fmt.Errorf("parse attributes for entity %s: %w", id, err)
				}
			}
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// formatKnownEntities renders knownEntityContext entries for
// buildSummaryPrompt — one line per entity with attributes, an entity
// with none omitted entirely (nothing to compare a new attribute
// against, so it adds nothing to the supersession judgment this exists
// for).
func formatKnownEntities(known []knownEntityContext) string {
	var sb strings.Builder
	wrote := false
	for _, k := range known {
		if len(k.attributes) == 0 {
			continue
		}
		attrsJSON, err := json.Marshal(k.attributes)
		if err != nil {
			continue
		}
		fmt.Fprintf(&sb, "- %s (id: %s): %s\n", k.name, k.id, attrsJSON)
		wrote = true
	}
	if !wrote {
		return ""
	}
	return sb.String()
}
