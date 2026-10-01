-- Gap Closure Phase 3 (docs/GAP_CLOSURE_PLAN.md §4.2): hupi-export and
-- hupi-import are exactly the kind of data-movement operation an audit
-- trail exists to catch — a scope's entire history leaving the live
-- store is at least as significant as a single retrieval. Extends
-- audit_log's event_type check rather than reusing 'admin_provision':
-- export/import aren't provisioning actions, and a distinct type lets
-- `hupi-audit query -event-type export` answer "who's taken data out of
-- this deployment" directly.
--
-- Wrapped in an explicit transaction (docs/CODEBASE_SURVEY_AND_REVIEW.md
-- C-section): the drop and the add are two separate statements, and
-- psql's default autocommit mode runs each one as its own transaction —
-- an interruption between them (a dropped connection, a killed process)
-- would leave audit_log with no event_type constraint at all, silently
-- accepting any string. begin/commit makes the whole widen-the-check
-- step atomic: either the old constraint is still there, or the new one
-- is, never neither.
begin;
alter table audit_log drop constraint audit_log_event_type_check;
alter table audit_log add constraint audit_log_event_type_check
    check (event_type in (
        'capture', 'correct', 'retrieve', 'trace',
        'admin_provision', 'admin_ui_view',
        'export', 'import'
    ));
commit;
