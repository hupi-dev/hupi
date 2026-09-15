-- Hardening Phase 1 (docs/HARDENING_PLAN.md D4): a non-owner, non-superuser
-- role for the running application to connect as. Postgres RLS (added in
-- Phase 3) is bypassed by the table owner and by superusers unless a table
-- is marked FORCE ROW LEVEL SECURITY — without this role, RLS would
-- silently enforce nothing for the app's own queries.
--
-- Deliberately does NOT set a password here: run
--   ALTER ROLE hupi_app WITH PASSWORD '...';
-- separately, out of band, so no secret ever lands in a file meant to be
-- checked into version control.

do $$
begin
    if not exists (select from pg_roles where rolname = 'hupi_app') then
        create role hupi_app with login;
    end if;
end
$$;

grant usage on schema public to hupi_app;

-- The scoped memory tables (RLS target in Phase 3) plus their supporting
-- tables and the identity tables (not RLS-scoped — users/teams/etc. have
-- their own natural keys, not scope_kind/scope_owner).
grant select, insert, update, delete on
    episodes, summaries, summary_key_facts, entities, deployment_meta,
    users, teams, team_members, api_keys
to hupi_app;

grant usage, select on all sequences in schema public to hupi_app;
