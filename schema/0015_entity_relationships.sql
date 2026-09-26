-- Entity relationships (docs/ENTITY_RELATIONSHIPS_PLAN.md): closes the
-- "limited temporal/relationship reasoning" gap identified by comparing
-- HUPI against Zep/Graphiti's graph-native design and Mem0's optional
-- graph layer. entities (schema/0001) has no way to connect two entities
-- to each other at all — this table is that connection, with bi-temporal
-- validity so a relationship's own history (not just its current state)
-- is queryable, the same "supersede, don't destroy" philosophy summaries'
-- own supersedes/correction_reason already use.
--
-- predicate is free text, not a constrained enum like entities.kind: the
-- entity-kind validation bug fixed this session (models inventing kinds
-- outside a 7-value enum) strongly suggests a predicate enum -- covering
-- a much larger, genuinely open-ended space of relationship types -- would
-- be a worse version of the same problem. Lightly sanitized in Go
-- (non-empty, length-capped, lowercased) before it ever reaches this
-- table, not constrained here.
--
-- subject_id/object_id/predicate stay unencrypted, matching
-- entities.kind/entities.name today (see that table's own attributes
-- column for the actual encryption boundary) -- they're structural,
-- needed for indexing and joining, not a new security-model decision.
--
-- subject_id/object_id are composite foreign keys against
-- (scope_kind, scope_owner, id), not just entities(id): entities.id is
-- only unique *within* a scope (its real primary key is
-- (scope_kind, scope_owner, id) -- schema/0002_tier3_phase1_identity.sql,
-- since two different teams can each have their own "project:hupi").
-- A bare `references entities(id)` wouldn't just fail to find a unique
-- constraint to reference (which is the error that caught this) -- if
-- Postgres had somehow allowed it, it would have let a relationship
-- silently reference an entity row in a *different* scope entirely, a
-- real cross-scope data leak, not just a schema mistake.
create table entity_relationships (
    id                 text primary key,
    scope_kind         text not null,
    scope_owner        text not null,
    subject_id         text not null,
    predicate          text not null,
    object_id          text not null,
    valid_from         date,               -- null = unknown when it started
    valid_until        date,               -- null = still current
    source_summary_id  text references summaries(id),
    created_at         timestamptz not null default now(),
    foreign key (scope_kind, scope_owner, subject_id) references entities (scope_kind, scope_owner, id),
    foreign key (scope_kind, scope_owner, object_id) references entities (scope_kind, scope_owner, id)
);

create index entity_relationships_subject_idx on entity_relationships (scope_kind, scope_owner, subject_id);
create index entity_relationships_object_idx  on entity_relationships (scope_kind, scope_owner, object_id);

grant select, insert, update, delete on entity_relationships to hupi_app;

-- Same RLS pattern every other scope-owned table already uses
-- (schema/0005) -- two scope-variable pairs for the same reason
-- (Store.buildAnchor reads both the acting user's and the workspace's
-- scope in one transaction), enforced only for non-owner roles.
alter table entity_relationships enable row level security;

create policy scope_isolation on entity_relationships
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
