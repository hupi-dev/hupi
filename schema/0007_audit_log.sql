-- Gap Closure Phase 2 (docs/GAP_CLOSURE_PLAN.md §4.3): one table, every
-- event type — captures, retrievals, investigation (hupi-trace), and
-- admin actions all land here, rather than a report tool having to UNION
-- episodes-as-writes with separate retrieval/admin logs. Single source of
-- truth for "what happened, in what order."
create table audit_log (
    id                      bigserial primary key,
    ts                      timestamptz not null default now(),
    event_type              text not null
                            check (event_type in (
                                'capture', 'correct', 'retrieve', 'trace',
                                'admin_provision', 'admin_ui_view'
                            )),
    -- Identity.UserID for an authenticated request; an operator name
    -- (hupi-admin/hupi-trace's -actor flag, or an operators.name for the
    -- admin UI) for anything with no API-key identity — see
    -- schema/0008_admin_operators.sql.
    actor                   text not null,

    acting_scope_kind       text not null,
    acting_scope_owner      text not null,
    workspace_scope_kind    text not null,
    workspace_scope_owner   text not null,

    -- identity.Ref of whatever was touched, when there is one single
    -- target (a specific summary/entity/episode) — null for events like
    -- admin_provision that don't target a memory record.
    target_ref              jsonb,
    detail                  jsonb not null default '{}'
);

create index audit_log_ts_idx on audit_log (ts);
create index audit_log_scope_idx on audit_log (workspace_scope_kind, workspace_scope_owner, ts);
create index audit_log_actor_idx on audit_log (actor, ts);
create index audit_log_event_type_idx on audit_log (event_type, ts);

-- RLS, deliberately asymmetric — see docs/GAP_CLOSURE_PLAN.md §4.3:
--
-- INSERT is scope-checked against the writing session's own session
-- variables (same pattern as schema/0005), catching an application bug
-- that logs a row under the wrong scope — the same "second independent
-- safety net" role RLS plays everywhere else in this project.
--
-- SELECT has NO scope-restricting policy — audit_log's entire purpose is
-- letting an operator (hupi-audit) see across every scope, not just one.
-- This doesn't weaken an existing guarantee: anyone holding the hupi_app
-- credential can already choose which scope's session variables to set
-- (the same trust model hupi-trace already relies on to investigate an
-- arbitrary scope), so unrestricted audit_log reads are consistent with
-- that, not a new hole.
alter table audit_log enable row level security;

create policy audit_insert_scope_check on audit_log
    for insert
    with check (
        acting_scope_kind = current_setting('hupi.acting_scope_kind', true)
        and acting_scope_owner = current_setting('hupi.acting_scope_owner', true)
        and workspace_scope_kind = current_setting('hupi.workspace_scope_kind', true)
        and workspace_scope_owner = current_setting('hupi.workspace_scope_owner', true)
    );

create policy audit_select_unrestricted on audit_log
    for select
    using (true);

-- Deliberately no update/delete grant, unlike every other table hupi_app
-- owns (schema/0004 grants full CRUD on episodes/summaries/users/etc.) —
-- an audit trail the same role generating events can also erase isn't
-- one. Rotating out old entries, if ever needed, is a superuser/owner
-- operation (like a migration), not something the running application
-- does to its own history.
grant select, insert on audit_log to hupi_app;
grant usage, select on sequence audit_log_id_seq to hupi_app;
