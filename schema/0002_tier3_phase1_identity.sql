-- Tier 3 (Professional Shared) — Phase 1: identity + scope foundation.
-- See docs/TIER3_PLAN.md. No behavior change for existing Tier 1/2
-- deployments: every row that existed before this migration becomes
-- scope_kind='private', scope_owner='user:default' automatically via the
-- column defaults below (Postgres backfills existing rows at ADD COLUMN
-- time when a default is given — no separate UPDATE needed for that
-- part). Application-level scope *filtering* is Phase 2, not this file.

-- ============================================================
-- Identity: users, teams, membership, API keys.
--
-- scope_owner (added to episodes/summaries/entities below) is
-- deliberately NOT a foreign key to either users or teams — it's
-- polymorphic (a user id when scope_kind='private', a team id when
-- scope_kind='shared'), and Postgres has no native polymorphic FK.
-- Referential integrity across that boundary is an application-level
-- responsibility going forward (Phase 2's scope-filtering queries, plus
-- the row-level-security policies planned for later phases).
-- ============================================================
create table users (
    id         text primary key,        -- e.g. "user:sujith"
    email      text,
    created_at timestamptz not null default now()
);

create table teams (
    id         text primary key,        -- e.g. "team:acme-eng"
    name       text not null,
    created_at timestamptz not null default now()
);

create table team_members (
    team_id text not null references teams(id) on delete cascade,
    user_id text not null references users(id) on delete cascade,
    role    text not null default 'member' check (role in ('member', 'admin')),
    primary key (team_id, user_id)
);

create table api_keys (
    key_hash   text primary key,        -- sha256 hex of the raw key; the raw key itself is never stored
    user_id    text not null references users(id) on delete cascade,
    created_at timestamptz not null default now(),
    revoked_at timestamptz
);

-- The default owner of all pre-Tier-3 data (see backfill below) — every
-- single-user Tier 1/2 deployment maps to exactly this one user until
-- someone else is explicitly added.
insert into users (id, email) values ('user:default', null)
    on conflict (id) do nothing;

-- ============================================================
-- Scope columns on the three memory tables.
-- ============================================================
alter table episodes
    add column scope_kind  text not null default 'private',
    add column scope_owner text not null default 'user:default';
alter table episodes
    add constraint episodes_scope_kind_check check (scope_kind in ('private', 'shared'));

alter table summaries
    add column scope_kind  text not null default 'private',
    add column scope_owner text not null default 'user:default';
alter table summaries
    add constraint summaries_scope_kind_check check (scope_kind in ('private', 'shared'));

alter table entities
    add column scope_kind  text not null default 'private',
    add column scope_owner text not null default 'user:default';
alter table entities
    add constraint entities_scope_kind_check check (scope_kind in ('private', 'shared'));

create index episodes_scope_idx  on episodes  (scope_kind, scope_owner);
create index summaries_scope_idx on summaries (scope_kind, scope_owner);
create index entities_scope_idx  on entities  (scope_kind, scope_owner);

-- ============================================================
-- entities' primary key becomes scope-qualified: "project:hupi" is only
-- unique *within* a scope once more than one tenant exists. Verified
-- against schema/0001_init.sql that nothing holds a foreign key into
-- entities.id, so this is a safe, isolated change.
--
-- summaries.id and episodes.id deliberately do NOT get the same
-- treatment: summary_key_facts.summary_id and summaries.supersedes (and
-- episodes.refers_to/supersedes) all hold real foreign keys into those
-- ids, which requires them to stay globally unique on their own. Instead,
-- summary id *generation* becomes scope-namespaced in Phase 5
-- (internal/consolidation's nextSummaryID), and episode ids are already
-- random/globally unique by construction (ULID-shaped), so neither needs
-- a key restructure — see docs/TIER3_PLAN.md §4.
-- ============================================================
alter table entities drop constraint entities_pkey;
alter table entities add primary key (scope_kind, scope_owner, id);

-- ============================================================
-- Rename the Tier 1/2 singleton self_model to be user-owned, per
-- docs/TIER3_PLAN.md D3 (self_model stays personal, even inside a team
-- workspace). Safe no-op if no self_model row exists yet.
-- ============================================================
update entities set id = 'self_model:user:default'
    where kind = 'self_model' and id = 'self_model:primary';
