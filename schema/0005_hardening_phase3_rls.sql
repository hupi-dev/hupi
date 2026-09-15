-- Hardening Phase 3 (docs/HARDENING_PLAN.md D2): row-level security on the
-- three scope-carrying memory tables, as a second, independent isolation
-- layer beneath the application-level scope filtering already in every
-- query (verified in schema/0002-0003's Go call sites). Enforced only for
-- non-owner roles (see schema/0004's hupi_app) — the table owner and any
-- superuser still bypass RLS by default, which is intentional: this is
-- deliberately not FORCE ROW LEVEL SECURITY, so migrations and direct DBA
-- access aren't blocked by it.
--
-- Two scope-variable pairs, not one, because a single logical operation
-- (Store.buildAnchor: self_model from the acting user, everything else
-- from the workspace — docs/TIER3_PLAN.md D3) legitimately reads from two
-- different scopes in one transaction. USING allows a row matching EITHER
-- pair (read); WITH CHECK only allows the workspace pair (write) — nothing
-- should ever be written into a user's private scope as a side effect of
-- a team-workspace request.
--
-- current_setting(..., true) returns NULL when unset, and
-- `NULL = anything` is never true in SQL — a transaction that forgets to
-- call dbscope.SetSession denies all rows, fails closed, not open.

alter table episodes enable row level security;

create policy scope_isolation on episodes
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

alter table summaries enable row level security;

create policy scope_isolation on summaries
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

alter table entities enable row level security;

create policy scope_isolation on entities
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

-- summary_key_facts is deliberately NOT RLS-protected here: it has no
-- scope_kind/scope_owner of its own (see docs/HARDENING_PLAN.md — adding
-- a denormalized copy was out of scope for this pass), is only ever
-- INSERTed (from within storeSummary's already-scoped transaction), and
-- has no direct SELECT path anywhere in the codebase today — it's only
-- ever reached by joining through summaries, which is RLS-protected. If a
-- direct query path is ever added, revisit this.
