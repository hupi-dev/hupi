-- Supports per-scope BM25 keyword-search governance
-- (internal/store/retrieve.go's keywordSearchTierForScope): BM25 has no
-- database index — every stored field is encrypted at rest, so it
-- decrypts and live-scores the entire in-scope corpus on every keyword
-- search, a real, corpus-size-linear cost unlike vector search's own
-- HNSW-indexed lookups. Rather than only the existing blunt global
-- HUPI_ENABLE_KEYWORD_SEARCH on/off switch, retrieval now narrows (or, for
-- a genuinely oversized scope, disables) keyword search based on that
-- scope's own real corpus size, read from this table.
--
-- Raw counts, not a precomputed tier: changing the threshold env vars
-- takes effect on the very next request, without waiting for the next
-- nightly consolidation run to re-derive anything.
--
-- Updated once per day, per active scope, by
-- internal/consolidation.Runner.RunDaily (only on a day that had real
-- episodes — a day with nothing new doesn't change a scope's corpus
-- size, so there's nothing to recompute) — not written by the live
-- request path at all, so a retrieval call only ever does one cheap
-- point-lookup against an already-current row.
create table scope_corpus_size (
    scope_kind     text not null,
    scope_owner    text not null,
    episode_count  integer not null default 0,
    summary_count  integer not null default 0,
    updated_at     timestamptz not null default now(),
    primary key (scope_kind, scope_owner)
);

grant select, insert, update, delete on scope_corpus_size to hupi_app;

-- Same RLS shape as every other scoped table (schema/0018_summary_key_facts_rls.sql's
-- exact USING/WITH CHECK pattern) — a retrieval-tuning signal, not memory
-- content, but no reason to make it the one scoped table without this
-- second, independent isolation layer.
alter table scope_corpus_size enable row level security;

create policy scope_isolation on scope_corpus_size
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
