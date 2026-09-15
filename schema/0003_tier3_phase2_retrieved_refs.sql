-- Tier 3 (Professional Shared) — Phase 2, schema half: replace the three
-- bare-id retrieval-trace arrays with one scope-qualified jsonb column.
-- See docs/TIER3_PLAN.md D2 and internal/identity.Ref.
--
-- Existing retrieved_summary_ids/retrieved_entity_ids/retrieved_episode_ids
-- values are not migrated: they were always assumed globally unique (true
-- for a single-scope Tier 1/2 deployment) and the application code
-- consuming them changes in the same commit as this migration, so there's
-- no window where old rows are read with the new code's assumptions.

alter table episodes
    drop column retrieved_summary_ids,
    drop column retrieved_entity_ids,
    drop column retrieved_episode_ids,
    add column retrieved_refs jsonb not null default '[]';
