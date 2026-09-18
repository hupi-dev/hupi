# Code Guide — Project Structure and Module Trigger Reference

This is a structural map of the actual code: what lives where, which
package depends on which, and — for every entry point, HTTP or otherwise
— the exact sequence of functions and files a request passes through.

It complements, and deliberately doesn't repeat, the design narrative in
[HOW_IT_WORKS.md](HOW_IT_WORKS.md) (the *why*) and
[ARCHITECTURE.md](../ARCHITECTURE.md) (the design intent). This document is
the *where or does the code actually go* reference — every function and
file name here is verified against the code as it exists today, not
against an earlier design.

For the HTTP API specifically, see the companion document
[API_REFERENCE.md](API_REFERENCE.md), which walks all four routes
request-to-response in full call-chain detail.

## 1. Project layout

```
cmd/
  hupi/                  gateway server — long-lived, serves the HTTP API
  hupi-consolidate/      cron job — nightly consolidation + calendar-boundary rollups, once per active scope
  hupi-trace/            CLI — decrypt and print one episode's retrieval trace
  hupi-correct/          CLI — write a superseding, corrected summary version
  hupi-selfcheck/        cron job — run memory-probe regression checks
  hupi-admin/            CLI — provision users/teams/memberships/API keys/operators
  hupi-admin-ui/         web server — same provisioning, browser UI, see ADMIN_UI.md
    web/                 the React frontend (Vite/TypeScript/Tailwind), embedded via assets.go's //go:embed web/dist
  hupi-audit/            CLI — query audit_log (tail, filtered query)
  hupi-export/           CLI — write an age-encrypted HPMF snapshot, one scope or -all
  hupi-import/           CLI — load an HPMF snapshot back into Postgres, fresh or -merge
  hupi-rotate-key/       CLI — online, resumable per-scope key rotation (start/continue/status/prune)

internal/
  identity/              Scope, Identity, Ref — pure data types, zero dependencies
  pgfmt/                 Postgres array/vector literal formatting helpers
  crypto/                field encryption (Encryptor) + per-scope key management (KeyStore)
  dbscope/                RLS-aware scoped transaction helper (Querier, Run, SetSession)
  audit/                  single audit_log writer (Write, LogStandalone) used by store/consolidation/CLI tools
  provider/               LLM vendor abstraction: Provider interface, OpenAICompat, Anthropic, Registry
  auth/                   scope provisioning + operator credentials (this repo); real API-key resolution is the Tier 3 extension — see §6
  gateway/                HTTP surface: Handler, request/response types, Retriever/Capturer/Authenticator interfaces
  store/                  Postgres+pgvector implementation of Capturer/Retriever/Trace
  consolidation/          the nightly rollup engine (Runner)
  hpmf/                   HPMF read/write (export/import) + age packaging — MEMORY_FORMAT.md's implementation
  rotate/                 online, resumable per-scope key rotation (Start, Continue, Status, Prune)
  selfcheck/              probe runner used by cmd/hupi-selfcheck
  bootstrap/              startup wiring: config, DB connection, key store

schema/                   numbered SQL migrations, applied in order 0001 -> 0011
                          migrate.sh — POSIX-sh idempotent runner used by the Docker image/Kubernetes migrate Job (install.sh's bash equivalent is for bare-metal; kept as two separate implementations on purpose, see migrate.sh's own comment)
docs/                      this file, and everything else under docs/

Dockerfile                 one image, all 11 binaries — gateway is the default ENTRYPOINT, everything else runs via a `command:` override
deploy/k8s/                 plain Kubernetes manifests, numbered in apply order
deploy/helm/hupi/           the same resources as a Helm chart, values.yaml-parameterized
vscode-extension/           VS Code extension (chat sidebar + inline edit) — see VSCODE_EXTENSION.md
```

## 2. Package dependency graph

Arrows read "depends on." This is a strict layering — nothing here is
circular, and the leaf packages (`identity`, `pgfmt`) have zero internal
dependencies on purpose, since they're the vocabulary every other package
shares.

```
identity   (no deps)
pgfmt      (no deps)
provider   (no internal deps — only stdlib + net/http)

crypto     -> identity
dbscope    -> identity
audit      -> identity, dbscope
gateway    -> identity, provider

auth       -> identity, crypto
store      -> identity, provider, crypto, dbscope, pgfmt, gateway, audit
consolidation -> identity, provider, crypto, dbscope, pgfmt, audit
hpmf       -> identity, crypto, dbscope, pgfmt, audit   (+ filippo.io/age)
rotate     -> identity, crypto, dbscope, audit

selfcheck  -> gateway, provider

bootstrap  -> identity, crypto, provider   (+ blank-imports the pgx driver)

cmd/hupi              -> bootstrap, gateway, store, auth
cmd/hupi-consolidate  -> bootstrap, consolidation, identity
cmd/hupi-trace        -> bootstrap, store, identity
cmd/hupi-correct      -> bootstrap, consolidation, identity
cmd/hupi-selfcheck    -> bootstrap, store, selfcheck
cmd/hupi-admin        -> bootstrap, auth, audit, identity
cmd/hupi-admin-ui     -> bootstrap, auth, audit, identity
cmd/hupi-audit        -> bootstrap
cmd/hupi-export       -> bootstrap, hpmf, auth, identity   (+ filippo.io/age)
cmd/hupi-import       -> bootstrap, hpmf, auth, identity   (+ filippo.io/age)
cmd/hupi-rotate-key   -> bootstrap, rotate, identity
```

Note `store` depends on `gateway` (for the `gateway.Episode`,
`gateway.RetrievalResult`, `gateway.Capturer`/`Retriever` types it
implements) — but `gateway` does not depend on `store`. The interfaces are
defined by the consumer (`gateway`), implemented by the provider
(`store`), which is what lets `cmd/hupi` wire a concrete `*store.Store`
into a `gateway.Handler` typed against the interfaces, without `gateway`
ever needing to know Postgres exists.

## 3. Module responsibilities, at a glance

| Package | Responsibility | Key exported names |
|---|---|---|
| `identity` | The scope/identity vocabulary every other package shares | `Scope`, `Identity`, `Ref`, `DefaultScope`, `ScopeKindPrivate`/`ScopeKindShared` |
| `pgfmt` | Format Go values as Postgres literals, driver-agnostically | `TextArray`, `ParseTextArray`, `VectorLiteral`, `Nullable` |
| `crypto` | AES-256-GCM field encryption; one or more keys per scope (versioned for rotation) | `Encryptor`, `KeyStore` (`GetOrCreate` = current version, `GetVersion` = a specific one, `CurrentVersion`, `CreateNextVersion`, `Evict`), `GenerateDEK`, `WrapDEK`/`UnwrapDEK` |
| `dbscope` | Open a transaction with RLS session variables set correctly | `Querier`, `SetSession`, `Run` |
| `audit` | Single writer for every `audit_log` row, any event type | `Entry`, `Write`, `LogStandalone`, `Event*` constants |
| `provider` | One interface per vendor wire format, not per vendor | `Provider`, `OpenAICompat`, `Anthropic`, `Registry` |
| `auth` | Scope provisioning + operator credentials (this repo); real end-user/team auth is a separate, commercially-licensed extension — see §6 | `Store` (`CreateUser`, `CreateTeam`, `ListUsers`, `ListTeams`, `CreateOperator`, `ResolveOperator`, `ListOperators`, `RevokeOperator`); `TeamAuthenticator` interface + `NewTeamAuthenticator` hook (nil unless the Tier 3 extension is present) |
| `gateway` | The HTTP surface + the interfaces storage must implement | `Handler`, `Retriever`, `Capturer`, `Authenticator`, `Episode`, `RetrievalResult` |
| `store` | Postgres+pgvector implementation of retrieval/capture/trace | `Store` (`Retrieve`, `Capture`, `Trace`) |
| `consolidation` | Nightly rollup: episodes -> grounded summaries | `Runner` (`RunDaily`, `RunRollup`, `Correct`) |
| `hpmf` | Portable memory format read/write (MEMORY_FORMAT.md) + age packaging | `ExportScope`, `ExportBundle`, `ImportScope`, `PackAndEncrypt`, `DecryptAndUnpack`, `Manifest` |
| `rotate` | Online, resumable per-scope key rotation | `Runner` (`Start`, `Continue`, `Status`, `Prune`) |
| `selfcheck` | Run probes against a live `Retriever` | `Probe`, `Result`, `Run` |
| `bootstrap` | Read config, connect Postgres, build the `KeyStore` | `Deps`, `Load` |

## 4. Cross-cutting concerns (these three show up in nearly every call chain)

### 4.1 Scope resolution

Every operation in `store` and `consolidation` takes one or two
`identity.Scope` values — never infers them. There are exactly two ways a
scope gets decided:

- **HTTP requests**: `gateway.Handler.resolveScope` (private routes) or
  `resolveTeamScope` (team routes) — see [API_REFERENCE.md](API_REFERENCE.md)
  for the exact logic.
- **Cron/CLI**: the caller passes a scope explicitly — `cmd/hupi-consolidate`
  enumerates every user/team via `loadActiveScopes` and loops;
  `cmd/hupi-trace`/`cmd/hupi-correct` take `-scope-kind`/`-scope-owner` flags.

Almost everything that reads or writes data takes **two** scope
parameters, `actingUser` and `workspace`, not one — see
[TIER3_PLAN.md D3](TIER3_PLAN.md) for why: `self_model` always anchors to
`actingUser` (personal voice never changes based on which workspace you're
in), while everything actually searched or written is scoped to
`workspace`. For a private request the two are equal; only a team-routed
request has them differ.

### 4.2 Encryption key resolution

No code holds a single, package-wide encryption key. Every encrypt/decrypt
call site resolves its key fresh via `keys.GetOrCreate(ctx, scope)`
(`internal/crypto/keystore.go`) — cached in memory after the first
resolution per scope, but always looked up by the specific scope that
record belongs to. `store.Store` and `consolidation.Runner` each hold a
`*crypto.KeyStore` (not a `*crypto.Encryptor`) for exactly this reason —
see [HARDENING_PLAN.md](HARDENING_PLAN.md) D5-D7.

### 4.3 RLS session variables

Every database read or write that touches `episodes`, `summaries`, or
`entities` runs inside a transaction opened by `dbscope.Run` (or, for
`consolidation.storeSummary`'s multi-statement write, a transaction that
calls `dbscope.SetSession` directly). That call sets four Postgres session
variables (`hupi.acting_scope_kind`/`owner`,
`hupi.workspace_scope_kind`/`owner`) that the RLS policies in
`schema/0005_hardening_phase3_rls.sql` check on every row. Code that
bypasses `dbscope` and queries `*sql.DB` directly will see **zero rows**
under RLS, not an error — this is deliberate (fail closed), and is
exactly what `internal/store/rls_test.go` tests for.

## 5. Non-HTTP entry points, call chain by call chain

The HTTP routes are covered in full in [API_REFERENCE.md](API_REFERENCE.md).
The five other binaries, below.

### `cmd/hupi-consolidate` — nightly rollup

1. `bootstrap.Load(ctx)` — config, DB, `KeyStore`.
2. `loadActiveScopes(ctx, deps.DB)` (`cmd/hupi-consolidate/main.go`) — `select id from users` + `select id from teams`, returns one `identity.Scope` per row.
3. For each scope, `runner.RunDaily(ctx, scope, date)` (`internal/consolidation/runner.go`):
   1. `dbscope.Run` -> `loadDailyEpisodes` -> `scanEpisodeSources` — loads and decrypts the day's `type='interaction'` episodes for that scope.
   2. If zero episodes: return, no-op.
   3. `generateSummary(ctx, scope, "daily", period, sources)` — picks `summarySystemPrompt` or `teamSummarySystemPrompt` (`prompts.go`) based on `scope.Kind`, calls `r.consolidation.ChatCompletion` (external LLM call, no open transaction), parses the response with `extractJSON`.
   4. `storeSummary(ctx, in)` (`consolidation/store.go`):
      - `groundingCheck(ctx, ...)` (`grounding.go`) — a *second*, independent LLM call via `r.grounding.ChatCompletion`.
      - Resolve the scope's encryptor, encrypt the summary text.
      - One `dbscope.Run` transaction: `nextSummaryID` (versioned id lookup), `insert into summaries`, one `insert into summary_key_facts` per fact, `upsertEntities` (decrypt-merge-encrypt-upsert each touched entity).
      - After commit: `embedSummary` — embeds the summary text (external call) and writes `summaries.embedding` in its own short transaction.
   5. `embedHighImportanceEpisodes(ctx, scope, date)` — selects episodes above the importance threshold with no embedding yet; for each, embeds (external call) and writes back in its own short transaction — never one transaction spanning the whole loop, since each iteration makes a network call.
4. Failures per scope are logged and counted, not fatal to the batch — one team's bad day doesn't block everyone else's.
5. `runDueRollups(ctx, runner, date, scopes)` (`cmd/hupi-consolidate/rollup.go`): `dueRollups(date)` checks calendar boundaries (Monday/1st/Jan 1 → weekly/monthly/yearly) and returns zero or more `rollupJob`s; for each job, for each scope, `runner.RunRollup(...)`, same per-scope failure isolation as step 3-4. `RunRollup` itself checks `summaryExists` first and no-ops if that level+period is already there, so a repeated or missed cron run is harmless.

### `cmd/hupi-trace` — retrieval audit

1. `bootstrap.Load(ctx)`.
2. `store.Trace(ctx, scope, episodeID, actor)` (`internal/store/trace.go`) — `actor` is the `-actor` flag (default `$USER`):
   1. `dbscope.Run(scope, scope)` loads and decrypts the episode row (`ts`, `memory_gate`, `input_text`, `output_text`, `retrieved_refs`) and, in the same transaction, `audit.Write` a `trace` event (`event_type`, `actor`, `target_ref` = the episode) — investigating someone's memory content is exactly what the chosen audit level exists to record.
   2. Unmarshal `retrieved_refs` (jsonb) into `[]identity.Ref`.
   3. For each ref, dispatch by `ref.Kind` to `loadTraceSummary` / `loadTraceEntity` / `loadTraceEpisodeHit` — each opens its **own** `dbscope.Run(ref.Scope, ref.Scope)`, since a single episode's refs can span two different scopes (a self_model ref from the acting user's private scope alongside workspace-scoped summary/entity refs).
3. `printTrace` formats the result to stdout.

### `cmd/hupi-correct` — write a correction

1. `bootstrap.Load(ctx)`, read the corrected content JSON file into a `consolidation.ConsolidationOutput`.
2. `runner.Correct(ctx, scope, oldSummaryID, output, reason, actor)` (`consolidation/runner.go`) — `actor` is the `-actor` flag (default `$USER`), since a correction is always a deliberate human action, never `systemActor`:
   1. Load the old summary's `level`/`period`/`source_episode_ids`/`source_summary_periods`, scoped.
   2. Reload the original source material (`loadEpisodesByID` for a daily summary, `loadSummaries` at `sourceLevelBelow(level)` for a rollup) so the grounding check runs against real source text, not the (possibly wrong) old summary.
   3. `storeSummary` — same path as consolidation's own writes, but with `supersedes`/`correctionReason` set, so the schema records this as a new version, never an edit; the `audit_log` entry it writes gets `event_type = 'correct'` instead of `'capture'`.

### `cmd/hupi-audit` — query the audit trail

Two subcommands, both plain `*sql.DB` queries against `audit_log`
(no `dbscope.Run` needed — its `SELECT` policy is unconditionally
permissive, schema/0007_audit_log.sql):

| Subcommand | Behavior |
|---|---|
| `tail -n N` | `order by ts desc limit N`, then prints oldest-first |
| `query -scope-kind -scope-owner -actor -event-type -since -until -limit` | Builds a parameterized `WHERE` clause from whichever flags are set, `order by ts asc` |

### `cmd/hupi-export` — write a portable snapshot

1. `bootstrap.Load(ctx)`; resolve age recipients (`-recipient` or `-passphrase`, `internal/hpmf/age.go`).
2. Resolve the scope list: one scope (`-scope-kind`/`-scope-owner`) or every scope via `auth.Store.ListUsers`/`ListTeams` (`-all`).
3. `hpmf.ExportBundle` (`internal/hpmf/bundle.go`) — for each scope, `hpmf.ExportScope` (`internal/hpmf/export.go`): queries `episodes`/`summaries`+`summary_key_facts`/`entities` scoped and decrypted, writes `episodes/YYYY/MM/YYYY-MM-DD.jsonl`, `summaries/<level>/<period>.json` (array of every version, correction history included), `entities.jsonl`; writes one `export` `audit_log` entry per scope. Writes `manifest.json` once all scopes are done.
4. `hpmf.PackAndEncrypt` (`internal/hpmf/age.go`) — tars+gzips the temp directory, age-encrypts it to the `-out` path; the temp directory is removed either way.

### `cmd/hupi-import` — load a portable snapshot back in

1. `bootstrap.Load(ctx)`; resolve age identities (`-identity` file or `-passphrase`).
2. `hpmf.DecryptAndUnpack` + `hpmf.ReadManifest` (checks `hpmf_version`'s major version matches).
3. Per scope (one, or every scope in the manifest for `-all` — provisioning the user/team first via `auth.Store.CreateUser`/`CreateTeam` if it doesn't exist): `hpmf.ImportScope` (`internal/hpmf/import.go`) — without `-merge`, requires the target scope to be empty first; entities skipped if the id already exists, episodes deduped by `hash` (regenerating the id on an unrelated global-id collision, remapping any `refers_to` that pointed at the old one), summaries skipped per (level, period) if any version already exists, else re-inserted with freshly generated ids and `supersedes` remapped within that period's version chain. Writes one `import` `audit_log` entry per scope.

### `cmd/hupi-rotate-key` — online, resumable key rotation

Default action (no `-status`/`-prune-old-versions`) runs a rotation to completion in one invocation, safe to interrupt and re-run:

1. `bootstrap.Load(ctx)`.
2. `rotate.Runner.Start(ctx, scope, actor)` (`internal/rotate/rotate.go`) — `keys.CurrentVersion` + `keys.CreateNextVersion` mint the new DEK; upserts one row in `key_rotations` (idempotent: returns the existing from/to unchanged if a rotation is already `in_progress`); writes a `key_rotation` `audit_log` entry (`action: start`). From this point, every *new* write anywhere in the app resolves the new version via `KeyStore.GetOrCreate` — nothing in this package has to chase writes that happen during the rotation.
3. Loop `rotate.Runner.Continue(ctx, scope, batchSize, actor)` until `done`: each call migrates up to `batchSize` rows of whichever table `cursor_table` points at (`episodes` -> `summaries` -> `entities`, migrating each `summaries` row's `summary_key_facts` alongside it), decrypting with `keys.GetVersion(fromVersion)` and re-encrypting with `keys.GetVersion(toVersion)` inside one transaction per batch; a row with a `NULL` encrypted column (`episodes.note` on a non-feedback row) is left `NULL`, not turned into an encrypted `""`. Advancing past the last table calls `markCompleted` (`status='completed'`, another `audit_log` entry).
4. `-status`: prints the `key_rotations` row for the scope, no writes.
5. `-prune-old-versions`: `rotate.Runner.Prune` — refuses unless `status='completed'`, double-checks no row anywhere in the scope still references a version it's about to delete (not just trusting the status flag), deletes those `scope_keys` rows, and calls `KeyStore.Evict` so this process's own cache stops serving the pruned version immediately (a separately-running process, e.g. the live gateway, keeps its cached copy until it restarts — the CLI prints this caveat).

### `cmd/hupi-selfcheck` — retrieval regression check

1. `bootstrap.Load(ctx)`, load `probes.yaml` into `[]selfcheck.Probe`.
2. `selfcheck.Run(ctx, retriever, probes)` (`internal/selfcheck/selfcheck.go`) — for each probe, `retriever.Retrieve(ctx, scope, scope, messages)` (same scope for both parameters — a probe has no "acting user" concept) against the live `store.Store`, then checks the returned `Gate` and `ContextMessage` against the probe's `ExpectMinGate`/`ExpectSubstring`.
3. Exits non-zero if any probe fails — designed for cron + alerting.

### `cmd/hupi-admin` — provisioning

`create-user`/`create-team`/`create-operator`/`revoke-operator` are a
thin, direct call into `internal/auth.Store` (this repo — every tier).
`add-member`/`create-key` go through the `runAddMember`/`runCreateKey`
hooks instead (nil unless the Tier 3 extension is present — §6); the
switch checks for nil first and returns a clear "requires the HUPI
Enterprise build" error rather than a panic or a confusing failure
further down. Every subcommand that does real work is followed by
`logAdminAction` — `audit.LogStandalone` with `event_type =
'admin_provision'`, `actor` = the `-actor` flag (default `$USER`),
best-effort (a failed audit write logs to stderr, doesn't fail the
command whose real work already succeeded):

| Subcommand | Calls | Requires Tier 3 extension? |
|---|---|---|
| `create-user` | `auth.Store.CreateUser` — inserts into `users`, then eagerly `keys.GetOrCreate` for that user's private scope (provisions the DEK immediately, not lazily) | No |
| `create-team` | `auth.Store.CreateTeam` — same pattern for a team's shared scope | No |
| `add-member` | `runAddMember` hook -> `TeamAuthenticator.AddTeamMember` — upserts `team_members` | Yes |
| `create-key` | `runCreateKey` hook -> `TeamAuthenticator.CreateAPIKey` — generates a raw key, persists only its sha256 hash, prints the raw value once | Yes |
| `create-operator` | `auth.Store.CreateOperator` — generates an admin-UI operator credential, persists only its sha256 hash (schema/0008_admin_operators.sql) | No |
| `revoke-operator` | `auth.Store.RevokeOperator` | No |

### `cmd/hupi-admin-ui` — provisioning, over HTTP

`internal/auth.Store` underneath for the user/operator routes (this repo
— every tier), plus `internal/audit.Query` for the audit-log route.
Every team/API-key route (`/api/teams...`, `/api/users/{id}/keys`,
`/api/keys/revoke`) only exists if the Tier 3 extension registered them
via the `mountTeamRoutes` hook (§6) — absent that, `routes()` simply
never mounts them, a 404 rather than an error. The one route that spans
both — `GET /api/users/{id}` — nil-checks `server.teamStore` and returns
empty `teams`/`keys` when the extension isn't present, which is the
correct Tier 1/2 answer (no team memberships or API keys exist there
either way), not a degraded one. Routing and JSON marshaling only —
`cmd/hupi-admin-ui/handlers.go` has no business logic of its own, every
handler calls straight into `auth.Store` (or `audit.Query`) and writes a
JSON response; there is no `html/template` anywhere in this binary
anymore. Anything outside `/api/` is served by
`cmd/hupi-admin-ui/assets.go`, which embeds the React frontend
(`cmd/hupi-admin-ui/web`, a separate Vite/TypeScript/npm project — see
`web/README.md`) via `//go:embed web/dist` and serves it with an
SPA-fallback `http.FileServer` (unknown paths fall back to `index.html`
so React Router's client routes survive a hard refresh). `web/dist` is
npm build output, not source-controlled beyond a committed placeholder
`index.html` (so `go build` never fails on a fresh clone before the npm
build has run) — `install.sh`/the Dockerfile build it automatically. Every
handler that mutates state, plus the two "bundled
detail" GETs and the audit-log GET, also calls
`s.audit(r, eventType, scope, detail)` — `admin_ui_view` for a view,
`admin_provision` for a provisioning action — with `actor` pulled from the
request context (`operatorFromContext`), set by `requireOperatorAuth`
after resolving the Basic Auth password against
`auth.Store.ResolveOperator`. See [ADMIN_UI.md](ADMIN_UI.md) for the full
route list, the auth/exposure model, why named operators replaced a
single shared token, and why this exists at all despite
`TIER3_PLAN.md`'s original CLI-only non-goal.

## 6. Tier 3: the open-core build split

Tier 3 (real end-user/team authentication, `/v1/team/...` routes, team
CLI subcommands) lives in a separate, commercially-licensed repo
(`hupi-t3`), not this one — see [ARCHITECTURE.md § Licensing and the
open-core split](../ARCHITECTURE.md) for the full reasoning on what
stayed here vs. what moved. This section is the mechanical how: two
things make it work without build tags or a Go module dependency
between the repos.

**1. Physical file overlay, not an import.** Go only allows a package's
`internal/` directory to be imported by code rooted at that directory's
parent — a rule enforced by looking at the file tree on disk at compile
time, not at module or repo boundaries. `hupi-t3`'s files
(`internal/auth/team.go`, `internal/auth/oidc.go`,
`internal/gateway/team.go`, `internal/consolidation/team.go`,
`cmd/hupi-admin/team.go`, `cmd/hupi-admin-ui/team_handlers.go`) mirror
this repo's paths exactly;
its own `build.sh` clones this repo into a temp directory, copies its
files onto those same paths, and runs `go build` from the combined tree.
At that point they're indistinguishable from any other file in
`internal/auth`/`internal/gateway`/etc. — free to import
`internal/crypto`, `internal/identity`, anything else in the tree,
exactly as if they'd always lived there. A plain `go build ./...` in
this repo alone never sees those files at all.

**2. Nil-by-default hook variables, set via `init()`.** Even with the
import problem solved, this repo's own shared code (`cmd/hupi/main.go`,
`cmd/hupi-admin/main.go`, `cmd/hupi-admin-ui/handlers.go`) still needs to
*call* the Tier 3 code when it's present — but a direct reference like
`handler.HandleTeamChatCompletions` would fail to compile the moment
`team.go` is absent. The fix: this repo declares package-level variables
of function type, nil by default:

```go
// internal/gateway/handler.go (this repo)
var MountTeamRoutes func(mux *http.ServeMux, h *Handler)
```

Callers check for `nil` before using it:

```go
// cmd/hupi/main.go (this repo)
if gateway.MountTeamRoutes != nil {
    gateway.MountTeamRoutes(mux, handler)
}
```

In a plain build, nothing ever sets `MountTeamRoutes`, so it stays `nil`
forever, the `if` never fires, and `/v1/team/...` is simply never
registered — not an error, the correct Tier 1/2 state. When
`hupi-t3`'s `internal/gateway/team.go` is part of the build, its `init()`
(which Go runs automatically for every compiled package, before `main`)
assigns a real implementation:

```go
// internal/gateway/team.go (hupi-t3, overlaid at build time)
func init() {
    MountTeamRoutes = func(mux *http.ServeMux, h *Handler) {
        mux.HandleFunc("POST /v1/team/{team_id}/chat/completions", h.HandleTeamChatCompletions)
        mux.HandleFunc("POST /v1/team/{team_id}/feedback", h.HandleTeamFeedback)
    }
}
```

Same source on both sides of the split, no build tags — the only thing
that differs between the two builds is whether that one variable
happens to be `nil`, which depends entirely on whether the file was
present at compile time.

The full set of hooks, all following this pattern:

| Hook (declared here) | Set by (in `hupi-t3`) | Nil behavior |
|---|---|---|
| `gateway.MountTeamRoutes` | `internal/gateway/team.go` | `/v1/team/...` never mounted |
| `auth.NewTeamAuthenticator` | `internal/auth/team.go` (dispatches to `internal/auth/oidc.go` too, when OIDC env vars are set — see [OIDC.md](OIDC.md)) | `HUPI_REQUIRE_AUTH=true` fails at startup with a clear error (`cmd/hupi/main.go`'s `resolveAuth`) instead of silently granting no-auth access |
| `consolidation`'s `teamPromptProvider` | `internal/consolidation/team.go` | Every summary uses `summarySystemPrompt`, even for a `shared` scope — harmless, since Tier 1/2 never produces one |
| `cmd/hupi-admin`'s `runAddMember`/`runCreateKey` | `cmd/hupi-admin/team.go` | Those two subcommands return "requires the HUPI Enterprise build" instead of running |
| `cmd/hupi-admin-ui`'s `mountTeamRoutes` | `cmd/hupi-admin-ui/team_handlers.go` | `/api/teams...`, `/api/users/{id}/keys`, `/api/keys/revoke` never mounted; `server.teamStore` stays `nil`, and the one shared route that reads it (`GET /api/users/{id}`) returns empty `teams`/`keys` rather than erroring |

## 7. Where to look for what

| If you're asking... | Look at |
|---|---|
| "What happens when a chat request comes in?" | [API_REFERENCE.md](API_REFERENCE.md) |
| "Why does retrieval behave this way?" | [ARCHITECTURE.md § Retrieval Engine](../ARCHITECTURE.md), [HOW_IT_WORKS.md §4](HOW_IT_WORKS.md) |
| "What's the record schema (episode/summary/entity)?" | [MEMORY_FORMAT.md](MEMORY_FORMAT.md) |
| "Is X actually implemented, or just designed?" | [DESIGN_VS_BUILT.md](DESIGN_VS_BUILT.md) |
| "How does the multi-tenant/team model work?" | [TIER3_PLAN.md](TIER3_PLAN.md) |
| "Why isn't Tier 3's code in this repo, and how does the build still work?" | §6 above, [ARCHITECTURE.md § Licensing and the open-core split](../ARCHITECTURE.md) |
| "How does encryption/RLS actually work?" | [HARDENING_PLAN.md](HARDENING_PLAN.md), §4 above |
| "What does this product do, for whom?" | [BUSINESS_PROCESS.md](BUSINESS_PROCESS.md) |
| "How do I deploy this to Kubernetes?" | [INSTALL.md § Containerized deployment](INSTALL.md#containerized-deployment), [Dockerfile](../Dockerfile), [deploy/k8s/](../deploy/k8s/), [deploy/helm/hupi/](../deploy/helm/hupi/) |
| "How do I use HUPI from inside VS Code?" | [VSCODE_EXTENSION.md](VSCODE_EXTENSION.md), [vscode-extension/](../vscode-extension/) |
