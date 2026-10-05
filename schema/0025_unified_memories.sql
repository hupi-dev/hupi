-- Phase 0 of docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md: the unified
-- memory model. Purely additive — this migration creates two new
-- tables and touches nothing existing. Nothing reads or writes these
-- tables yet; application code is a separate, later change, sequenced
-- deliberately behind this schema landing and baking on its own first
-- (see the plan doc's own rollback-window open question).
--
-- `memories` replaces the role both `entities.attributes` (schema/0001,
-- a flat encrypted-JSON-blob overlay with no history) and
-- `summary_key_facts` (schema/0001, already append-only and versioned
-- via Go-level supersession logic) play today, with `is_static`
-- distinguishing the two instead of two differently-shaped tables. See
-- the plan doc's "Current state" section for the real, confirmed bug
-- (Finding 1, docs/CONSOLIDATION_ARCHITECTURE_REVIEW_PLAN.md) this is
-- meant to close a whole class of.
--
-- entity_id is nullable and composite, same FK shape
-- entity_relationships (schema/0015) already uses for the identical
-- reason: entities.id is only unique *within* a scope
-- (schema/0002_tier3_phase1_identity.sql dropped its global PK for
-- exactly this), so a bare `references entities(id)` would silently
-- allow a cross-scope reference instead of failing to find a
-- constraint to reference at all. Nullable because a pure event fact
-- (what summary_key_facts rows are today) doesn't always have one
-- single owning entity — a fact can be grounded in a summary without
-- being "about" any one entity in particular.
--
-- summary_id is a plain (non-composite) FK, same as
-- summary_key_facts.summary_id today — summaries.id stays globally
-- unique by construction (see schema/0002's own comment on why it
-- deliberately wasn't scope-qualified the way entities.id was), so no
-- composite key is needed here either.
--
-- Not yet wired into internal/rotate/rotate.go's tableOrder
-- (currently ["episodes", "summaries", "entities"], with
-- summary_key_facts handled implicitly alongside summaries) —
-- deliberately deferred, since this table is empty until the backfill
-- tool and application code (later Phase 0 PRs) actually write to it.
-- Must be added before memories carries real data, or a key rotation
-- will silently skip this table entirely while still reporting success
-- for the other four.
--
-- embedding/embedding_model mirror schema/0021's addition to
-- summary_key_facts verbatim, including deliberately omitting an ANN
-- index for the same reason that migration gives: ranking only ever
-- happens within a summary's own already-selected fact set, never a
-- cross-table nearest-neighbor search.
create table memories (
    id                  text primary key,
    scope_kind          text not null check (scope_kind in ('private', 'shared')),
    scope_owner         text not null,

    entity_id           text,
    summary_id          text references summaries(id),

    content             bytea not null,                 -- encrypted
    key_version         int not null default 1,         -- same role as
                                                          -- episodes/
                                                          -- summaries/
                                                          -- summary_key_facts/
                                                          -- entities'
                                                          -- own key_version
                                                          -- (schema/0010) —
                                                          -- which scope DEK
                                                          -- version `content`
                                                          -- is encrypted
                                                          -- under, so
                                                          -- internal/rotate
                                                          -- can migrate this
                                                          -- table the same
                                                          -- way it already
                                                          -- migrates the
                                                          -- other four

    is_static           boolean not null default false, -- false = event
                                                          -- fact (today's
                                                          -- summary_key_facts
                                                          -- row); true =
                                                          -- stable attribute
                                                          -- (today's
                                                          -- entities.attributes
                                                          -- key)
    is_inference        boolean not null default false,
    source_count         int not null default 1,
    expires_at           date,
    expire_reason        text,

    grounded            boolean not null default true,
    source_episode_ids  text[] not null default '{}',

    embedding           vector(1536),
    embedding_model     text,

    created_at          timestamptz not null default now(),
    updated_at          timestamptz not null default now(),

    foreign key (scope_kind, scope_owner, entity_id) references entities (scope_kind, scope_owner, id)
);

create index memories_scope_idx on memories (scope_kind, scope_owner);
create index memories_entity_idx on memories (scope_kind, scope_owner, entity_id) where entity_id is not null;
create index memories_summary_idx on memories (summary_id);
create index memories_expires_idx on memories (expires_at) where expires_at is not null;

grant select, insert, update, delete on memories to hupi_app;

-- Same scope_isolation policy shape as every other scope-owned table
-- (schema/0005/0015/0018's own identical comment on the USING/WITH
-- CHECK split applies verbatim here).
alter table memories enable row level security;

create policy scope_isolation on memories
    for all
    using (
        (scope_kind = current_setting('hupi.acting_scope_kind', true)
         and scope_owner = current_setting('hupi.acting_scope_owner', true))
        or
        (scope_kind = current_setting('hupi.workspace_scope_kind', true)
         and scope_owner = current_setting('hupi.workspace_scope_owner', true))
    )
    with check (
        scope_kind = current_setting('hupi.workspace_scope_kind', true)
        and scope_owner = current_setting('hupi.workspace_scope_owner', true)
    );

-- memory_relations: fact-to-fact relations (updates/extends/derives),
-- Phase 3 of the plan doc. Created now, alongside memories, so the
-- unified schema lands in one migration rather than two — nothing
-- populates this table until Phase 3's application code ships.
--
-- Deliberately allows more than one relation between the same ordered
-- pair of memories (no unique constraint on from/to/type): plan doc
-- open question 2 (does a fact need to both extend one fact and derive
-- from another) is answered at the *from* side needing multiple rows
-- to different *to* targets, which this already supports without
-- needing a decision yet on whether the *same pair* can hold more than
-- one relation_type simultaneously — deferred, not foreclosed.
create table memory_relations (
    id                  text primary key,
    scope_kind          text not null check (scope_kind in ('private', 'shared')),
    scope_owner         text not null,

    from_memory_id      text not null references memories(id) on delete cascade,
    to_memory_id        text not null references memories(id) on delete cascade,
    relation_type       text not null check (relation_type in ('updates', 'extends', 'derives')),

    created_at          timestamptz not null default now()
);

create index memory_relations_scope_idx on memory_relations (scope_kind, scope_owner);
create index memory_relations_from_idx on memory_relations (from_memory_id);
create index memory_relations_to_idx on memory_relations (to_memory_id);

grant select, insert, update, delete on memory_relations to hupi_app;

alter table memory_relations enable row level security;

create policy scope_isolation on memory_relations
    for all
    using (
        (scope_kind = current_setting('hupi.acting_scope_kind', true)
         and scope_owner = current_setting('hupi.acting_scope_owner', true))
        or
        (scope_kind = current_setting('hupi.workspace_scope_kind', true)
         and scope_owner = current_setting('hupi.workspace_scope_owner', true))
    )
    with check (
        scope_kind = current_setting('hupi.workspace_scope_kind', true)
        and scope_owner = current_setting('hupi.workspace_scope_owner', true)
    );
