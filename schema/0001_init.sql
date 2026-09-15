-- HUPI Tier 1/2 schema. One deployment, one owner, no `scope` column yet —
-- that's added deliberately in a later migration when Tier 3 is built
-- (see ARCHITECTURE.md § Suggested build order, step 9), not baked in now.
--
-- Sensitive text fields are `bytea`, not `text`: the application encrypts
-- them (AES-256-GCM, nonce prepended to the ciphertext) before INSERT and
-- decrypts after SELECT — see ARCHITECTURE.md § Storage security. Nothing
-- in this schema can enforce that; it's a contract the application layer
-- must honor for every read/write path, including ad-hoc migrations.
--
-- Embedding dimension is fixed per column at 1536 (matches
-- text-embedding-3-small / many common models). Switching to an embedding
-- model with a different dimension requires a migration that drops and
-- rebuilds the vector column and its index — `pgvector` columns are
-- fixed-width, this isn't a config toggle.

create extension if not exists vector;

-- ============================================================
-- episodes: the raw interaction log. Append-only in practice —
-- nothing in this schema deletes or updates input_text/output_text
-- once written; corrections happen via `supersedes` on new rows,
-- never UPDATE on old ones.
-- ============================================================
create table episodes (
    id                  text primary key,               -- ULID, sortable
    ts                  timestamptz not null default now(),
    type                text not null
                        check (type in ('interaction', 'note', 'event', 'feedback')),

    -- provider that produced this episode (null for type='feedback')
    provider_vendor     text,
    provider_model      text,
    provider_endpoint   text,

    context_tags        text[] not null default '{}',

    -- encrypted application-level (see file header)
    input_text          bytea,
    output_text         bytea,

    importance          real check (importance between 0 and 1),
    hash                text not null,                  -- sha256 hex, dedup key
    supersedes          text references episodes(id),
    externalized        boolean not null default false,
    truncated           boolean not null default false,

    -- retrieval provenance, written at capture time — see
    -- ARCHITECTURE.md § Retrieval observability & evaluation
    memory_gate         text check (memory_gate in ('skipped', 'partial', 'full')),
    retrieved_summary_ids text[] not null default '{}',
    retrieved_entity_ids  text[] not null default '{}',
    retrieved_episode_ids text[] not null default '{}', -- other high-importance episodes matched by vector search, see episodes.embedding below

    -- only set when type = 'feedback'
    refers_to           text references episodes(id),
    rating              text check (rating in ('memory_correct', 'memory_wrong', 'memory_missing')),
    note                bytea,

    -- populated only for high-importance episodes the consolidation
    -- engine decided were worth indexing directly (most retrieval hits
    -- summaries instead; see ARCHITECTURE.md § Retrieval Engine)
    embedding           vector(1536)
);

create index episodes_ts_idx on episodes (ts);
create index episodes_type_idx on episodes (type);
create index episodes_hash_idx on episodes (hash);
create index episodes_refers_to_idx on episodes (refers_to) where refers_to is not null;
create index episodes_embedding_idx on episodes using hnsw (embedding vector_cosine_ops)
    where embedding is not null;

-- ============================================================
-- summaries: hierarchical rollups. Corrections are new rows with
-- `supersedes` set, never in-place edits — see MEMORY_FORMAT.md
-- § Grounding & correction.
-- ============================================================
create table summaries (
    id                      text primary key,           -- e.g. sum_2026-09-09_daily_v1
    period                  text not null,               -- ISO date / week / month / year
    level                   text not null
                            check (level in ('daily', 'weekly', 'monthly', 'yearly')),
    status                  text not null default 'draft'
                            check (status in ('draft', 'reviewed')),

    generated_by_vendor     text,
    generated_by_model      text,

    source_episode_ids      text[] not null default '{}',
    source_summary_periods  text[] not null default '{}',

    summary                 bytea,                       -- encrypted, prose only, not retrieved as fact
    entities_touched        text[] not null default '{}',

    grounding_checked       boolean not null default false,
    supersedes              text references summaries(id),
    correction_reason       text,

    created_at              timestamptz not null default now(),
    embedding                vector(1536)
);

create index summaries_period_level_idx on summaries (period, level);
create index summaries_current_idx on summaries (period, level) where supersedes is null;
create index summaries_embedding_idx on summaries using hnsw (embedding vector_cosine_ops)
    where embedding is not null;

-- key_facts is a child table, not a jsonb column: each fact carries its
-- own citation and grounding result independently (see MEMORY_FORMAT.md
-- § Grounding & correction) and this makes "give me every ungrounded
-- fact ever produced" a plain query instead of a jsonb scan.
create table summary_key_facts (
    id                  bigserial primary key,
    summary_id          text not null references summaries(id) on delete cascade,
    fact                bytea not null,                 -- encrypted
    source_episode_ids  text[] not null default '{}',
    grounded            boolean not null
);

create index summary_key_facts_summary_id_idx on summary_key_facts (summary_id);
create index summary_key_facts_grounded_idx on summary_key_facts (grounded);

-- ============================================================
-- entities: the long-lived knowledge graph (people, projects,
-- preferences, skills, self_model). Not time-sliced, updated in place
-- (unlike episodes/summaries) since an entity is "current state", not
-- a historical record — see MEMORY_FORMAT.md § Entity record.
--
-- NOTE / open caveat: `id` is a human-readable slug (e.g.
-- "project:hupi", "person:jane-doe") by design, so the relevance gate
-- can do cheap exact-key matching on it (see ARCHITECTURE.md's
-- "Stage 1 pre-check"). That means IDs of kind='person' can themselves
-- encode a name and are NOT covered by the `attributes` field
-- encryption below. Acceptable for now; revisit if person-entity IDs
-- need to be opaque tokens with the display name moved into
-- `attributes` instead.
-- ============================================================
create table entities (
    id              text primary key,
    kind            text not null
                    check (kind in ('person', 'project', 'preference', 'skill', 'place', 'organization', 'self_model')),
    name            text not null,
    first_seen      date not null default current_date,
    last_updated    date not null default current_date,
    attributes      bytea                                -- encrypted, json-serialized before encryption
);

create index entities_kind_idx on entities (kind);

-- ============================================================
-- deployment_meta: singleton row, the Postgres equivalent of
-- manifest.json's live-relevant fields (export-only fields like
-- export_encryption live in the export artifact, not here).
-- ============================================================
create table deployment_meta (
    id                              boolean primary key default true check (id),
    hpmf_version                    text not null default '1.0',
    owner_id                        text not null,
    created_at                      timestamptz not null default now(),
    last_consolidated_at            timestamptz,
    consolidation_provider_history  jsonb not null default '[]'
);
