-- Backing store for the public hosted demo (docs/TODO.md #2): an
-- anonymous, short-lived guest session, capped on message count and
-- consolidation calls, cleaned up by cmd/hupi-demo-sweep once expired.
-- guest_user_id is a normal private-scope `users` row (internal/demo
-- provisions it via auth.Store.CreateUser, same DEK provisioning path as
-- any other user) — a demo guest is Tier 1/2, not a team, on purpose.
create table demo_sessions (
    token_hash        text primary key,
    guest_user_id     text not null references users(id) on delete cascade,
    created_at        timestamptz not null default now(),
    expires_at        timestamptz not null,
    message_count     int not null default 0,
    consolidate_count int not null default 0
);

-- cmd/hupi-demo-sweep's own query shape (expired or past the hard
-- backstop age) — this is the one query pattern that actually runs
-- against this table on a schedule.
create index demo_sessions_expires_at_idx on demo_sessions (expires_at);

grant select, insert, update, delete on demo_sessions to hupi_app;
