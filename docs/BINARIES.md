# HUPI binaries — what each one does and how to run it

Every `cmd/` binary in this repo, grouped by how you're meant to run it:
a long-running server, a recurring scheduled job, or a one-off operator/
benchmark command. For *how these fit together* at a call-chain level,
see [CODE_GUIDE.md §5](CODE_GUIDE.md); this document is the
run-it-yourself reference — every flag, every binary-specific env var,
one real example invocation each.

All 19 binaries call `bootstrap.Load` first, which reads a standard set
of env vars shared by every one of them (not repeated per-binary below):

| Env var | Purpose |
|---|---|
| `HUPI_DATABASE_URL` (or `HUPI_APP_DATABASE_URL`) | Postgres connection string |
| `HUPI_PROVIDERS_CONFIG` | path to `providers.yaml` (defaults to `./providers.yaml`) |
| `HUPI_KEK` | base64 key-encryption key (envelope encryption root) |
| `HUPI_WRAPPED_DEK_PATH` | where the wrapped per-deployment DEK lives |

A provider-specific API key (whatever `providers.yaml`'s `api_key_env`
names for each profile, e.g. `OPENAI_API_KEY`) is also bootstrap-standard
and not re-listed per binary.

Every operator CLI binary below also accepts `-actor` (default: `$USER`,
falling back to `$LOGNAME`, then `"unknown"`) — who gets recorded on that
action's `audit_log` entry.

---

## Long-running servers (4)

Start once, left running (systemd/Kubernetes), never a cron job.

### `cmd/hupi` — the gateway

The product itself: an OpenAI-compatible server answering
`POST /v1/chat/completions` and `POST /v1/feedback`, wiring retrieval,
the provider registry, and capture together. Also serves `GET /healthz`
(liveness, no DB touch), `GET /readyz` (readiness, pings DB), and
`GET /metrics` (Prometheus).

- **Flags**: none.
- **Env vars**: `HUPI_LISTEN_ADDR` (default `127.0.0.1:8787`);
  `HUPI_REQUIRE_AUTH` (default off — every request resolves to
  `identity.DefaultScope`; set to exactly `"true"` to require a real API
  key via the Tier 3 authenticator).
- **Run it**:
  ```bash
  HUPI_DATABASE_URL=postgres://... HUPI_KEK=... \
  HUPI_WRAPPED_DEK_PATH=./dek.wrapped HUPI_PROVIDERS_CONFIG=./providers.yaml \
  HUPI_LISTEN_ADDR=0.0.0.0:8787 \
  ./hupi
  ```

### `cmd/hupi-admin-ui` — browser-based provisioning

The same user/team/operator/key operations `hupi-admin` exposes as a
CLI, over a browser — `/api/*` behind CSRF protection and HTTP Basic
named-operator auth. Never shows memory content (that's `hupi-trace`'s
job, deliberately CLI-only).

- **Flags**: none.
- **Env vars**: `HUPI_ADMIN_UI_LISTEN_ADDR` (default `127.0.0.1:8788`).
- **Run it**: `HUPI_ADMIN_UI_LISTEN_ADDR=127.0.0.1:8788 ./hupi-admin-ui`
  — then authenticate with a token from `hupi-admin create-operator`.

### `cmd/hupi-dashboard` — read-only analytics

Conversation volume, theme word cloud, entity-relationship graph,
memory health, retrieval-governance transparency, and a security-posture
panel (key rotation history, recent audit events) over one scope's own
memory. No login in this OSS build (every request resolves to
`identity.DefaultScope`); Tier 3 adds real sign-in.

- **Flags**: none.
- **Env vars**: `HUPI_DASHBOARD_LISTEN_ADDR` (default `127.0.0.1:8790`).
- **Run it**:
  `HUPI_DASHBOARD_LISTEN_ADDR=127.0.0.1:8790 ./hupi-dashboard`

### `cmd/hupi-demo` — the public hosted demo

Anonymous, short-lived guest sessions against the real
`gateway.Handler` — its own binary and listen address, since an
anonymous public endpoint is a fundamentally different trust boundary
than the main gateway. `POST /demo/session`, `POST /v1/chat/completions`,
`POST /demo/consolidate-now`, plus the same `/healthz`/`/readyz`/
`/metrics` trio.

- **Flags**: none.
- **Env vars**: `HUPI_DEMO_LISTEN_ADDR` (default `127.0.0.1:8789`);
  `HUPI_DEMO_ALLOWED_ORIGIN` (default `https://hupi.dev`, for CORS);
  `HUPI_DEMO_MAX_SESSIONS_PER_IP_PER_HOUR` (default `3`);
  `HUPI_DEMO_SESSION_TTL`, `HUPI_DEMO_MAX_MESSAGES_PER_SESSION`,
  `HUPI_DEMO_MAX_CONSOLIDATE_PER_SESSION`, `HUPI_DEMO_MAX_SESSIONS_PER_DAY`
  (each falls back to `internal/demo.DefaultLimits`' own default if unset).
- **Run it**:
  ```bash
  HUPI_PROVIDERS_CONFIG=./providers.demo.yaml \
  HUPI_DEMO_ALLOWED_ORIGIN=https://hupi.dev HUPI_DEMO_LISTEN_ADDR=0.0.0.0:8789 \
  ./hupi-demo
  ```

---

## Recurring scheduled jobs (3)

Invoked by cron (or a Kubernetes CronJob — see `deploy/k8s/08-*`/`09-*`).
Explicitly documented in each binary's own doc comment as *not* meant to
run as a long-lived process.

### `cmd/hupi-consolidate` — nightly rollup (daily)

Runs one consolidation pass per active scope (every user + every team),
plus weekly/monthly/yearly rollups when the date crosses a calendar
boundary. Re-running for an already-consolidated date regenerates from
that day's full episode set and supersedes the existing draft — not a
way to fix one wrong fact (use `hupi-correct` for that).

- **Flags**: `-date` (default: yesterday) — `YYYY-MM-DD`.
- **Env vars**: `HUPI_CONSOLIDATE_CONCURRENCY` (default `5` — bounds
  how many scopes run concurrently); `HUPI_PUSHGATEWAY_URL` (if set,
  pushes this run's metrics to a Prometheus Pushgateway afterward).
- **Run it**: `./hupi-consolidate` (defaults to yesterday) or
  `./hupi-consolidate -date 2026-09-09`.

### `cmd/hupi-demo-sweep` — expire demo sessions (~every 15 min)

Deletes expired hosted-demo guest sessions and everything they wrote.
Hardcodes a 24-hour sweep threshold.

- **Flags/env vars**: none — no configurable parameters at all.
- **Run it**: `./hupi-demo-sweep` (e.g. `*/15 * * * * /path/to/hupi-demo-sweep`).

### `cmd/hupi-selfcheck` — memory regression probes (weekly)

Re-runs a small, hand-written set of "does memory still know this"
questions against the live retrieval pipeline. Exits non-zero if any
probe fails, so it plugs directly into alerting.

- **Flags**: none — takes one optional **positional** argument (the
  probes file path, default `probes.yaml`).
- **Run it**: `./hupi-selfcheck probes.yaml`.

---

## One-off operator commands (12)

Run by hand, reactively or on demand — not scheduled, not servers.

### `cmd/hupi-admin` — identity provisioning (CLI)

Subcommands: `create-user`, `set-password`, `create-team`,
`add-member`/`create-key` (Tier 3 only — error out in this OSS build),
`create-operator`, `revoke-operator`.

- **Flags** (per-subcommand): `create-user`: `-id`, `-email`;
  `set-password`: `-user`, `-password` (omit to read from stdin, avoids
  shell history); `create-team`: `-id`, `-name`; `create-operator`:
  `-name`; `revoke-operator`: `-name`.
- **Run it**: `hupi-admin create-user -id user:alice -email alice@example.com`

### `cmd/hupi-audit` — query the audit trail (CLI)

Subcommands: `tail` (most recent N entries), `query` (filtered search
by scope/actor/event type/target/time range).

- **Flags**: `tail`: `-n` (default `20`). `query`: `-scope-kind`,
  `-scope-owner`, `-actor`, `-event-type`, `-target-id`, `-since`,
  `-until` (RFC3339), `-limit` (default `100`).
- **Run it**:
  `hupi-audit query -scope-kind shared -scope-owner team:acme-eng -event-type retrieve -since 2026-09-01T00:00:00Z -limit 200`

### `cmd/hupi-backfill-memories` — populate the unified `memories` table

Backfills `memories` from a scope's existing `entities.attributes` and
`summary_key_facts` — read-only against the old tables, idempotent and
resumable against the new one.

- **Flags**: `-scope-kind`, `-scope-owner` (required); `-status`
  (report-only, no writes); `-batch-size` (default `200`).
- **Run it**: `hupi-backfill-memories -scope-kind private -scope-owner user:alice`

### `cmd/hupi-bench` — LoCoMo/LongMemEval benchmark harness

Replays a public benchmark through the real gateway (real episodes,
real nightly consolidation by shelling out to the real
`hupi-consolidate` binary, real retrieval), then answers its questions
through the real chat path. Supports resume, a `-baseline` no-memory
control, and `-answer-only` (re-answer without re-replaying).

- **Flags**: `-benchmark` (`locomo`|`longmemeval`), `-data-file`,
  `-conv-index`, `-all-conversations`, `-baseline`, `-answer-only`,
  `-answer-model`, `-out-file`, `-retrieved-context-out-file`,
  `-consolidate-bin` (default `./hupi-consolidate`).
- **Run it**:
  `hupi-bench -benchmark locomo -data-file ./locomo10.json -all-conversations -out-file predictions.json`

### `cmd/hupi-correct` — write a correction (CLI)

Writes a corrected summary version that supersedes the wrong one — the
only sanctioned way to fix a bad memory. `-dump-template` writes the
summary's current content as an editing starting point (so a correction
only has to touch what's actually wrong, not hand-restate every
attribute).

- **Flags**: `-summary-id`, `-reason` (required unless dumping),
  `-content` (path to corrected JSON), `-dump-template` (path to dump
  current content to instead of correcting), `-scope-kind` (default
  `private`), `-scope-owner` (default the default user).
- **Run it**:
  ```bash
  hupi-correct -summary-id sum_2026-09-09_daily_v1 -dump-template correction.json
  # edit correction.json
  hupi-correct -summary-id sum_2026-09-09_daily_v1 -reason "wrong birth year" -content correction.json
  ```

### `cmd/hupi-export` — write a portable snapshot

Age-encrypted HPMF export of one scope, or (`-all`) every scope in the
deployment.

- **Flags**: `-scope-kind`, `-scope-owner` (one scope) or `-all` (whole
  deployment); `-out` (required); `-recipient` (age public key) or
  `-passphrase` (reads `$HUPI_EXPORT_PASSPHRASE`, deliberately never a
  flag so it never lands in shell history).
- **Env vars**: `HUPI_EXPORT_PASSPHRASE` (only with `-passphrase`).
- **Run it**:
  `hupi-export -scope-kind private -scope-owner user:alice -recipient age1... -out alice.age`

### `cmd/hupi-export-memory` — full decrypted memory dump (diagnostic)

Dumps every entity, every current summary with its key facts, and every
relationship for one scope as JSON to stdout — "what does this scope
know, full stop," unlike `hupi-trace`'s "what did this one turn see."
Built for external diagnostic tooling (EvalMem's `export_full_memory`).

- **Flags**: `-scope-kind` (default `private`), `-scope-owner` (or
  positional).
- **Run it**: `hupi-export-memory user:alice`

### `cmd/hupi-import` — load a portable snapshot back in

The reverse of `hupi-export`. Target scope must be empty unless
`-merge` is set (dedups by hash/id/period).

- **Flags**: `-in` (required), `-scope-kind`/`-scope-owner` (one scope)
  or `-all` (whole-deployment restore, provisioning users/teams as
  needed), `-merge`, `-identity` (age identity file) or `-passphrase`
  (reads `$HUPI_IMPORT_PASSPHRASE`).
- **Env vars**: `HUPI_IMPORT_PASSPHRASE` (only with `-passphrase`).
- **Run it**:
  `hupi-import -in alice.age -scope-kind private -scope-owner user:alice -identity alice-key.txt`

### `cmd/hupi-ingest-turns` — replay a conversation (EvalMem adapter)

Reads a flat JSON array of turns on stdin, replays them into the
gateway with backdated timestamps, then shells out to the real
`hupi-consolidate` once per distinct session date. Prints a `run_ctx`
JSON object an external Python eval harness needs for later calls.

- **Flags**: `-sample-id`, `-answer-model`, `-consolidate-bin` (default
  `./hupi-consolidate`), `-skip-consolidate` (replay only, faster smoke
  tests — don't use for a real evaluation).
- **Run it**:
  `hupi-ingest-turns -sample-id conv-26 -consolidate-bin ./hupi-consolidate < turns.json`

### `cmd/hupi-answer-question` — answer one question (EvalMem adapter)

Answers a question against an already-ingested scope, either via real
HUPI retrieval ("native" mode) or with hand-fed oracle context bypassing
retrieval ("oracle" mode). Always sets `X-Hupi-Memory` capture off so
diagnostic calls never pollute real memory.

- **Flags**: `-scope-owner`, `-scope-kind` (default `private`),
  `-question`, `-oracle-context`, `-oracle` (bool, required alongside
  `-oracle-context` so an intentionally empty oracle string isn't
  mistaken for "native mode"), `-answer-model`.
- **Run it**:
  `hupi-answer-question -scope-owner user:evalmem-conv-30 -question "What did I say about my dog?" -oracle -oracle-context "The user's dog is named Rex."`

### `cmd/hupi-reembed` — re-embed after a provider switch

Re-embeds summaries/high-importance episodes/entities/grounded key
facts that either never got a vector or got one from a model that isn't
the currently configured one. Safe to kill and re-run.

- **Flags**: `-scope-kind`, `-scope-owner` (required); `-status`
  (report-only); `-batch-size` (default `100`).
- **Run it**: `hupi-reembed -scope-kind private -scope-owner user:alice`

### `cmd/hupi-rotate-key` — online key rotation

Rotates one scope's data-encryption key with no downtime, resumable if
killed. `-prune-old-versions` deletes old key material after a
completed rotation.

- **Flags**: `-scope-kind`, `-scope-owner` (required); `-status`;
  `-prune-old-versions`; `-batch-size` (default `500`, capped at
  `internal/rotate.MaxBatchSize`).
- **Run it**: `hupi-rotate-key -scope-kind private -scope-owner user:alice`

### `cmd/hupi-trace` — retrieval audit (CLI)

Prints exactly what one turn's retrieval engine saw and injected:
episode metadata plus every retrieved summary, entity, episode, and
memory (including inferred/grounding-checked flags) that fed that
turn's context. "Why did it say that," turned from an unanswerable
question into a command.

- **Flags**: `-scope-kind` (default `private`), `-scope-owner` (default
  the default user). Requires one positional argument: `<episode_id>`.
- **Run it**: `hupi-trace ep_abc123def456`

---

## Quick reference

| Binary | Run mode | One-line purpose |
|---|---|---|
| `hupi` | server | The gateway — chat completions, retrieval, capture |
| `hupi-admin-ui` | server | Browser-based user/team/key provisioning |
| `hupi-dashboard` | server | Read-only memory analytics |
| `hupi-demo` | server | Public hosted demo (anonymous guest sessions) |
| `hupi-consolidate` | cron (daily) | Nightly summary + rollup generation |
| `hupi-demo-sweep` | cron (~15 min) | Expire demo guest sessions |
| `hupi-selfcheck` | cron (weekly) | Memory regression probes |
| `hupi-admin` | CLI | Create/manage users, teams, operators |
| `hupi-audit` | CLI | Query the audit trail |
| `hupi-backfill-memories` | CLI | Populate the unified `memories` table |
| `hupi-bench` | CLI | Replay LoCoMo/LongMemEval benchmarks |
| `hupi-correct` | CLI | Write a correction (supersede a wrong summary) |
| `hupi-export` | CLI | Portable encrypted scope/deployment backup |
| `hupi-export-memory` | CLI | Full decrypted memory dump (diagnostic) |
| `hupi-import` | CLI | Restore a portable backup |
| `hupi-ingest-turns` | CLI | Replay a conversation (EvalMem adapter) |
| `hupi-answer-question` | CLI | Answer one question (EvalMem adapter) |
| `hupi-reembed` | CLI | Re-embed after a provider/model switch |
| `hupi-rotate-key` | CLI | Online, resumable per-scope key rotation |
| `hupi-trace` | CLI | Show exactly what one turn's retrieval saw |
