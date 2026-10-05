-- Adds what schema/0025's `memories` table was missing for the
-- application cutover (internal/consolidation/store.go's
-- upsertEntities, internal/store/retrieve.go's entities.attributes
-- readers) to actually write/read static attribute rows through it —
-- see docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md Phase 0.
--
-- attribute_key: which entity attribute (e.g. "city") a static
-- (is_static = true) row holds the value of. Not in 0025 at all —
-- internal/backfillmemories only needed it to *derive* a deterministic
-- row id (memoryIDForAttribute), never to query by it afterward. The
-- application write path does need to query by it: "what's the current
-- row for entity E's city attribute, so a new value can supersede it"
-- has no answer without a real, queryable column. Null for event facts
-- (is_static = false) and for the empty-attributes placeholder row
-- backfillmemories writes for an entity whose legacy attributes blob
-- held no keys at all — neither represents one single named attribute.
--
-- supersedes: same role as summaries.supersedes (schema/0001) and the
-- same "current = nothing points at me" read convention
-- (summaries_current_idx's own comment) — this is what gives attributes
-- real history for the first time, replacing the old flat per-key
-- overlay (mergeAttributes) that silently discarded an attribute's
-- prior value on every update.
begin;

alter table memories add column attribute_key text;
alter table memories add column supersedes text references memories(id);

-- Mirrors summaries_current_idx (schema/0001) verbatim, one level down:
-- there, the partial index is on (period, level) where supersedes is
-- null, which that table's own comment already flags as answering the
-- wrong question (current = not superseded by a newer row, not "this
-- row's own supersedes is null") — not repeating that mistake here by
-- indexing the lookup columns themselves rather than pretending
-- "supersedes is null" is a filter condition for current-ness.
create index memories_entity_attr_idx on memories (entity_id, attribute_key) where is_static = true;

commit;
