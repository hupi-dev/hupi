-- Supports hupi-dashboard's Tier-3 sign-in (cmd/hupi-dashboard,
-- hupi-t3's cmd/hupi-dashboard/team.go overlay) — Tier 1/2 never
-- populates any of this, same as users/teams/api_keys in
-- schema/0002_tier3_phase1_identity.sql are also Tier-3-only in practice
-- but defined here, since Postgres is one unified backend across tiers
-- (ARCHITECTURE.md's Tiers section).
--
-- password_hash is nullable and set only via a new `hupi-admin
-- set-password` subcommand — admin-driven, no self-service reset,
-- matching create-user/create-key/create-operator's existing
-- admin-only provisioning model. bcrypt, same as any standard Go
-- password-hashing library produces; verified by hupi-t3's login route,
-- never by the public repo.
alter table users add column password_hash text;

-- Opaque session token for hupi-dashboard's browser login — only its
-- sha256 hash is stored, same pattern operators.token_hash
-- (schema/0008) and api_keys.key_hash (schema/0002) already use. No
-- cookies: the frontend holds the raw token in sessionStorage and sends
-- Authorization: Bearer <token>, which never gets auto-resent cross-site
-- the way a cookie or Basic Auth credential would — see
-- cmd/hupi-dashboard/auth.go's own doc comment for why that sidesteps
-- CSRF entirely instead of needing cmd/hupi-admin-ui's
-- Sec-Fetch-Site-based mitigation.
create table dashboard_sessions (
    token_hash   text primary key,
    user_id      text not null references users(id),
    scope_kind   text not null,
    scope_owner  text not null,
    created_at   timestamptz not null default now(),
    expires_at   timestamptz not null
);

create index dashboard_sessions_expires_idx on dashboard_sessions (expires_at);

grant select, insert, delete on dashboard_sessions to hupi_app;

-- "Last used" doesn't exist on api_keys today — needed for
-- hupi-dashboard's admin "stale API key" panel (an admin-only, Tier-3
-- security-posture view). Nullable: never set until the first
-- successful resolve after this migration, same "unknown, not zero"
-- posture schema/0013's embedding_model backfill already uses for an
-- added column on existing rows.
alter table api_keys add column last_used_at timestamptz;

-- dashboard_login: every hupi-dashboard sign-in attempt (password or
-- OIDC, success or failure) — see internal/audit.EventDashboardLogin's
-- own doc comment for why this is its own type rather than folded into
-- admin_provision, same reasoning schema/0009/0011/0013 already used for
-- export/import/key_rotation/reembed.
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
        'dashboard_login'
    ));
commit;
