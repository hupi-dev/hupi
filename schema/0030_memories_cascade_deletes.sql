-- memories.entity_id/summary_id (schema/0025) were created without ON
-- DELETE CASCADE — fine for production, which never hard-deletes an
-- entity or summary row (entities are updated in place; summaries are
-- superseded, never deleted), but it makes every test's cleanup code a
-- silent trap: internal/consolidation/store.go's storeSummary now
-- mirrors every key fact into memories live (not just a later backfill
-- job), so a test cleanup block that deletes summaries/entities before
-- deleting memories hits a real FK violation that the
-- `_, _ = tx.Exec(...)` convention swallows silently, aborting the
-- whole cleanup transaction and leaking rows into the next test run —
-- the identical failure shape the entities FK already caused once
-- (see the attribute-cutover PR's own fix). Rather than auditing every
-- test file's delete ordering by hand, and trusting every future test
-- author to learn this the same way, cascading at the schema level
-- closes the whole class: a memories row has no independent meaning
-- without its parent entity/summary anyway, so cascading its deletion
-- alongside theirs is also the semantically correct behavior, not just
-- a test-convenience hack.
begin;

alter table memories drop constraint memories_scope_kind_scope_owner_entity_id_fkey;
alter table memories add constraint memories_scope_kind_scope_owner_entity_id_fkey
    foreign key (scope_kind, scope_owner, entity_id) references entities (scope_kind, scope_owner, id) on delete cascade;

alter table memories drop constraint memories_summary_id_fkey;
alter table memories add constraint memories_summary_id_fkey
    foreign key (summary_id) references summaries(id) on delete cascade;

commit;
