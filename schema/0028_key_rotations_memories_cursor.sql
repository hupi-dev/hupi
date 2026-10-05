-- internal/rotate/rotate.go's tableOrder just grew a fourth stop
-- ("memories", schema/0025) — key_rotations.cursor_table's check
-- constraint (schema/0010) only ever allowed the original three plus
-- 'done', so Continue's own setCursorTable call would fail the moment a
-- rotation actually walked off the end of "entities" and tried to set
-- cursor_table = 'memories'. Same drop-and-recreate-under-begin/commit
-- pattern as schema/0026's audit_log_event_type_check extension.
begin;

alter table key_rotations drop constraint key_rotations_cursor_table_check;
alter table key_rotations add constraint key_rotations_cursor_table_check
    check (cursor_table in ('episodes', 'summaries', 'entities', 'memories', 'done'));

commit;
