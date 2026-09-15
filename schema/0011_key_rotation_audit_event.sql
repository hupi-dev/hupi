-- Gap Closure Phase 4 (docs/GAP_CLOSURE_PLAN.md §4.4): a dedicated
-- audit_log event type for key rotation, same reasoning as export/import
-- getting their own in schema/0009 rather than being folded into
-- 'admin_provision' — "show me every rotation ever run on this scope" is
-- a real question, and a distinct type answers it directly via
-- `hupi-audit query -event-type key_rotation`.
alter table audit_log drop constraint audit_log_event_type_check;
alter table audit_log add constraint audit_log_event_type_check
    check (event_type in (
        'capture', 'correct', 'retrieve', 'trace',
        'admin_provision', 'admin_ui_view',
        'export', 'import',
        'key_rotation'
    ));
