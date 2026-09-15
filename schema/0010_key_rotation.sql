-- Gap Closure Phase 4 (docs/GAP_CLOSURE_PLAN.md §4.4): online, resumable
-- per-scope key rotation. See internal/crypto/keystore.go's doc comment
-- for why this requires every encrypted row to self-describe which DEK
-- version encrypted it — before this, "one DEK per scope" meant decrypt
-- never had to ask.
--
-- scope_keys becomes multi-version: PK gains `version`, so a scope can
-- have more than one live DEK generation while a rotation is in
-- progress. Old versions are kept, not deleted, once a rotation
-- completes — see hupi-rotate-key's -prune-old-versions.
alter table scope_keys drop constraint scope_keys_pkey;
alter table scope_keys add column version int not null default 1;
alter table scope_keys add primary key (scope_kind, scope_owner, version);

-- Every encrypted-column table records which version encrypted its
-- ciphertext. summary_key_facts gets its own column rather than
-- inheriting its parent summary's — self-describing rows are safer to
-- reason about during rotation than an implicit "always matches the
-- parent" coupling that a future change could quietly break.
alter table episodes add column key_version int not null default 1;
alter table summaries add column key_version int not null default 1;
alter table summary_key_facts add column key_version int not null default 1;
alter table entities add column key_version int not null default 1;

-- key_rotations is live progress, not history — one row per scope,
-- reused across successive rotations (the historical record of *that* a
-- rotation happened lives in audit_log, same as every other action in
-- this project). cursor_table/cursor_id let a killed hupi-rotate-key
-- process resume a batch job instead of restarting it; see
-- internal/rotate's doc comment for why that's safe even without them
-- (a migrated row stops matching the batch job's own WHERE clause), and
-- why they're kept anyway (avoids rescanning already-migrated rows).
create table key_rotations (
    scope_kind   text not null check (scope_kind in ('private', 'shared')),
    scope_owner  text not null,
    from_version int not null,
    to_version   int not null,
    status       text not null default 'in_progress'
                 check (status in ('in_progress', 'completed', 'failed')),
    cursor_table text not null default 'episodes'
                 check (cursor_table in ('episodes', 'summaries', 'entities', 'done')),
    cursor_id    text not null default '',
    started_at   timestamptz not null default now(),
    completed_at timestamptz,
    primary key (scope_kind, scope_owner)
);

grant select, insert, update, delete on key_rotations to hupi_app;
