# Tier 3 (Professional Shared) — Planning Document

Status: **Phases 1-5 of 6 done and verified against a real Postgres
instance (14 passing tests); Phase 6 partially done** (admin CLI shipped
and smoke-tested; RLS and per-tenant DEKs consciously deferred, not
rushed — see §7 for exactly why). Tier 3's core value — shared team
memory with per-user private scope and workspace routing — is
functionally complete. See §7 for the full account, and
[ARCHITECTURE.md § Tiers](../ARCHITECTURE.md) for why Tier 3 was
deliberately sequenced last, and
[DESIGN_VS_BUILT.md #7](DESIGN_VS_BUILT.md) for its place in the overall
gap list.

## 1. Goal

Let a team share memory — decisions, project knowledge, entities — while
each member keeps their own private history and personal voice
(`self_model`) intact. Tiers 1/2 (everything built so far) are exactly the
single-user system this extends; nothing about them changes for a Tier
1/2 deployment that never creates a team.

## 2. Non-goals for this plan

Explicitly out of scope, to keep the effort bounded:

- **No SSO/OIDC.** Authentication is a bearer API key mapping to a user
  row. Federated identity is a later concern, layered on top of the same
  `users` table.
- **No cross-team sharing or fine-grained per-entity ACLs.** Scope is
  exactly two kinds — private to a user, or shared with a team — not an
  arbitrary permission graph.
- **No billing/quota enforcement, no team-management UI.** Team/membership
  changes are direct DB writes or a thin CLI in this plan, not a product
  surface. *(Addendum, post-hardening: the "no UI" half of this was later
  reversed — see [ADMIN_UI.md](ADMIN_UI.md). The CLI, `cmd/hupi-admin`,
  still exists unchanged; the UI is an additional surface over the same
  `internal/auth.Store` operations, not a replacement for this plan's
  reasoning about *what* operations exist. Billing/quota enforcement is
  still not built.)*
- **No per-tenant encryption keys in the first shipped version** — see
  Decision D5 below.

## 3. Key design decisions

Five decisions this plan makes explicitly, so implementation doesn't
improvise them mid-way.

### D1 — Scope is two columns, not one string

MEMORY_FORMAT.md's original sketch used a single field like
`"private:<user_id>"`. **Recommendation: split it** —

```sql
scope_kind  text not null check (scope_kind in ('private', 'shared')),
scope_owner text not null  -- a user id if private, a team id if shared
```

A split is indexable and filterable directly; a combined string requires
parsing in every `WHERE` clause. This goes on `episodes`, `summaries`, and
`entities`.

### D2 — `retrieved_summary_ids`/`retrieved_entity_ids`/`retrieved_episode_ids` become scope-qualified references, not bare id strings

This is the subtlety worth flagging explicitly: today, an entity id like
`project:hupi` is assumed globally unique. Under Tier 3 it isn't — two
different teams can each have their own `project:hupi`. A single private
episode can legitimately retrieve entities from *multiple* teams at once
(a user who's a member of two teams, asking something that matches both).
Bare id strings in `retrieved_entity_ids` would then be ambiguous — you
couldn't tell, from the array alone, which team's `project:hupi` was
actually retrieved.

**Recommendation**: change these three columns from `text[]` to `jsonb`,
storing an array of `{"scope_kind": "...", "scope_owner": "...", "id": "..."}`
objects instead of bare strings. This is a real schema/type change (not
just "add a filter") to `episodes` and to `gateway.RetrievalResult`/
`gateway.Episode`'s corresponding Go fields, and to every place that reads
them back (`store.Trace`, `hupi-trace`'s printer).

The alternative — making entity/summary ids globally unique by encoding
scope into the id string itself (e.g. `team:acme-eng/project:hupi`) — was
considered and rejected: it complicates `slugPart`'s keyword-matching
logic (stage 1 of the retrieval gate) for no real benefit over just
storing the scope alongside the id where it's actually needed.

### D3 — `self_model` stays personal, even inside a team workspace

Per the original design language, a user's communication style shouldn't
change depending on which workspace they're in. **Recommendation:
`buildAnchor` always loads the *requesting user's* `self_model`**
(`scope_kind='private', scope_owner=<user_id>`), regardless of whether the
request is scoped to a personal or team workspace. Only the
entities/summaries layered on top of the anchor are workspace-scoped.

This means `entities.id` for self_model changes from the current global
singleton `self_model:primary` to one row per user
(`self_model:<user_id>`).

### D4 — Workspace routing is path-based

`POST /v1/chat/completions` keeps its current meaning (private scope,
resolved from the caller's identity). A new route,
`POST /v1/team/{team_id}/chat/completions`, scopes the turn to that team —
rejecting the request with `403` if the authenticated user isn't a member.
No per-message flag, matching the earlier design rationale (a per-message
"is this shared?" toggle is the kind of thing people forget).

### D5 — Encryption ships with a single deployment-wide DEK in v1; per-tenant DEKs are a named follow-up, not silently dropped

ARCHITECTURE.md's storage-security design floated per-tenant DEKs for
Tier 3 — each team's data encrypted under its own key, so one team's
compromised key can't expose another's. Building real multi-DEK key
management (generation, wrapping, rotation, lookup-by-scope) is itself a
project roughly the size of everything in `internal/crypto`/
`internal/bootstrap` today. **Recommendation: ship v1 of Tier 3 with the
existing single deployment-wide DEK**, with isolation resting on scope
filtering (D1/D2) and Postgres row-level security (§6) — and treat
per-tenant DEKs as Phase 6 (§7), an explicit, named hardening step, not an
assumption anyone should make is already true.

## 4. Schema changes

New tables:

```sql
create table users (
    id         text primary key,        -- "user:sujith"
    email      text,
    created_at timestamptz not null default now()
);

create table teams (
    id         text primary key,        -- "team:acme-eng"
    name       text not null,
    created_at timestamptz not null default now()
);

create table team_members (
    team_id text not null references teams(id),
    user_id text not null references users(id),
    role    text not null default 'member' check (role in ('member', 'admin')),
    primary key (team_id, user_id)
);

create table api_keys (
    key_hash   text primary key,        -- sha256 of the raw key; the raw key is never stored
    user_id    text not null references users(id),
    created_at timestamptz not null default now(),
    revoked_at timestamptz
);
```

Changes to existing tables:

```sql
alter table episodes  add column scope_kind  text not null default 'private',
                       add column scope_owner text not null default 'user:default';
alter table summaries add column scope_kind  text not null default 'private',
                       add column scope_owner text not null default 'user:default';
alter table entities  add column scope_kind  text not null default 'private',
                       add column scope_owner text not null default 'user:default';

alter table episodes  add constraint episodes_scope_kind_check  check (scope_kind in ('private','shared'));
alter table summaries add constraint summaries_scope_kind_check check (scope_kind in ('private','shared'));
alter table entities  add constraint entities_scope_kind_check  check (scope_kind in ('private','shared'));

create index episodes_scope_idx  on episodes  (scope_kind, scope_owner);
create index summaries_scope_idx on summaries (scope_kind, scope_owner);
create index entities_scope_idx  on entities  (scope_kind, scope_owner);
```

And per D2:

```sql
-- was: retrieved_summary_ids text[] not null default '{}'  (and entity/episode equivalents)
alter table episodes
    drop column retrieved_summary_ids,
    drop column retrieved_entity_ids,
    drop column retrieved_episode_ids,
    add column retrieved_refs jsonb not null default '[]';
    -- [{"kind": "summary"|"entity"|"episode", "scope_kind": "...", "scope_owner": "...", "id": "..."}]
```

`entities.id` and `summaries.id` stay globally-scoped-but-not-unique text
(uniqueness becomes `(scope_kind, scope_owner, id)` /
`(scope_kind, scope_owner, period, level)` via a composite unique
constraint, not the primary key alone) — this is the one piece of D2's
reasoning that *does* still require a constraint change, just not an id
format change.

## 5. Component-by-component change list

| Component | Change |
|---|---|
| `schema/` | New migration (`0002_tier3.sql`) per §4, plus a backfill (§7) |
| `internal/store/retrieve.go` | Every query (`stage1EntityMatches`, both `buildAnchor` queries, `vectorSearchSummaries`, `vectorSearchEpisodes`) gains a scope filter; `buildAnchor`'s self_model lookup becomes user-specific (D3); result assembly switches from bare id slices to the `retrieved_refs` shape (D2) |
| `internal/store/capture.go` | Writes `scope_kind`/`scope_owner` on every episode; writes `retrieved_refs` instead of three separate arrays |
| `internal/store/trace.go` | Reads `retrieved_refs`, resolves each by its own scope, not a bare id lookup |
| `internal/gateway/handler.go` | `Retriever`/`Capturer` interfaces gain an identity/scope parameter (breaking change); new auth middleware; new `/v1/team/{id}/...` route (D4); `HandleFeedback` scopes the feedback row to the same scope as the episode it refers to |
| `internal/consolidation/*` | `RunDaily`/`RunRollup` take a scope parameter; `upsertEntities`/`nextSummaryID` filter and constrain by scope; a new team-voice system prompt (`prompts.go`) for `scope_kind='shared'` runs |
| `internal/bootstrap/bootstrap.go` | Loads `users`/`teams`/`api_keys` alongside providers; still one DEK (D5) |
| `cmd/hupi` | Wires the auth middleware and the new route |
| `cmd/hupi-consolidate` | Enumerates scopes (query distinct `(scope_kind, scope_owner)` with new episodes) and loops `RunDaily` per scope, instead of one global call |
| `cmd/hupi-trace`, `cmd/hupi-selfcheck` | Both call `Retrieve`/`Trace` directly today with no identity — need an explicit `-user`/`-team` flag once those calls require a scope |
| New: `cmd/hupi-admin` (or similar) | Minimal CLI for creating users/teams/memberships/API keys — nothing in this plan builds a UI for this |

## 6. Row-level security, as defense in depth

Application-level `WHERE scope_kind = ... and scope_owner = ...` filtering
is necessary but a single missed clause across ~15 call sites is a
cross-tenant data leak, not a cosmetic bug. Recommendation: also enable
Postgres RLS on `episodes`/`summaries`/`entities`, with a policy tied to a
session-local setting (`set_config('hupi.user_id', ..., true)` /
`hupi.team_ids`) set once per request in `internal/store`. This makes a
forgotten application-level filter fail safe (return nothing) instead of
leaking silently.

## 7. Phased rollout

Each phase should be independently buildable and reviewable — not one
large change.

1. **Schema + identity foundation. DONE.** `schema/0002_tier3_phase1_identity.sql`
   — `users`/`teams`/`team_members`/`api_keys`, `scope_kind`/`scope_owner`
   on `episodes`/`summaries`/`entities`, `entities`' primary key changed to
   `(scope_kind, scope_owner, id)` (verified safe: nothing held an FK into
   `entities.id`), backfill to `scope_kind='private',
   scope_owner='user:default'` via column defaults. Applied to a real
   `pgvector/pgvector:pg16` container and verified: the same entity id
   coexists across two scopes, a true duplicate within one scope is
   correctly rejected by the new composite key.
2. **Query-layer scope filtering. DONE**, and widened along the way:
   building this surfaced that `retrieved_summary_ids`/`retrieved_entity_ids`/
   `retrieved_episode_ids` (bare `text[]`) can't disambiguate which scope a
   retrieved id came from once ids are only unique per-scope — so D2's
   `retrieved_refs jsonb` change (originally slated to land "in Phase 1/2
   together," see §10) landed now rather than later:
   `schema/0003_tier3_phase2_retrieved_refs.sql`, a new `internal/identity`
   package (`Scope`, `Ref`), and every call site in `internal/store` and
   `internal/consolidation` updated to filter and write scope. All of
   `Retriever`/`Capturer`/`Runner.RunDaily`/`RunRollup`/`Correct` now take
   an explicit `identity.Scope` parameter — callers (gateway, all `cmd/`
   binaries) pass `identity.DefaultScope` until Phase 3 supplies a real
   one, so behavior for a single-scope deployment is unchanged. Verified
   with a real integration test
   (`internal/store/scope_isolation_test.go`, run against the same live
   Postgres container) proving a shared-scope retrieval cannot see a
   private episode or a same-id entity from another scope — passing, not
   just compiling.
3. **Auth + identity threading. DONE.** New `internal/auth` package
   (`Resolve`, `GenerateKey`/`CreateUser`/`CreateAPIKey`/`CreateTeam`/
   `AddTeamMember`), `identity.Identity` added to `internal/identity`.
   `gateway.Handler` gained an `Auth Authenticator` field — nil (default)
   means Tier 1/2 mode, no `Authorization` header required, unchanged
   behavior; set (via `cmd/hupi`'s `HUPI_REQUIRE_AUTH=true`, opt-in on
   purpose so an existing deployment never starts requiring auth it wasn't
   configured for) means every request needs a valid `Bearer` key,
   rejected with 401 otherwise. `hupi-trace`/`hupi-correct` gained
   `-scope-kind`/`-scope-owner` flags; `hupi-selfcheck`'s `Probe.Scope`
   field (added during Phase 2) already covered its case. Verified with
   real integration tests (`internal/auth/auth_test.go`, live Postgres):
   a valid key resolves to the right user plus its team memberships, an
   unknown key and a revoked key both correctly fail closed.
   The `Retriever`/`Capturer` signature change itself landed in Phase 2,
   ahead of this bullet's original schedule, once it became clear scope
   had to be a parameter to filter queries at all.
4. **Workspace routing. DONE.** `POST /v1/team/{team_id}/chat/completions`
   and `.../feedback`, both requiring `h.Auth` to be configured — with no
   auth there's no way to prove team membership, so a team request always
   403s rather than being silently allowed. `gateway.Retriever.Retrieve`
   now takes both `actingUser` and `workspace` scopes (D3): self_model
   always anchors to the caller's own private scope, everything searched
   is scoped to the workspace. `HandleChatCompletions`/`HandleFeedback`
   were refactored into thin wrappers around `handleChatCompletionsScoped`/
   `handleFeedbackScoped` shared with the new team handlers. Verified with
   real tests: `internal/gateway/team_routing_test.go` (DB-independent —
   no-auth-configured fails closed, a member is authorized into the right
   `actingUser`/`workspace` pair, a non-member gets 403 distinct from an
   invalid key's 401) and `internal/store/scope_isolation_test.go`'s new
   `TestSelfModelAnchorsToActingUser` (live Postgres — a team-workspace
   turn anchors to the acting user's own self_model, never the
   workspace's).
5. **Scope-aware consolidation. DONE.** `cmd/hupi-consolidate` now calls
   `loadActiveScopes` (every user's private scope + every team's shared
   scope, from the `users`/`teams` tables) and loops `RunDaily` over all
   of them, logging and continuing past a single scope's failure rather
   than aborting the whole batch — one team's bad day shouldn't block
   everyone else's. The team-voice prompt (`teamSummarySystemPrompt`) had
   already landed in Phase 2 as a natural byproduct of threading scope
   through `generateSummary`. Verified against live Postgres
   (`cmd/hupi-consolidate/main_test.go`).
6. **Deferred hardening — partially done, rest consciously deferred.**
   - **Admin CLI: DONE.** `cmd/hupi-admin` (`create-user`, `create-team`,
     `add-member`, `create-key`), a thin wrapper over `internal/auth.Store`
     — smoke-tested end to end against live Postgres (provision a
     user+team+membership+key, confirm the key resolves correctly, per
     §9's testing plan).
   - **RLS policies and per-tenant DEKs: planned in detail, not yet
     implemented.** See [HARDENING_PLAN.md](HARDENING_PLAN.md) for the
     full design (why both need the same underlying `store`/`consolidation`
     transaction refactor, the two-scope-variable RLS policy shape D3's
     `actingUser`/`workspace` split requires, and the phased rollout).
   - **RLS policies: deliberately not done.** This turned out bigger than
     "add policies": Postgres RLS needs a session-local variable
     (`current_setting('hupi.scope_owner', true)`) checked per query, which
     only works reliably scoped to a transaction (`SET LOCAL` inside a
     `BEGIN`/`COMMIT`) — `internal/store`'s methods currently issue
     independent `*sql.DB` calls, not one transaction per request. Enabling
     RLS without that refactor would silently return zero rows everywhere
     (Postgres denies by default under RLS unless a policy matches), which
     is worse than not having it. Doing this properly means converting
     `store`'s per-call DB access to a per-request `*sql.Tx` — a refactor
     comparable in size to Phase 2, not a checkbox, and risky to do
     partially. Left as a clearly scoped follow-up rather than rushed.
   - **Per-tenant DEKs (D5): deliberately not done**, per D5's own
     recommendation — a full multi-DEK key-management system (generation,
     wrapping, rotation, lookup-by-scope) is roughly the size of everything
     in `internal/crypto`/`internal/bootstrap` today. Isolation currently
     rests entirely on the query-layer scope filtering verified in Phase 2
     (and would be strictly stronger once RLS lands too).
   - **Audit-log surface** ("who accessed what shared memory"): not built.
     The raw material exists (every episode already records its own
     scope), but there's no dedicated query/report surface for it yet.

## 8. Migration plan for existing Tier 1/2 data

For any deployment that already has data before this ships:

1. Add the new columns with defaults (`scope_kind='private'`,
   `scope_owner='user:default'`) — existing rows become valid Tier 3 rows
   with no data movement.
2. Create a `users` row for `user:default` (or the deployment's actual
   owner id) so the FK-adjacent scope references resolve to something
   real.
3. Rename the existing singleton `self_model:primary` entity to
   `self_model:user:default` per D3.
4. No `teams` rows are created automatically — sharing is opt-in from this
   point forward.

## 9. Testing & validation plan

Tier 3 is the first place in this codebase where a bug has a *security*
consequence (cross-tenant leakage), not just a correctness one — per
[DESIGN_VS_BUILT.md #6](DESIGN_VS_BUILT.md), there's no integration test
suite at all yet, which is a bigger problem here than anywhere else in the
system. Before Tier 3 ships, at minimum:

- A user in Team A's workspace cannot retrieve Team B's summaries/entities
  via any gate outcome (`skipped`/`partial`/`full`), even via a crafted
  query designed to trigger a vector-search near-match.
- A user's private episode never appears in a teammate's retrieval, even
  when both are members of the same team.
- `self_model` isolation: User A's `self_model` is never returned by User
  B's `buildAnchor` call, in any workspace.
- `hupi-trace`/`hupi-feedback`/`hupi-selfcheck` all respect scope — none
  of them should be able to read across a scope boundary the calling
  identity doesn't have.
- RLS policies (if implemented in this pass) are tested by attempting a
  query with the session variable unset or set to a different tenant, and
  confirming zero rows return rather than an error being the only thing
  standing between the request and someone else's data.

This requires the `docker-compose` Postgres test harness that
DESIGN_VS_BUILT.md #6 already calls out as missing — building that harness
is effectively a prerequisite for starting Tier 3 implementation, not
optional parallel work.

## 10. Risks

- **Scope-filter omission is the single biggest risk in this entire
  plan.** ~15 call sites need the same discipline applied correctly;
  RLS (§6) is the mitigation, not a substitute for getting the
  application-level filters right.
- **D2's `retrieved_refs` change touches every consumer of the old three
  array columns** (`Capture`, `Trace`, `hupi-trace`'s printer, and
  `gateway.RetrievalResult`/`Episode` themselves) — it's a schema-shape
  change, not just a new column, and should land in Phase 1/2 together so
  nothing is built twice.
- **Team-voice consolidation quality is unvalidated.** Nothing has tested
  whether an LLM actually produces a coherent "neutral team voice" summary
  distinct from an individual's — this may need prompt iteration once
  real team data exists to consolidate.
- **Cost**: per-scope consolidation (Phase 5) multiplies consolidation LLM
  calls by the number of active scopes (users + teams), not just active
  users — worth modeling before a team with many members generates a
  proportionally large daily bill.
