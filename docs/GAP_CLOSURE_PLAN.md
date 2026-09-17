# Gap Closure Plan — export/import, rollup scheduling, audit log, key rotation, containerization

`BUSINESS_PROCESS.md` §12 lists five things designed but not built. This
plan closes all five. Scope for each was set by explicit choice, not my
default — see §1. This is comparable in size to `TIER3_PLAN.md` and
`HARDENING_PLAN.md` combined; it's phased for the same reason those were
(§6), not because any one piece is optional.

## 1. Scope, as chosen

| Item | Chosen scope |
|---|---|
| Export/import | **Full**: per-scope export, whole-deployment admin export, and merge-capable import (not just fresh-scope) |
| Audit log | **Writes + retrievals + admin access** — the most complete of the three options offered |
| Key rotation | **Online, resumable, batched** — no downtime, safe to run while the gateway serves that scope's traffic |
| Containerization | **Dockerfile + Kubernetes manifests + Helm chart**. TLS is out of scope — sits behind an ingress that terminates it |
| Rollup scheduling | Not asked — the existing docs already fix the period-naming convention (`2026-W37`/`2026-09`/`2026`) and `Runner.RunRollup` already exists. Folded into `cmd/hupi-consolidate`'s nightly run rather than a new cron entry. |

## 2. Non-goals

Explicit, so none of these get assumed into scope mid-implementation:

- **No self-service triggers.** Export, rotation, and rollup are all
  operator-run CLI tools, same trust model as `hupi-admin`/`hupi-trace`
  today — not new HTTP endpoints, not buttons in the admin UI. The admin
  UI's own non-goals (`ADMIN_UI.md`) don't change.
- **No cross-schema-version import.** `hupi import` only ever loads an
  HPMF bundle produced by the same major version of `hupi export` it's
  running against. No migration-on-import logic.
- **No new authorization layer inside the CLI tools themselves.**
  Whoever can run `hupi-export`/`hupi-rotate-key`/`hupi-audit` against the
  database already has full operator trust — same as every existing
  operator tool. These don't re-derive per-scope permission checks.
- **No bundled Postgres in the Helm chart.** Stays external/bring-your-own,
  same philosophy as `install.sh` and `INSTALL.md` today. The chart takes
  a connection string, not a database.
- **Rollup scheduling doesn't get its own cron entry or binary.** It's a
  calendar check inside the existing nightly `hupi-consolidate` run.

## 3. Why key rotation is bigger than it sounds

Every other item in this plan is additive: new table, new binary, new
manifest. Key rotation is not additive — it changes what every existing
encrypted row means.

Today (`internal/crypto/keystore.go`): one DEK per scope, looked up by
`identity.Scope` alone. `Encryptor.Decrypt` has no way to know which DEK
version encrypted a given blob, because there's only ever one. **Online
rotation requires every encrypted row to carry a marker for which DEK
version encrypted it**, so a row written under the old key and a row
written under the new key can both be decrypted correctly during the
transition. That's a schema change on `episodes`, `summaries`, and
`entities` (§5), a new versioned shape for `scope_keys`, and a
`KeyStore` API that resolves "the Encryptor for scope X, version N," not
just "the Encryptor for scope X" — which then has to flow through every
call site that currently calls `keys.GetOrCreate(ctx, scope)` in
`internal/store`, `internal/consolidation`, and `cmd/hupi-trace`.

This is why it's phased last (§6): it touches the most call sites, and
by the time we get there, the audit-log work will already have built and
tested the "resumable, cursor-tracked background job" pattern this reuses
for batched re-encryption.

## 4. Design decisions

### 4.1 Rollup scheduling

`cmd/hupi-consolidate` gains calendar-boundary checks, run after
`RunDaily` succeeds for each scope, each independently idempotent (a
`select 1 from summaries where scope=... and level=... and period=...
and supersedes is null` guard before generating — `RunDaily` doesn't have
this guard today and a cron mis-fire duplicating a *daily* summary is a
pre-existing, out-of-scope gap; rollups get the guard because this is new
code and a scheduler that isn't safe to double-fire isn't a scheduler):

- **Weekly**: when `date` is a Monday, roll up the just-finished ISO week
  (Mon-Sun) — 7 `daily` periods → one `weekly` period (`2026-W37`).
- **Monthly**: when `date` is the 1st, roll up the just-finished month's
  `weekly` periods (a month spans 4-5 ISO weeks; the boundary weeks that
  straddle two months both count, matching how `2026-W37`-style periods
  already don't align to month boundaries).
- **Yearly**: when `date` is Jan 1, roll up the just-finished year's 12
  `monthly` periods.

No new schema. No new binary. `RunRollup`'s signature already takes
`sourcePeriods` as a caller-computed list (`runner.go:249`) — this is
exactly the "calendar logic belongs in the cron layer" the existing doc
comment in `cmd/hupi-consolidate/main.go` already anticipated.

### 4.2 Export / import

**Per-scope export** (`hupi-export -scope private:user:alice` or
`-scope shared:team:acme`) produces the HPMF v1 directory tree
`MEMORY_FORMAT.md` already specifies, packed into one `age`-encrypted
`.age` file — unchanged from the original design except the manifest gains
`scope_kind`/`scope_owner` (that document predates Tier 3 and assumed a
single owner).

**Whole-deployment export** (`hupi-export -all`) loops every scope from
`users`/`teams` (same enumeration `cmd/hupi-consolidate`'s
`loadActiveScopes` already does) and writes each into its own
subdirectory (`scopes/private-user_alice/...`,
`scopes/shared-team_acme/...`) under one outer manifest, then encrypts the
whole tree as one `.age` file — one artifact, one recipient key, every
scope inside it.

**Import** (`hupi-import <file> -scope <target>`):

- **Fresh scope** (target has zero episodes): load directly, as originally
  designed.
- **Merge** (target already has data): dedup episodes by their existing
  `hash` column (`episodes_hash_idx` already exists — an episode whose
  hash is already present in the target scope is skipped, not
  reinserted). Summaries: skip if a summary with that exact id already
  exists in the target scope (summary ids are already
  scope-namespaced and date/level-derived per `TIER3_PLAN.md` §4, so a
  collision means "already imported this period," not a real conflict).
  Entities: insert only if that id doesn't already exist in the target
  scope — merge does **not** attempt to merge entity attributes
  (that's what consolidation's `upsertEntities` does going forward; import
  isn't trying to replace live entity refinement with a stale snapshot).
- `index/` (embeddings) stays excluded from every export, unchanged —
  `hupi import` always rebuilds them locally via the target deployment's
  configured embedding provider, exactly as `MEMORY_FORMAT.md` already
  specifies.

New binaries: `cmd/hupi-export`, `cmd/hupi-import`. No schema changes.

### 4.3 Audit log

One new table, `audit_log`, is the single spine for every event type
below — not a UNION of episodes-as-writes plus a separate retrieval log
plus a separate admin log. Simpler report tool, one source of truth for
"what happened, in what order," across categories that would otherwise
need reconciling by timestamp.

```
audit_log (
  id            bigserial primary key,
  ts            timestamptz not null default now(),
  event_type    text not null,   -- capture | retrieve | trace | correct | admin_provision | admin_ui_view
  actor         text not null,   -- identity.UserID, or an operator name for CLI/admin-UI actions (below)
  acting_scope_kind  text, acting_scope_owner  text,
  workspace_scope_kind text, workspace_scope_owner text,
  target_ref    jsonb,           -- identity.Ref of whatever was touched, if applicable
  detail        jsonb not null default '{}'
)
```

Hook points:

- **Writes**: `internal/store.Capture` and `consolidation.storeSummary`
  log `capture`/`correct` after a successful write, inside the same
  scoped transaction.
- **Retrievals**: `internal/store.Retrieve` logs `retrieve` with both the
  acting-user and workspace scopes (the same split `dbscope` already
  carries) and the `retrieved_refs` that came back.
- **Investigation**: `hupi-trace` logs `trace`. CLI tools have no
  API-key identity today, so this (and `admin_provision` below) needs an
  actor from somewhere else — see the flag below.
- **Admin actions**: `hupi-admin` logs `admin_provision` for
  create-user/create-team/add-member/create-key.
- **Admin UI**: logs `admin_ui_view` for every user/team detail page
  view, and `admin_provision` for its create/revoke actions.

**Design decision I'm flagging, not deciding silently**: `hupi-admin-ui`
today authenticates with **one shared token** (`HUPI_ADMIN_UI_TOKEN`,
`ADMIN_UI.md`) — there is no per-operator identity to log. "Who viewed
this" is meaningless if every operator is indistinguishable. Making the
chosen audit level (which explicitly includes admin access) actually mean
something for the admin UI requires replacing the single shared token
with a small set of **named operator credentials** — a new `operators`
table (`name`, `token_hash`, `created_at`, `revoked_at`), same shape as
`api_keys` but for operators instead of end users. `HUPI_ADMIN_UI_TOKEN`
as an env var goes away; operators are provisioned once (e.g. via
`hupi-admin create-operator`) the same way users are today. CLI tools
(`hupi-trace`, `hupi-admin`) get a `-actor` flag, defaulting to `$USER`
from the environment if omitted — real accountability requires someone to
type their own name, but a default means this never blocks a script.

**RLS on `audit_log`**: `INSERT` is scope-checked (`with check`) against
the writing session's own scope variables, same pattern as every other
table — this catches an application bug that logs the wrong scope, the
same "second independent safety net" role RLS already plays elsewhere.
`SELECT` deliberately has **no** scope-restricting policy — `audit_log`'s
entire purpose is letting `hupi-audit` see across scopes, and restricting
reads would defeat that. This is a conscious asymmetry: anyone holding
the `hupi_app` credential can already choose which scope's session
variables to set (the same trust model `hupi-trace` already relies on to
investigate an arbitrary scope), so unrestricted `audit_log` reads don't
weaken an existing guarantee, they're consistent with it.

New binary: `hupi-audit` (subcommands: `tail`, `query -scope=... -since=... -event-type=...`).

### 4.4 Key rotation (online, resumable, batched)

Schema (see §3 for why this is unavoidable):

- `scope_keys` gains `version int not null default 1` and its primary key
  becomes `(scope_kind, scope_owner, version)` — multiple generations of a
  scope's key can coexist during a rotation.
- `episodes`, `summaries`, `entities` each gain `key_version int not null
  default 1`, recording which version of that row's scope's key encrypted
  its ciphertext columns.
- New `key_rotations` table tracks progress: `(scope_kind, scope_owner,
  from_version, to_version, cursor_table, cursor_id, status, started_at,
  completed_at)` — `status` is `in_progress | completed | failed`, and
  `cursor_table`/`cursor_id` is exactly enough to resume a batch job that
  got killed partway through `episodes` and hadn't started `summaries` yet.

`KeyStore` changes from "one Encryptor per scope" to "one Encryptor per
(scope, version)" — `GetOrCreate(ctx, scope)` becomes
`GetOrCreate(ctx, scope, version)`, and every call site in
`internal/store`, `internal/consolidation`, and `cmd/hupi-trace` starts
passing the row's own `key_version` instead of implicitly assuming there's
only one.

Rotation flow (`hupi-rotate-key -scope team:acme`):

1. Generate a new DEK, wrap it, insert as `scope_keys` version N+1.
   **All new writes to that scope switch to N+1 immediately** (`KeyStore`
   always encrypts with the highest version) — the "no downtime" part of
   this is really "old data stays on the old key until re-encrypted; new
   data never touches the old key again," not "nothing changes until the
   job finishes."
2. Insert a `key_rotations` row, `status=in_progress`.
3. Batch job walks `episodes`, then `summaries`, then `entities` for that
   scope, in primary-key order, N rows at a time (small transactions):
   decrypt each row's ciphertext columns with version N's Encryptor,
   re-encrypt with N+1's, update `key_version`, advance the cursor.
   Resumable: a re-run of `hupi-rotate-key` for a scope with an
   `in_progress` row picks up from `cursor_table`/`cursor_id` instead of
   restarting.
4. When all three tables are fully at N+1: mark the `key_rotations` row
   `completed`. Version N's `scope_keys` row is kept, not deleted —
   "completed" means no ciphertext references it anymore, but keeping it
   costs nothing and removes any risk of a missed row becoming
   permanently unreadable. A separate, explicit `-prune-old-versions` flag
   deletes fully-superseded old versions; not automatic.

## 5. Component change list

New migrations (numeric, applied in order after `0006`):

- `0007_audit_log.sql` — `audit_log` table, RLS (insert-checked,
  unrestricted select per §4.3), grants to `hupi_app`.
- `0008_admin_operators.sql` — `operators` table, drops the single-token
  admin-UI auth model.
- `0009_export_import_audit_events.sql` — extends `audit_log.event_type`
  with `export`/`import`.
- `0010_key_rotation.sql` — `key_version` columns, `scope_keys` PK
  change, `key_rotations` table.
- `0011_key_rotation_audit_event.sql` — extends `audit_log.event_type`
  with `key_rotate`.

New packages: `internal/audit` (log-writer helper used by `store`,
`consolidation`, CLI tools), `internal/hpmf` (directory-tree read/write +
`age` wrap/unwrap, shared by `hupi-export`/`hupi-import`), `internal/rotate`
(the batch-rotation runner, analogous to `internal/consolidation`).

New binaries: `hupi-export`, `hupi-import`, `hupi-audit`, `hupi-rotate-key`.

Changed: `cmd/hupi-consolidate` (rollup scheduling), `internal/store`
(audit hooks, `key_version`-aware encrypt/decrypt), `internal/consolidation`
(audit hooks, `key_version`-aware), `cmd/hupi-trace` (`-actor` flag, audit
hook, `key_version`-aware decrypt), `cmd/hupi-admin` (`-actor` flag, audit
hook, new `create-operator` subcommand), `cmd/hupi-admin-ui` (named-operator
auth replacing the shared token, audit hooks), `internal/crypto`
(version-aware `KeyStore`).

New deployment artifacts: `Dockerfile` (multi-stage, all binaries in one
image), `deploy/k8s/` (Deployment, Service, Ingress route with no TLS
config, ConfigMap for `providers.yaml`, Secret template, CronJobs for
`hupi-consolidate` and `hupi-selfcheck`), `deploy/helm/hupi/` (chart
wrapping the above with `values.yaml` for per-environment overrides).

## 6. Phased rollout

Ordered so each phase either stands alone or lays groundwork the next
phase reuses — not by §12's original list order.

1. **Rollup scheduling. DONE.** `cmd/hupi-consolidate/rollup.go`,
   `Runner.RunRollup`'s idempotency guard (`summaryExists`). Tested:
   `cmd/hupi-consolidate/rollup_test.go` (calendar logic, pure) +
   `internal/consolidation/runner_test.go`'s
   `TestRunRollup_IdempotentAcrossReruns` (real Postgres). Docs updated:
   `BUSINESS_PROCESS.md` §10/§12, `DESIGN_VS_BUILT.md` #4, `CODE_GUIDE.md`.
2. **Audit log. DONE.** New table `audit_log` (`schema/0007`, insert-only
   for `hupi_app` by design — see its comment) and `operators`
   (`schema/0008`, replacing the single shared `HUPI_ADMIN_UI_TOKEN`).
   New package `internal/audit` (`Write`/`LogStandalone`, used by
   `internal/store`'s `Capture`/`Retrieve`/`Trace`,
   `internal/consolidation`'s `storeSummary`, and every `hupi-admin`/
   `hupi-admin-ui` action). New binary `hupi-audit` (`tail`, `query`).
   `hupi-trace`/`hupi-correct`/`hupi-admin` gained an `-actor` flag
   (default `$USER`). Tested: `internal/store/audit_test.go` (capture,
   retrieve, trace — including that a team-scoped capture's `actor` is
   the individual member, not the team),
   `internal/consolidation/runner_test.go`'s
   `TestCorrect_WritesAuditLogWithGivenActor`,
   `cmd/hupi-admin-ui/handlers_test.go`'s `TestRequireOperatorAuth` and
   `TestAdminUIActions_AreAudited` — all real Postgres. Docs updated:
   `BUSINESS_PROCESS.md` §7/§12, `ADMIN_UI.md`, `INSTALL.md`,
   `CODE_GUIDE.md`, `API_REFERENCE.md`.
3. **Export / import. DONE.** New package `internal/hpmf`
   (`export.go`/`import.go`/`bundle.go`/`age.go`/`types.go`/`jsonl.go`/`util.go`),
   new binaries `hupi-export`/`hupi-import`, new dependency
   `filippo.io/age`, migration `0009` (extends `audit_log.event_type` with
   `export`/`import`). Per-scope or whole-deployment (`-all`) export;
   import either requires an empty target scope or, with `-merge`, dedups
   episodes by `hash`, entities by id, and summaries by (level, period) —
   re-inserting with freshly generated ids and remapped `supersedes`/
   `refers_to` rather than ever reusing a source scope's ids verbatim.
   Two real bugs found only by running this against Postgres, not by
   inspection: (1) nesting a query inside an open `Rows` cursor on the
   same transaction surfaced as "driver: bad connection" under pgx; (2)
   reusing an episode's exported id blindly silently no-op'd via
   `ON CONFLICT DO NOTHING` when that id already belonged to a different
   scope's row — fixed by checking `RowsAffected` and generating a fresh
   id on a genuine collision. Also fixed along the way: entity
   `attributes` were serializing as an escaped JSON *string* instead of a
   nested object, contradicting MEMORY_FORMAT.md's own "human-readable,
   diffable" principle. Tested: `internal/hpmf/hpmf_test.go` (round-trip
   with decrypted-content verification, merge dedup on a second import,
   fresh-mode rejection, whole-deployment scope isolation, age
   encrypt/decrypt round-trip including a wrong-identity failure case),
   all real Postgres, plus a full manual CLI smoke test (export → import
   into a different scope → whole-deployment restore into a fresh
   database). **Known gap, documented, not silently dropped**: nothing
   yet re-embeds imported historical data for vector search — see
   MEMORY_FORMAT.md's portability workflow note. Docs updated:
   `MEMORY_FORMAT.md` (directory layout, manifest.json, portability
   workflow — all three predated Tier 3 and needed real changes, not just
   a built/not-built note), `BUSINESS_PROCESS.md` §12, `INSTALL.md`,
   `CODE_GUIDE.md`.
4. **Key rotation. DONE.** `internal/crypto.KeyStore` became
   version-aware (`GetOrCreate` = current version for writes, `GetVersion`
   = a specific version for reads, `CurrentVersion`, `CreateNextVersion`,
   `Evict`) — this rippled through every encrypt/decrypt call site in
   `internal/store`, `internal/consolidation`, and `internal/hpmf`, plus
   `internal/bootstrap`'s legacy-DEK migration (its `ON CONFLICT` target
   needed updating for the new 3-column `scope_keys` PK — a raw-SQL change
   the compiler couldn't catch). New migrations `0010` (`scope_keys`
   versioning, `key_version` on `episodes`/`summaries`/`summary_key_facts`/
   `entities`, `key_rotations` progress table) and `0011` (audit event
   type). New package `internal/rotate` (`Start`/`Continue`/`Status`/`Prune`)
   and binary `hupi-rotate-key`. Two real bugs found only by running this
   against Postgres: (1) `Prune` deleted the database row but the
   `KeyStore`'s own in-memory cache kept serving the "pruned" key — fixed
   with `KeyStore.Evict`, called from `Prune`, documented as only covering
   the calling process (a live gateway with the same key cached needs its
   own restart); (2) none in the migration logic itself, but the design
   review while writing tests confirmed a subtlety worth stating plainly:
   a row already migrated to the new version structurally can't be
   reprocessed or double-counted, because it stops matching the "still on
   the old version" query the batch job issues — this is what makes
   resuming after a crash correct without the persisted cursor needing to
   be perfectly accurate, not just resumable in the loose sense. Tested:
   `internal/rotate/rotate_test.go` — full lifecycle with decrypted-content
   verification, resume after a simulated crash (a second `Runner`/`KeyStore`
   picks up correctly), a concurrent write during an in-progress rotation
   landing on the new version and never being touched by the batch job,
   prune refusing before completion and succeeding after, `Start` being
   idempotent while `in_progress` — all real Postgres, plus a full manual
   CLI smoke test (seed → rotate → trace → audit → prune). Docs updated:
   `BUSINESS_PROCESS.md` §12, `INSTALL.md`, `CODE_GUIDE.md`.
5. **Containerization. DONE.** `Dockerfile` (multi-stage: `golang:1.25-bookworm`
   builder producing all 11 static binaries — `hupi`, `hupi-consolidate`,
   `hupi-selfcheck`, `hupi-trace`, `hupi-correct`, `hupi-admin`,
   `hupi-admin-ui`, `hupi-audit`, `hupi-export`, `hupi-import`,
   `hupi-rotate-key` — with `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`;
   `alpine:3.20` runtime with `ca-certificates` + `postgresql-client`, a
   non-root `hupi` user, one image for every binary rather than eleven).
   New `schema/migrate.sh` — a POSIX-sh idempotent migration runner,
   deliberately a separate implementation from `install.sh`'s bash
   version (bare-metal vs. container/Kubernetes contexts — see its own
   doc comment), using the same per-migration "already applied" probe
   query pattern. New `/healthz` (liveness, no dependencies) and
   `/readyz` (readiness, pings the DB with a 2s timeout) endpoints on
   `cmd/hupi`. New `deploy/k8s/` — numbered plain manifests: ConfigMaps
   for `providers.yaml` and (example) `probes.yaml`, example Secrets for
   the app DSN/KEK/provider keys and the separate higher-privilege
   migration DSN, a migrate Job, the gateway Deployment/Service/Ingress
   (deliberately no TLS/certificate config — the existing ingress
   controller in front terminates TLS, per explicit direction), CronJobs
   for `hupi-consolidate` (nightly) and `hupi-selfcheck` (weekly), and an
   optional admin-UI Deployment/Service (ClusterIP only, no Ingress,
   since every action it exposes is destructive-adjacent). New
   `deploy/helm/hupi/` chart wrapping the same set with `values.yaml` for
   per-environment overrides — `secrets.app.create`/`secrets.adminDB.create`
   toggle between chart-managed and pre-existing Secrets, `migrate.enabled`
   wires the migration as a `pre-install,pre-upgrade` hook Job with
   `before-hook-creation` delete policy, `adminUI.enabled` and
   `selfcheck.enabled` gate their optional resources. Verified: `docker
   build` succeeds (184MB image); confirmed all 11 binaries + `psql`
   present; `schema/migrate.sh` run from inside the image against the
   real test Postgres, both fresh-apply and idempotent re-run; the
   gateway container smoke-tested (`/healthz`→200, `/readyz`→200,
   `/v1/chat/completions`→502 against a fake upstream, the correct
   signal); `hupi-admin-ui` run via `--entrypoint` override (401 without
   auth, as expected); `helm lint` clean, `helm template` rendered with
   every optional value combination and the output re-validated with
   `kubectl apply --dry-run=client` against the real API schema (no
   errors); the plain `deploy/k8s/*.yaml` manifests dry-run validated the
   same way. Also discovered and documented (not a bug, a design note):
   plain `docker run IMAGE ARG` appends `ARG` to a fixed `ENTRYPOINT`
   rather than replacing it, so manual testing of the non-default
   binaries needs `--entrypoint`; this doesn't affect the Kubernetes
   design, since a pod spec's `command:` fully replaces the entrypoint.
   Per explicit instruction, the image was tagged `sujithsamuel/hupi`
   (`:test` and `:latest`, matching the manifests/chart defaults) and
   **not pushed** — build-and-verify-locally only, left for the user to
   push themselves once reviewed. Docs updated: `INSTALL.md`,
   `CODE_GUIDE.md`, this file.

   **Update**: `sujithsamuel/hupi:latest` is now actually pushed and
   public (`linux/amd64` only for now — a multi-arch `linux/arm64` build
   was attempted first but was slow enough under QEMU emulation to not
   be worth the wait; amd64-only shipped instead, arm64 remains
   available on request). Verified with a real `docker rmi` +
   fresh-`docker pull` + `docker run` round trip, not just a successful
   push. `docker-compose.yml` now defaults to `image:
   sujithsamuel/hupi:latest` rather than `build: .`.

Each phase ships with its own real-Postgres integration tests
(`HUPI_TEST_DATABASE_URL`, `-count=1`) before moving to the next, matching
this project's existing testing discipline — see §7.

## 7. Testing plan

- **Rollup**: seed daily summaries spanning a fake "just finished" week/
  month/year, run the scheduler logic for that date, assert the right
  rollup appears exactly once (including on a simulated double-fire).
- **Export/import**: round-trip a scope through export → import into a
  fresh scope, assert byte-for-byte record equivalence (minus embeddings,
  by design); round-trip merge-import into a scope with overlapping and
  non-overlapping data, assert no duplicates and no data loss; whole-
  deployment export/import across multiple scopes, assert scope
  boundaries survive the round trip (a private scope's data never lands
  in another scope on import).
- **Audit log**: one test per hook point asserting the expected row
  appears with the right `event_type`/`actor`/scopes; an RLS test
  mirroring `internal/store/rls_test.go`'s pattern, confirming the insert
  check rejects a mismatched scope while select remains unrestricted.
- **Key rotation**: rotate a scope with existing data, assert every row
  is readable throughout (including mid-rotation, simulating a read that
  lands between two committed batches); kill the job mid-batch and
  restart, assert it resumes rather than restarting or corrupting
  already-migrated rows; concurrent-write test — write new data *during*
  an in-progress rotation, assert it lands on the new version and is
  never touched by the batch job.
- **Containerization**: build the image, run it against the existing
  `hupi-pg-test` container via plain `docker run` (no `kind`/`minikube`
  available in this environment), exercising the same golden-path checks
  `install.sh` and `INSTALL.md` already use (`/healthz`, `/readyz`, a
  chat-completions round trip, `schema/migrate.sh` fresh-apply and
  idempotent re-run) — plus `helm lint`/`helm template` and
  `kubectl apply --dry-run=client` for schema-level validation of both
  the Helm chart and the plain manifests, since an actual cluster wasn't
  available to apply them against for real.

## 8. Risks

- **Key rotation was the one item here with a real correctness bar** — a
  bug means either unreadable data (wrong key selected) or a scope's DEK
  silently never actually rotating. Phased fourth and tested most
  heavily as planned (§4.4); old key versions are kept, not deleted, by
  default, exactly as designed. One residual, permanent limitation
  rather than a bug: `Prune` can only evict a key from the process that
  ran it, never from another already-running process's memory — see
  `internal/crypto.KeyStore.Evict`'s doc comment and `hupi-rotate-key`'s
  own `-prune-old-versions` output.
- ~~The admin-UI auth change (§4.3) is additional scope beyond the literal
  ask~~ **Resolved** — confirmed and implemented as part of phase 2.
- **Whole-deployment export** is the first tool in this codebase that
  reads across every scope in one run by design, rather than one scope at
  a time. It needs the same operator-trust framing `hupi-admin` already
  has (§2's non-goals), stated explicitly so it doesn't get mistaken for
  a new authorization boundary.
- **Containerization manifests/chart were validated by schema, not by a
  live cluster** — no `kind`/`minikube` was available in this
  environment. `kubectl apply --dry-run=client` and `helm template` +
  `helm lint` catch structural errors (bad fields, broken templating,
  invalid YAML) but not things only a running cluster would surface
  (RBAC gaps, a CNI/ingress controller quirk, actual probe timing under
  real load). Treat the first real-cluster deploy as the actual first
  test of that layer, same caution `INSTALL.md` already gives bare-metal
  installs.
