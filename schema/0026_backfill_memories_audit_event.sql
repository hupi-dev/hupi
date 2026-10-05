-- New audit_log.event_type value for internal/backfillmemories
-- (cmd/hupi-backfill-memories) — Phase 0 of
-- docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md's backfill tool, same
-- reasoning schema/0009/0011/0013/0023 already used for export/import/
-- key_rotation/reembed/dashboard_login: a new kind of operation that
-- writes real data needs its own audited event type, not an overloaded
-- existing one.
--
-- Wrapped in an explicit transaction — see schema/0009's comment on why
-- (the drop and the add are otherwise two separately-autocommitted
-- statements under psql's defaults).
begin;
alter table audit_log drop constraint audit_log_event_type_check;
alter table audit_log add constraint audit_log_event_type_check
    check (event_type in (
        'capture', 'correct', 'retrieve', 'trace',
        'admin_provision', 'admin_ui_view',
        'export', 'import',
        'key_rotation',
        'reembed',
        'dashboard_login',
        'backfill_memories'
    ));
commit;
