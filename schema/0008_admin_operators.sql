-- Gap Closure Phase 2 (docs/GAP_CLOSURE_PLAN.md §4.3): replaces
-- cmd/hupi-admin-ui's single shared HUPI_ADMIN_UI_TOKEN with named
-- operator credentials. The audit level chosen for this project explicitly
-- includes admin access ("who viewed this") — that's meaningless if every
-- operator authenticates with the same indistinguishable token. Same
-- shape as api_keys (schema/0002), one hash per credential, raw value
-- never persisted.
create table operators (
    name       text primary key,        -- e.g. "alice" — what gets recorded as audit_log.actor
    token_hash text not null unique,
    created_at timestamptz not null default now(),
    revoked_at timestamptz
);

grant select, insert, update, delete on operators to hupi_app;
