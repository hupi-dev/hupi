# RLS + Per-Tenant DEKs — Planning Document

Status: **All 5 phases done and verified against live Postgres — RLS
(Phases 1-3) and per-tenant DEKs (Phases 4-5) are both fully implemented.**
17/17 tests passing with `-count=1` (no cache). See §6 for the full
account, including real bugs both rollouts caught in existing test
helpers on the first honest test runs. This is the two items
[TIER3_PLAN.md §7 Phase 6](TIER3_PLAN.md) deliberately deferred rather
than half-build: Postgres row-level security as a second, independent
isolation layer beneath the application's own scope filtering, and
per-tenant data encryption keys so one team's compromised key can't
expose another's. Read TIER3_PLAN.md first — this assumes the scope
model (`identity.Scope`, `identity.Ref`), the `actingUser`/`workspace`
split (D3), and the current single-DEK encryption model (D5) it
established.

## 1. Goal

Add two independent hardening layers on top of the scope filtering
already shipped and tested in Tiers 1-3:

1. **RLS**: even a query that forgot its `WHERE scope_kind = ... AND
   scope_owner = ...` clause — a bug in code not yet written — should
   still be refused by Postgres itself, not just by application discipline.
2. **Per-tenant DEKs**: a compromised or leaked encryption key should only
   expose the one scope it belongs to, not the whole deployment.

Neither changes what the system does from a user's perspective. Both are
"in case the first layer has a bug" — done right, this work should be
invisible to anyone using HUPI.

## 2. Non-goals

- **Not doing key rotation for an existing scope's DEK** (re-encrypting
  every row a tenant already has under a new key) — this plan covers
  DEKs coming into existence and being used, not a rotation workflow for
  one already in use. Rotating the shared KEK (re-wrapping every DEK) is
  in scope; re-encrypting tenant *data* under a new DEK is not.
  *(Addendum: built later — see [GAP_CLOSURE_PLAN.md §4.4](GAP_CLOSURE_PLAN.md),
  `internal/rotate`, `hupi-rotate-key`. This required `KeyStore` itself to
  become version-aware, which this plan's single-DEK-per-scope design
  didn't anticipate.)*
- **Not moving off a single, shared KEK.** Per-tenant applies to the DEK
  layer only — see §4 D2 below for why.
- **Not building the audit-log surface** ("who accessed what shared
  memory") — a third, separately-scoped Phase 6 item, not covered here.
  *(Addendum: built later — see [GAP_CLOSURE_PLAN.md §4.3](GAP_CLOSURE_PLAN.md),
  `internal/audit`, `hupi-audit`.)*

## 3. Why these are bigger than they sound

Both items run into the same underlying fact: `internal/store`'s methods
each issue independent `*sql.DB` calls today, not one transaction (or one
resolved key) per logical operation. That was a reasonable design for
Tiers 1-3 — nothing needed request-scoped state — but both RLS and
per-tenant DEKs *do* need something resolved once per request and used
consistently across every DB call that request makes. Getting this wrong
doesn't fail loudly:

- A missing `SET LOCAL` under RLS doesn't error — Postgres just returns
  zero rows (RLS denies by default), which looks like "no memories yet,"
  not a bug.
- Using the wrong scope's DEK doesn't error either — `Encryptor.Decrypt`
  fails with an authentication error from AES-GCM, which is at least loud,
  but only if the refactor is complete; a half-migrated call site could
  easily encrypt under the right key and decrypt under a stale cached one.

Both are worth doing carefully, together, rather than bolted on piecemeal.

## 4. Key design decisions

### D1 — One transaction-scoped helper serves both concerns

Rather than doing an RLS pass and a separate DEK pass (each touching the
same ~15-20 call sites in `internal/store`/`internal/consolidation`), do
one refactor that introduces a single per-operation context carrying both
the RLS session-variable setup *and* the resolved encryptor(s), and
migrate every call site once:

```go
// scopedOp is resolved once per logical Store/Runner operation and reused
// for every DB call and every encrypt/decrypt that operation needs.
type scopedOp struct {
    tx         *sql.Tx
    actingEnc  *crypto.Encryptor // actingUser's scope's key
    workspaceEnc *crypto.Encryptor // workspace's scope's key
}
```

`Store.Retrieve`, `Store.Capture`, `Store.Trace`, and `Runner`'s methods
each open a `scopedOp` at the top and use it throughout, instead of
reaching for `s.db`/`s.enc` directly. This is the single biggest
mechanical change in this plan, and doing RLS and DEKs as one pass over
these call sites is meaningfully cheaper than two.

### D2 — RLS needs two scope-variable pairs, not one, because of D3

`buildAnchor` (docs/TIER3_PLAN.md D3) already reads from two different
scopes in one call: `self_model` from `actingUser`, the latest summary
pointer from `workspace`. A single `hupi.scope_kind`/`hupi.scope_owner`
pair can't express "allow rows matching *either* of these two scopes" —
the policy needs two pairs, and rows are allowed if they match either:

```sql
create policy scope_isolation on entities for all
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
```

`USING` (what you can read) allows either scope; `WITH CHECK` (what you
can write) only allows the workspace scope — nothing should ever be
*written* into a user's private scope as a side effect of a team-workspace
request. `episodes` and `summaries` get the same two-clause policy;
`current_setting(..., true)` returns NULL when unset, and `NULL = anything`
is never true in SQL, so an operation that forgets to set these variables
correctly gets zero rows — fails closed by construction, not by
convention.

### D3 — Don't hold a transaction open across an LLM/embedding call

`Store.Retrieve` calls the embedding provider (a network round trip)
*between* its pre-embedding reads (anchor, stage-1 entity scan) and its
post-embedding reads (the two vector searches). `Runner.generateSummary`
calls the consolidation LLM between loading source episodes and writing
the summary. Neither should happen inside an open Postgres transaction —
holding a transaction across a slow external call risks connection pool
exhaustion and idle-in-transaction timeouts for no benefit.

**Recommendation**: each logical operation opens *multiple short
transactions*, not one long one — a `scopedOp` per contiguous group of DB
calls, closed before any external call, re-opened after. This is a
deliberate, load-bearing design choice, not a shortcut: it changes "one
transaction per request" into "one or more transactions per request,
each fully containing only DB work."

### D4 — RLS requires a non-owner, non-superuser database role

Postgres RLS is bypassed by the table owner and by superusers unless the
table is explicitly marked `FORCE ROW LEVEL SECURITY`. Today,
`HUPI_DATABASE_URL` is used both to run migrations (as the owning role)
and to run the live app — if that continues, RLS silently does nothing for
the app's own queries. **Recommendation**: introduce a dedicated
`hupi_app` role with `SELECT`/`INSERT`/`UPDATE`/`DELETE` (no `CREATE`, no
ownership) on the scoped tables, and have the running app (`cmd/hupi`,
`cmd/hupi-consolidate`, etc.) connect as `hupi_app` via a new
`HUPI_APP_DATABASE_URL` (falling back to `HUPI_DATABASE_URL` if unset, so
a dev setup without the extra role still works — with a startup warning
that RLS isn't actually enforced in that mode).

### D5 — Per-tenant DEKs live in a table, not a file, once there's more than one

`hupi.dek.wrapped` (today, singular) becomes a table:

```sql
create table scope_keys (
    scope_kind  text not null check (scope_kind in ('private', 'shared')),
    scope_owner text not null,
    wrapped_dek bytea not null,
    created_at  timestamptz not null default now(),
    primary key (scope_kind, scope_owner)
);
```

Storing wrapped (KEK-encrypted) DEKs in the same Postgres instance as the
data they protect is safe — the KEK itself never leaves the deployment's
secret store (env var today, a real KMS in a hardened build), so
possessing the database alone still isn't enough to decrypt anything.

### D6 — DEK creation is eager, at provisioning time, not lazy

A scope's DEK is generated when the scope is created — inside
`internal/auth.Store.CreateUser`/`CreateTeam` — not lazily on first write.
This keeps key lifecycle colocated with identity lifecycle: `hupi-admin
create-team` is the one moment "this tenant now exists," and it should be
the one moment "this tenant's key now exists" too, rather than scattered
into whichever code path happens to write first.

### D7 — Migrating existing data needs no re-encryption

The existing single global DEK doesn't need to be replaced — it becomes
`private:user:default`'s dedicated entry in `scope_keys` (wrapped under
the same KEK, registered under that one scope). Every *new* user or team
created after this migration gets its own, distinct, freshly generated
DEK. This avoids the expensive and risky alternative (decrypt everything
under the old key, re-encrypt under new per-scope keys) entirely — it's a
one-row `INSERT`, not a data migration.

## 5. Component-by-component change list

| Component | Change |
|---|---|
| `schema/` | New migration: RLS `ENABLE`/policies on `episodes`/`summaries`/`entities` (D2); `scope_keys` table (D5); `hupi_app` role + grants (D4) |
| `internal/crypto` | New `KeyStore` type: `GetOrCreate(ctx, scope) (*Encryptor, error)`, wraps/unwraps via the existing `WrapDEK`/`UnwrapDEK`, caches resolved `*Encryptor` per scope in memory (avoid unwrapping on every field access) |
| `internal/auth` | `CreateUser`/`CreateTeam` call `KeyStore.GetOrCreate` for the new scope's DEK (D6) |
| `internal/store` | New `scopedOp` helper (D1); every method converted from `s.db`/`s.enc` to `tx`/resolved encryptor within a `scopedOp`; split into pre-embed/post-embed transaction groups in `Retrieve` (D3) |
| `internal/consolidation` | Same `scopedOp` treatment; `storeSummary`'s existing transaction gains the `SET LOCAL` calls; `generateSummary`'s LLM call stays outside any transaction |
| `internal/bootstrap` | `loadEncryptor` becomes `loadKeyStore`, returning a `*crypto.KeyStore` instead of a single `*Encryptor`; migrates the existing wrapped-DEK file into `scope_keys` on first run after upgrade (D7) |
| `cmd/hupi`, `cmd/hupi-consolidate`, etc. | Connect via `HUPI_APP_DATABASE_URL` if set (D4); pass the `KeyStore` instead of a single `Encryptor` into `store.New`/`consolidation.New` |

## 6. Phased rollout

1. **`hupi_app` role + connection split (D4). DONE.**
   `schema/0004_hardening_phase1_app_role.sql` creates `hupi_app`
   (no password committed — set separately, out of band) with grants on
   every scoped and identity table; `internal/bootstrap.resolveDatabaseURL`
   prefers `HUPI_APP_DATABASE_URL`, falling back to `HUPI_DATABASE_URL`
   with a loud `slog.Warn`. Verified: the full test suite, run against
   `hupi_app`'s connection, passes unchanged.
2. **`scopedOp` refactor, no RLS or per-tenant keys yet. DONE.** New
   `internal/dbscope` package (`Querier`, `SetSession`, `Run`) — every call
   site in `internal/store` and `internal/consolidation` converted from
   `s.db`/`r.db` direct calls to `dbscope.Run`-managed short transactions,
   split around every external LLM/embedding call (D3) and around
   `buildAnchor`'s genuine two-scope read (D2). Added
   `internal/consolidation/runner_test.go`'s `TestRunDaily_WritesGroundedSummary`
   specifically because `storeSummary`'s transaction path (the biggest
   single change in this step) had no existing test coverage — it now
   exercises the full insert-summary/insert-key-facts/upsert-entity/embed
   path end to end against live Postgres.
3. **RLS policies (D2). DONE** (without `FORCE ROW LEVEL SECURITY` — see
   D4's note on why the table owner/superuser stay exempt on purpose).
   `schema/0005_hardening_phase3_rls.sql`. **This step caught real bugs on
   the first honest test run**: several existing tests' *setup/cleanup
   helpers* issued raw, unscoped SQL (direct `s.db.Exec`, bypassing
   `dbscope`) — under RLS those inserts were rejected outright (loud
   failure, `go test -count=1` surfaced it immediately), but a couple of
   *reads* (a cleanup `DELETE`, an embedding-seed verification) would have
   silently matched zero rows instead of erroring, which would have made
   the affected tests pass for the wrong reason rather than fail. All
   fixed to route through `dbscope.Run` like real application code must.
   Added three tests that verify RLS itself, not just that the app still
   works on top of it: `internal/store/rls_test.go` —
   `TestRLS_DeniesCompletelyUnscopedRead`,
   `TestRLS_TwoScopePolicyIsExactNotBroad` (the OR in D2's policy grants
   exactly the two scopes set, not anything broader),
   `TestRLS_RejectsWriteOutsideWorkspace` (`WITH CHECK` rejects a row
   claiming a scope other than the transaction's workspace). 14/14 tests
   passing with `-count=1` (no cache) against live Postgres.
4. **`scope_keys` table + `KeyStore` (D5, D6, D7). DONE.**
   `schema/0006_hardening_phase4_scope_keys.sql`; new
   `internal/crypto.KeyStore` (`GetOrCreate`, in-memory cache, handles the
   concurrent-creation race via `ON CONFLICT DO NOTHING` + reload).
   `internal/auth.Store.CreateUser`/`CreateTeam` provision each new scope's
   DEK eagerly (D6). `internal/bootstrap.loadKeyStore` migrates the legacy
   single-file DEK into `scope_keys` under `private:user:default` on first
   run (D7) — smoke-tested end to end with a real simulated legacy
   deployment (a genuine wrapped-DEK file, no prior `scope_keys` row):
   confirmed the row lands correctly and `user:default` gets created.
   `store`/`consolidation` fully switched from a single `*Encryptor` field
   to `keys.GetOrCreate(ctx, scope)` at every encrypt/decrypt call site —
   this also surfaced a subtle test-fixture bug on the way: several test
   helpers built a *standalone* `Encryptor` from a fixed key to seed
   fixtures, which after this change no longer matches whatever key
   `KeyStore` actually resolves for that scope. Fixed by having every test
   fixture resolve its encryptor the same way production code does
   (`keys.GetOrCreate`), not a parallel one.
5. **Cross-checks. DONE.** `internal/crypto/keystore_test.go`:
   `TestKeyStore_ScopesGetDistinctKeys` (scope A's ciphertext fails under
   scope B's resolved key — the actual per-tenant guarantee, not just that
   the plumbing compiles), `TestKeyStore_SameScopeIsConsistent` (two
   independent `KeyStore` instances against the same DB resolve to the
   same usable key — simulates two processes), `TestKeyStore_WrongKEKCannotUnwrap`
   (a `KeyStore` built with the wrong KEK fails to unwrap an existing
   scope's DEK, rather than silently producing a wrong-but-usable key).
   17/17 tests passing (`-count=1`, live Postgres) across the whole
   project.

## 7. Testing plan

Building on the pattern from `internal/store/scope_isolation_test.go` and
`internal/gateway/team_routing_test.go` (real Postgres, `HUPI_TEST_DATABASE_URL`):

- **RLS holds even when application code doesn't ask it to.** Issue a raw
  query against `hupi_app`'s connection with no session variables set;
  assert zero rows, not an error and not real data.
- **RLS holds under the two-scope policy specifically.** A query with only
  `hupi.acting_scope_kind`/`owner` set (not workspace) sees the acting
  user's own rows but not the workspace's, and vice versa — this is the
  exact case D2 exists to handle correctly, not just permissively.
- **Existing scope-isolation tests still pass** with RLS enabled — proof
  the two layers agree, not just that RLS doesn't crash anything.
- **DEK isolation**: decrypting scope A's stored ciphertext with scope B's
  resolved `*Encryptor` fails (`crypto.Encryptor.Decrypt`'s AES-GCM
  authentication check should reject it) — proves keys are actually
  different per scope, not the same key reused under different lookups.
- **Migration correctness**: after running the D7 migration against a
  Tier 1-3 database with existing data, all pre-existing rows still
  decrypt correctly using `private:user:default`'s (migrated) key.

## 8. Risks

- **This is the highest-risk change made to HUPI so far.** Every prior
  phase added a capability; this one changes how *every existing* read
  and write path talks to the database. A mistake here doesn't fail
  loudly (§3) — extra care in review and the zero-rows/wrong-key tests in
  §7 are load-bearing, not optional extras.
- **Connection pool sizing.** Short-lived transactions per operation
  (D3) mean more `BEGIN`/`COMMIT` round trips than today's single-call
  pattern — worth a quick benchmark against the live Postgres container
  before assuming this is free.
- **`current_setting` typos are invisible.** A GUC name typo'd differently
  between the policy definition and the Go code setting it doesn't error
  at all — it just means the policy's `current_setting(...)` call reads a
  variable that was never set, so it fails closed (returns nothing) rather
  than open. Safe by construction (D2's NULL-comparison behavior), but
  worth a startup self-check that round-trips a known scope through a real
  RLS-protected read before serving traffic.
- **Per-tenant DEKs increase key-loss blast radius per-tenant, but that's
  the point.** Losing `private:user:default`'s DEK today loses everything;
  after this change, losing one team's DEK loses only that team's data —
  a strict improvement, but it also means there are now N places a key can
  be lost instead of one, and `scope_keys` itself becomes a
  single-point-of-failure table worth backing up with the same care as the
  rest of the database.
