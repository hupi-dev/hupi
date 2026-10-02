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
  hupi-demo/             gateway server — long-lived, the public hosted anonymous demo (guest sessions, its own listen address, capped usage so a bug here can't affect a real deployment)
  hupi-demo-sweep/       cron job — delete expired demo guest sessions and their data
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
  hupi-reembed/          CLI — online, resumable per-scope re-embedding after an embedding-provider change
  hupi-bench/            CLI — LoCoMo/LongMemEval benchmark harness: replays a benchmark conversation through the real gateway + real consolidation, writes predictions in each benchmark's own scoring-input shape (see bench/ below, docs/BENCHMARKS.md)
  hupi-ingest-turns/     CLI — EvalMem adapter tool: replay a flat turn list into a fresh scope, then consolidate (see docs/EVALMEM_INTEGRATION_PLAN.md)
  hupi-export-memory/    CLI — EvalMem adapter tool: dump a scope's entire decrypted memory (every entity/summary+key-facts/relationship) for external diagnostics
  hupi-answer-question/  CLI — EvalMem adapter tool: answer one question against an already-ingested scope, in native (real retrieval) or oracle (hand-fed context) mode

internal/
  identity/              Scope, Identity, Ref — pure data types, zero dependencies
  pgfmt/                 Postgres array/vector literal formatting helpers
  crypto/                field encryption (Encryptor) + per-scope key management (KeyStore)
  dbscope/                RLS-aware scoped transaction helper (Querier, Run, SetSession)
  audit/                  single audit_log writer (Write, LogStandalone) used by store/consolidation/CLI tools
  provider/               LLM vendor abstraction: Provider interface, OpenAICompat, Anthropic, Registry, VisionCapable (image captioning)
  ingest/                 pure-function file-attachment text extraction (Extract: .txt/.docx/.pdf -> plain text) — no DB/network, a leaf package like crypto/pgfmt
  auth/                   scope provisioning + operator credentials (this repo); real API-key resolution is the Tier 3 extension — see §6
  gateway/                HTTP surface: Handler, request/response types, Retriever/Capturer/Authenticator interfaces; attachments.go merges file/image attachments into the captured turn
  store/                  Postgres+pgvector implementation of Capturer/Retriever/Trace
  consolidation/          the nightly rollup engine (Runner)
  hpmf/                   HPMF read/write (export/import) + age packaging — MEMORY_FORMAT.md's implementation
  rotate/                 online, resumable per-scope key rotation (Start, Continue, Status, Prune)
  reembed/                online, resumable per-scope re-embedding after an embedding-provider change (Runner: Status, Continue, LogRun) — used by cmd/hupi-reembed
  selfcheck/              probe runner used by cmd/hupi-selfcheck
  bootstrap/              startup wiring: config, DB connection, key store
  demo/                   anonymous guest-session lifecycle (create/resolve/sweep) backing the public hosted demo — used by cmd/hupi-demo, cmd/hupi-demo-sweep
  metrics/                every Prometheus metric HUPI exposes at /metrics, plus an http.HandlerFunc-wrapping helper — used by cmd/hupi, cmd/hupi-demo, internal/gateway

schema/                   numbered SQL migrations, applied in order 0001 -> 0021
                          migrate.sh — POSIX-sh idempotent runner used by the Docker image/Kubernetes migrate Job (install.sh's bash equivalent is for bare-metal; kept as two separate implementations on purpose, see migrate.sh's own comment)
                          migration_scripts_consistency_test.go — a `schema`-package Go test parsing both migrate.sh and install.sh, failing if they ever disagree on the migration file set or a probe's SQL (the two scripts' own duplication is deliberate, see migrate.sh's comment; this only guards against it silently drifting)
docs/                      this file, and everything else under docs/
bench/                     LoCoMo/LongMemEval benchmark data-fetch scripts, each benchmark's own unmodified upstream scoring code, the EvalMem Python adapter, and archived run results — the non-Go scaffolding cmd/hupi-bench's predictions need to actually get scored; see docs/BENCHMARKS.md and docs/EVALMEM_INTEGRATION_PLAN.md

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
ingest     (no internal deps — only stdlib + github.com/ledongthuc/pdf)

crypto     -> identity
dbscope    -> identity
audit      -> identity, dbscope
gateway    -> identity, provider, ingest

auth       -> identity, crypto
store      -> identity, provider, crypto, dbscope, pgfmt, gateway, audit
consolidation -> identity, provider, crypto, dbscope, pgfmt, audit
hpmf       -> identity, crypto, dbscope, pgfmt, audit   (+ filippo.io/age)
rotate     -> identity, crypto, dbscope, audit
reembed    -> identity, crypto, dbscope, pgfmt, audit, provider, consolidation (EmbedderIdentity, EpisodeEmbedImportanceThreshold, Episode/EntityEmbedText)

selfcheck  -> gateway, provider

bootstrap  -> identity, crypto, provider   (+ blank-imports the pgx driver)

demo       -> identity, auth, dbscope
metrics    (no internal deps — only prometheus/client_golang + net/http)

cmd/hupi               -> bootstrap, gateway, store, auth, metrics
cmd/hupi-demo          -> bootstrap, auth, consolidation, demo, gateway, metrics, store
cmd/hupi-demo-sweep    -> bootstrap, auth, demo
cmd/hupi-consolidate   -> bootstrap, consolidation, identity
cmd/hupi-trace         -> bootstrap, store, identity
cmd/hupi-correct       -> bootstrap, consolidation, identity
cmd/hupi-selfcheck     -> bootstrap, store, selfcheck
cmd/hupi-admin         -> bootstrap, auth, audit, identity
cmd/hupi-admin-ui      -> bootstrap, auth, audit, identity
cmd/hupi-audit         -> bootstrap
cmd/hupi-export        -> bootstrap, hpmf, auth, identity   (+ filippo.io/age)
cmd/hupi-import        -> bootstrap, hpmf, auth, identity   (+ filippo.io/age)
cmd/hupi-rotate-key    -> bootstrap, rotate, identity
cmd/hupi-reembed       -> bootstrap, reembed, identity
cmd/hupi-bench         -> bootstrap, auth, dbscope, gateway, identity, store   (+ shells out to the built hupi-consolidate binary)
cmd/hupi-ingest-turns  -> bootstrap, auth, gateway, identity, store   (+ shells out to hupi-consolidate)
cmd/hupi-export-memory -> bootstrap, identity, store
cmd/hupi-answer-question -> bootstrap, gateway, identity, store
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
| `crypto` | AES-256-GCM field encryption; one or more keys per scope (versioned for rotation) | `Encryptor`, `KeyStore` (`GetOrCreate` = current version, `GetVersion` = a specific one, `CurrentVersion`, `CreateNextVersion`, `Evict`), `GenerateDEK`, `WrapDEK`/`UnwrapDEK`, `ErrKeyVersionNotFound` (sentinel a caller can `errors.Is` against to detect, and recover from, a version pruned out from under a concurrent reader — see `reembed.decryptWithRetry` below) |
| `dbscope` | Open a transaction with RLS session variables set correctly | `Querier`, `SetSession`, `Run` |
| `audit` | Single writer for every `audit_log` row, any event type, plus the read side | `Entry`, `Write`, `LogStandalone`, `Event*` constants, `Query`, `QueryFilter` (`TargetID` included, matched against `target_ref->>'id'` via a partial expression index, `schema/0020_audit_log_target_ref_index.sql`) |
| `provider` | One interface per vendor wire format, not per vendor | `Provider`, `OpenAICompat`, `Anthropic`, `Registry`, `Role` (`Valid()` — checked in `handleChatCompletionsScoped` alongside the existing empty-messages check, same `FeedbackRating.valid()` style), `VisionCapable` (`DescribeImage` — a separate, narrow interface both adapters implement for image-attachment captioning, not a change to `Provider`/`Message`/`ChatRequest` themselves), `ImageInput` |
| `ingest` | Convert one uploaded file's raw bytes to plain text, once, at ingest time | `Extract` (dispatches on sniffed magic bytes — `.txt`/`.docx`/`.pdf` — never on caller-supplied filename/content-type alone), `Attachment`, `Result` (`Text` + an optional non-fatal `Warning`, e.g. a scanned/image-only PDF with no extractable text layer) |
| `auth` | Scope provisioning + operator credentials (this repo); real end-user/team auth is a separate, commercially-licensed extension — see §6 | `Store` (`CreateUser`, `CreateTeam`, `ListUsers`, `ListTeams`, `CreateOperator`, `ResolveOperator`, `ListOperators`, `RevokeOperator`); `TeamAuthenticator` interface + `NewTeamAuthenticator` hook (nil unless the Tier 3 extension is present) |
| `gateway` | The HTTP surface + the interfaces storage must implement | `Handler`, `Retriever`, `Capturer`, `Authenticator`, `Episode`, `RetrievalResult`; `mergeAttachments` (`attachments.go`) — merges file/image attachments into the last user message's content before capture, via `internal/ingest` for documents and `provider.VisionCapable.DescribeImage` for images |
| `store` | Postgres+pgvector implementation of retrieval/capture/trace | `Store` (`Retrieve`, `Capture`, `Trace`) |
| `consolidation` | Nightly rollup: episodes -> grounded summaries | `Runner` (`RunDaily`, `RunRollup`, `Correct`) |
| `hpmf` | Portable memory format read/write (MEMORY_FORMAT.md) + age packaging | `ExportScope`, `ExportBundle`, `ImportScope`, `PackAndEncrypt`, `DecryptAndUnpack`, `Manifest` |
| `rotate` | Online, resumable per-scope key rotation | `Runner` (`Start`, `Continue`, `Status`, `Prune`), `MaxBatchSize` (5000, enforced by `clampBatchSize` at the top of `Continue` — a caller-requested batch above the cap is silently capped, non-positive is coerced to 1) |
| `reembed` | Bulk re-embed a scope's summaries/high-importance episodes/entities/grounded key facts after an embedding-provider change | `Runner` (`Status`, `Continue`, `LogRun`) |
| `selfcheck` | Run probes against a live `Retriever` | `Probe`, `Result`, `Run` |
| `bootstrap` | Read config, connect Postgres, build the `KeyStore`; register `identity.DefaultUserID` in `users` on every startup (idempotent — `ensureDefaultUser`, not just the one-time legacy-DEK migration path) | `Deps`, `Load` |
| `demo` | Anonymous guest-session lifecycle (create/resolve/sweep), capped usage — backs the public hosted demo | `Store` (`CreateSession`, `Resolve`, `ConsolidateNow`, `Sweep`), `IPRateLimiter` |
| `metrics` | Every Prometheus metric HUPI exposes at `/metrics`, plus a handler-wrapping helper | `InstrumentHandler`, `RetrievalGateTotal`, `AuthResolveTotal`, `ProviderCallDuration`, `ProviderCallErrorsTotal`, `CaptureTotal`, `CaptureDuration`, `ConsolidationRunsTotal`, `ConsolidationDuration`, `GroundingFactsTotal`, `RollupRunsTotal` |

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

Every database read or write that touches `episodes`, `summaries`,
`summary_key_facts`, or `entities` runs inside a transaction opened by
`dbscope.Run` (or, for `consolidation.storeSummary`'s multi-statement
write, a transaction that calls `dbscope.SetSession` directly). That call
sets four Postgres session variables (`hupi.acting_scope_kind`/`owner`,
`hupi.workspace_scope_kind`/`owner`) that the RLS policies in
`schema/0005_hardening_phase3_rls.sql` (and, for `summary_key_facts`,
`schema/0018_summary_key_facts_rls.sql` — it had none of its own before,
relying only on caller-side sequencing) check on every row. Code that
bypasses `dbscope` and queries `*sql.DB` directly will see **zero rows**
under RLS, not an error — this is deliberate (fail closed), and is
exactly what `internal/store/rls_test.go` and
`internal/store/summary_key_facts_rls_test.go` test for.

## 5. Non-HTTP entry points, call chain by call chain

`cmd/hupi`'s own HTTP routes are covered in full in
[API_REFERENCE.md](API_REFERENCE.md). The other binaries, below —
including `cmd/hupi-demo`, which serves its own small HTTP surface via
the same `gateway.Handler` but isn't walked route-by-route in
API_REFERENCE.md, which is scoped to the main gateway.

### `cmd/hupi-consolidate` — nightly rollup

1. `bootstrap.Load(ctx)` — config, DB, `KeyStore`.
2. `loadActiveScopes(ctx, deps.DB)` (`cmd/hupi-consolidate/main.go`) — `select id from users` + `select id from teams`, returns one `identity.Scope` per row. Relies on `bootstrap.Load`'s `ensureDefaultUser` having already registered `identity.DefaultUserID` — otherwise a fresh Tier 1/2 install's default-user episodes would be captured but never discovered here, silently never consolidated.
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
| `query -scope-kind -scope-owner -actor -event-type -target-id -since -until -limit` | Builds a parameterized `WHERE` clause from whichever flags are set, `order by ts asc` — `-target-id` matches `target_ref->>'id'` via a partial expression index (`schema/0020_audit_log_target_ref_index.sql`), answering "every event about entity X" directly |

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
3. Loop `rotate.Runner.Continue(ctx, scope, batchSize, actor)` until `done`: `batchSize` is first clamped by `clampBatchSize` (`rotate.MaxBatchSize` = 5000 — a caller-requested value above the cap is silently capped, non-positive coerced to 1, bounding how long a single batch's transaction and row locks stay open). Each call then migrates up to that many rows of whichever table `cursor_table` points at (`episodes` -> `summaries` -> `entities`, migrating each `summaries` row's `summary_key_facts` alongside it), decrypting with `keys.GetVersion(fromVersion)` and re-encrypting with `keys.GetVersion(toVersion)` inside one transaction per batch; a row with a `NULL` encrypted column (`episodes.note` on a non-feedback row) is left `NULL`, not turned into an encrypted `""`. Advancing past the last table calls `markCompleted` (`status='completed'`, another `audit_log` entry).
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

### `cmd/hupi-reembed` — online, resumable re-embedding

Same shape as `cmd/hupi-rotate-key`, for a different trigger: the active
embedding provider changed, so existing vectors aren't comparable to new
ones.

1. `bootstrap.Load(ctx)`.
2. `reembed.Runner.Status(ctx, scope)` (`internal/reembed/reembed.go`) —
   a live `COUNT` per table (`summaries`, `episodes`, `entities`) of rows
   where `embedding is null or embedding_model is distinct from` the
   active provider's identity; printed, no writes. Unlike
   `internal/rotate`, there's no persisted cursor or `key_rotations`-style
   status row — "needs re-embedding" is a self-correcting predicate, so a
   crash/restart just re-runs the same query rather than needing to
   resume from a saved position.
3. Loop `reembed.Runner.Continue(ctx, scope, batchSize)` until `done`:
   each call processes up to `batchSize` rows from whichever table has
   work first (`summaries` -> `episodes` -> `entities` ->
   `summary_key_facts`) — decrypt via `r.decryptWithRetry`
   (`internal/reembed/reembed.go`), re-embed via `provider.Provider.Embed`,
   write back `embedding`/`embedding_model` inside a `dbscope.Run`
   transaction per batch. Episodes only count if `type='interaction' and
   importance >= consolidation.EpisodeEmbedImportanceThreshold`, and key
   facts only count if `grounded` and belonging to a current (non-
   superseded) summary — one never meant to be embedded, or never meant
   to be read again, is simply out of scope, not pending.
   `reembedKeyFactBatch` is the one exception to "one row, one `Embed`
   call": facts are short, so a whole batch is embedded in a single
   request (same reasoning `internal/consolidation`'s own write-time
   `embedKeyFacts` uses), with a mismatched vector count treated as a hard
   error rather than silently skipped. `decryptWithRetry` exists because a
   batch's `key_version`/ciphertext pair, read at the top of the batch,
   can go stale if `internal/rotate` migrates and then prunes the row's
   old version before this call decrypts it: `KeyStore.GetVersion` fails
   with `crypto.ErrKeyVersionNotFound`, and the retry re-reads *both* the
   current `key_version` and the ciphertext columns together (not just
   the version — a stale ciphertext paired with the fresh version fails
   decryption too) before retrying once.
4. `reembed.Runner.LogRun(ctx, scope, actor, counts)` once, after the
   loop finishes with any rows processed — one `audit.Entry`
   (`internal/audit`). Unlike `internal/rotate` (which audits
   automatically per step), this is the caller's own responsibility,
   since there's no multi-step state machine to hang it off of.

### `cmd/hupi-demo` — the public hosted demo

A deliberately separate long-lived HTTP server (own binary, own listen
address, own process) from `cmd/hupi` — so a bug in anonymous-demo
handling can never affect a real self-hosted deployment.

1. `bootstrap.Load(ctx)` + `VerifyEmbedding`; builds a real `store.New`,
   `auth.New`, and `consolidation.New(...)`.
2. `demo.New(deps.DB, authStore, runner, loadLimits())`
   (`internal/demo/store.go`) plus a `demo.NewIPRateLimiter(...)` — the
   in-memory per-IP limiter is a cheap casual-case guard; the real cost
   control is `demo.Store`'s own DB-backed daily/per-session caps.
3. `gateway.Handler{Auth: demoStore, Retriever: store, Capturer: store, ...}`
   — `demo.Store` itself implements `gateway.Authenticator` (`Resolve`),
   so a normal chat turn runs the same unmodified retrieval/injection/
   capture path as a real deployment; nothing is mocked or short-circuited
   in `gateway`/`store` for demo mode.
4. Routes, each wrapped in `metrics.InstrumentHandler`: `POST
   /demo/session` (`demo.Store.CreateSession`, gated by the IP limiter and
   a daily session cap), `POST /v1/chat/completions` (the real handler),
   `POST /demo/consolidate-now` (`demo.Store.ConsolidateNow` — runs a real
   `consolidation.Runner.RunDaily`, gated by its own separate counter so
   it can't become an unmetered LLM side channel), plus `GET
   /healthz`/`/readyz`/`/metrics` (the last unwrapped, `promhttp.Handler()`
   directly). CORS is restricted to a single configured origin
   (`HUPI_DEMO_ALLOWED_ORIGIN`).

Configured entirely by environment variables, no flags —
`HUPI_DEMO_LISTEN_ADDR`, `HUPI_DEMO_ALLOWED_ORIGIN`,
`HUPI_DEMO_SESSION_TTL`, `HUPI_DEMO_MAX_MESSAGES_PER_SESSION`,
`HUPI_DEMO_MAX_CONSOLIDATE_PER_SESSION`, `HUPI_DEMO_MAX_SESSIONS_PER_DAY`,
`HUPI_DEMO_MAX_SESSIONS_PER_IP_PER_HOUR`.

### `cmd/hupi-demo-sweep` — expire demo sessions

A cron job, same per-run shape as `cmd/hupi-consolidate`, not a
long-lived process:

1. `bootstrap.Load(ctx)`; `auth.New`; `demo.New(deps.DB, authStore, nil, demo.DefaultLimits)`
   — `nil` for the `ConsolidationRunner`, since a sweep never consolidates
   anything.
2. `demoStore.Sweep(ctx, 24*time.Hour)` — deletes every guest session past
   its own `expires_at`, plus a hard 24h backstop regardless of
   `expires_at` (a safety net against a TTL-logic bug leaving sessions
   around forever).

### `cmd/hupi-bench` — LoCoMo/LongMemEval benchmark harness

Not part of a normal deployment — the tool behind the real,
independently-reproducible numbers in [BENCHMARKS.md](BENCHMARKS.md).
Drives a real `gateway.Handler` (same wiring pattern as
`cmd/hupi-demo`/`cmd/hupi`) against a benchmark conversation, end to end:

1. `bootstrap.Load` + `VerifyEmbedding`; loads the benchmark data
   (`loadLoCoMoAll`/`loadLongMemEvalAll`, `cmd/hupi-bench/locomo.go`/
   `longmemeval.go`) into one shared `benchConversation{id, sessions, qa}`
   shape; `loadPriorAnswers` resumes from a prior `-out-file` so a
   late-batch failure (a real past incident: a late 429 once discarded
   ~31 already-consolidated conversations' work) doesn't lose completed
   work.
2. Per conversation: `resetScope` (raw scoped deletes) + `auth.Store.CreateUser`
   provision a fresh, isolated scope; builds a `gateway.Handler` exactly
   like a real deployment would.
3. One of three modes (`cmd/hupi-bench/replay.go`): `-baseline` (skip
   HUPI entirely, stuff raw transcripts into the answer model's context —
   the no-memory control); `-answer-only` (skip replay/consolidation,
   re-answer against an already-consolidated scope); or the real path —
   replay every session via `HandleChatCompletions` with a backdated
   `handler.Now`, then **shell out** to the real, separately-built
   `hupi-consolidate` binary once per distinct fabricated date (rollup
   logic lives only in that binary's own package, not in-process here).
4. QA phase: real retrieval, captured via `gateway.Handler.OnRetrieve`
   (so the diagnostic context dump is provably what the real answer saw,
   not a second, possibly-different `Retrieve()` call) + a real answer
   call using a dedicated concise/abstention-tuned system prompt. `X-Hupi-Capture: off`
   during QA turns so diagnostic calls don't pollute real episode memory.
5. Writes predictions in each benchmark's own native scoring-input shape
   (`marshalLoCoMoPredictions`/`marshalLongMemEvalHypotheses`) — scored by
   `bench/score_locomo.py`/`bench/score_longmemeval.sh`, each invoking
   that benchmark's own unmodified upstream scoring code.

### `cmd/hupi-ingest-turns`, `cmd/hupi-export-memory`, `cmd/hupi-answer-question` — EvalMem adapter tools

Three small CLIs, not part of a normal deployment, that
`bench/evalmem/hupi_adapter.py` shells out to — the Go-side half of
HUPI's integration with [EvalMem](https://github.com/ZeyuuLiu/EvalMem),
an external memory-diagnostic framework (see
[EVALMEM_INTEGRATION_PLAN.md](EVALMEM_INTEGRATION_PLAN.md)). Each
deliberately duplicates small pieces of `cmd/hupi-bench` (wire types,
`sendChatTurn`, the QA system prompt) rather than importing it — a
conscious "small tools don't cross-import" choice, not an oversight.

- **`hupi-ingest-turns`** implements EvalMem's `ingest_conversation`:
  reads a flat JSON array of turns from stdin (EvalMem's own flattened
  LoCoMo shape), groups them back into sessions by `session_index`,
  provisions a fresh scope (`user:evalmem-<sample-id>`), replays each
  session with a backdated `handler.Now`, then — unless
  `-skip-consolidate` — shells out to `hupi-consolidate -date <d>` per
  distinct date, same pattern as `cmd/hupi-bench`. Prints a `run_ctx` JSON
  blob to stdout for the Python adapter to pass into later calls.
- **`hupi-export-memory`** implements `export_full_memory`: a thin CLI
  wrapper around `store.Store.ExportMemory` (the actual logic lives in
  `internal/store`, not here) — dumps a scope's entire decrypted memory
  (every entity, every current summary + key facts, every relationship)
  as JSON. Contrasts with `cmd/hupi-trace`: trace answers "what did this
  one turn see," this answers "what does this scope know, full stop."
- **`hupi-answer-question`** implements `retrieve_original`/
  `generate_online_answer`/`generate_oracle_answer`: answers one question
  against an already-ingested scope, either in native mode (a real
  `HandleChatCompletions` call, context captured via `OnRetrieve`) or
  oracle mode (`X-Hupi-Memory: off` so real retrieval never runs, with
  hand-fed context injected as its own system message instead). Both
  modes set `X-Hupi-Capture: off`.

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
| "What's the exact prompt text for grounding/consolidation/RRF/etc., and what's the real measured threshold behind a given number?" | [PROCESS_REFERENCE.md](PROCESS_REFERENCE.md) |
| "What's the record schema (episode/summary/entity)?" | [MEMORY_FORMAT.md](MEMORY_FORMAT.md) |
| "How are uploaded files/images turned into memory?" | [ARCHITECTURE.md § Attachments](../ARCHITECTURE.md), [HOW_IT_WORKS.md §3](HOW_IT_WORKS.md), `internal/ingest/`, `internal/gateway/attachments.go` |
| "Is X actually implemented, or just designed?" | [DESIGN_VS_BUILT.md](DESIGN_VS_BUILT.md) |
| "How does the multi-tenant/team model work?" | [TIER3_PLAN.md](TIER3_PLAN.md) |
| "Why isn't Tier 3's code in this repo, and how does the build still work?" | §6 above, [ARCHITECTURE.md § Licensing and the open-core split](../ARCHITECTURE.md) |
| "How does encryption/RLS actually work?" | [HARDENING_PLAN.md](HARDENING_PLAN.md), §4 above |
| "What does this product do, for whom?" | [BUSINESS_PROCESS.md](BUSINESS_PROCESS.md) |
| "How do I deploy this to Kubernetes?" | [INSTALL.md § Containerized deployment](INSTALL.md#containerized-deployment), [Dockerfile](../Dockerfile), [deploy/k8s/](../deploy/k8s/), [deploy/helm/hupi/](../deploy/helm/hupi/) |
| "How do I use HUPI from inside VS Code?" | [VSCODE_EXTENSION.md](VSCODE_EXTENSION.md), [vscode-extension/](../vscode-extension/) |
| "Where do the published LoCoMo/LongMemEval numbers come from, and can I reproduce them?" | [BENCHMARKS.md](BENCHMARKS.md), §5's `cmd/hupi-bench` entry above, [bench/](../bench/) |
