# Installing HUPI — All Three Tiers

This document covers the actual, current install procedure — verified
against the code, not the original design. It assumes you've read
[BUSINESS_PROCESS.md](BUSINESS_PROCESS.md) for what the tiers mean; this
is the "how do I actually get one running" companion.

All three tiers run from the **same install** — Tiers 1 and 2 are
architecturally identical, and Tier 3 is the same install plus two extra
steps at the end. There's no separate build or package per tier.

This document covers the bare-metal path (Steps 1-6 below, or
`install.sh`). If you're deploying to Kubernetes instead, skip to
[Containerized deployment](#containerized-deployment) — same
prerequisites (an external Postgres you provide, no bundled database),
but the binaries and migrations ship in one Docker image instead.

## The fast path: `install.sh`

Everything below this section is the manual, step-by-step version of what
[`install.sh`](../install.sh) automates — pick your tier, and tell it
whether you already have Postgres and a provider config or want it to
provision both for you:

```bash
./install.sh                     # fully interactive — asks as it goes
./install.sh --tier=3 --yes      # non-interactive; fails loudly instead of
                                  # guessing when something has no safe
                                  # default (e.g. an LLM API key)
./install.sh --help              # every flag, including how to point it
                                  # at Postgres/providers you already run
```

It's safe to re-run — schema migrations, `hupi.env`, `HUPI_KEK`, and
`hupi_app`'s password are all re-detected and reused rather than
reapplied or rotated. The rest of this document is the reference for what
each of those steps actually does and why, and the path to reach for if
you want to do any of it by hand instead.

## Prerequisites

- PostgreSQL 16+ with the `pgvector` extension available
- Go 1.25+ (only needed to build the binaries; there are no prebuilt releases)
- At least one LLM provider API key (OpenAI, Anthropic, or anything else `internal/provider` supports)

## Step 1 — Postgres

Any Postgres with `pgvector` enabled works. For local use or evaluation,
this is the exact image the project itself was verified against:

```bash
docker run -d --name hupi-pg \
  -e POSTGRES_PASSWORD=hupi \
  -e POSTGRES_DB=hupi \
  -p 5432:5432 \
  pgvector/pgvector:pg16
```

For production, use whatever Postgres you already operate — managed or
self-hosted — as long as `pgvector` can be enabled on it. `pgvector` only
needs to exist as an extension; nothing else about your Postgres setup is
special-cased.

## Step 2 — Apply the schema, in order

The migrations are numbered and must be applied in that order — later
ones depend on tables and columns earlier ones create.

```bash
export HUPI_ADMIN_DATABASE_URL='postgres://postgres:hupi@localhost:5432/hupi?sslmode=disable'

for f in schema/0001_init.sql \
         schema/0002_tier3_phase1_identity.sql \
         schema/0003_tier3_phase2_retrieved_refs.sql \
         schema/0004_hardening_phase1_app_role.sql \
         schema/0005_hardening_phase3_rls.sql \
         schema/0006_hardening_phase4_scope_keys.sql \
         schema/0007_audit_log.sql \
         schema/0008_admin_operators.sql \
         schema/0009_export_import_audit_events.sql \
         schema/0010_key_rotation.sql \
         schema/0011_key_rotation_audit_event.sql \
         schema/0012_entity_embeddings.sql; do
  psql "$HUPI_ADMIN_DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"
done
```

Run these as the database owner or a superuser — `0004` creates the
`hupi_app` role and grants, which requires elevated privileges the
application's own runtime role deliberately doesn't have.

## Step 3 — Set the `hupi_app` password

Migration `0004` creates the `hupi_app` role on purpose without a
password, so no real secret ever sits in a file meant to be checked into
version control:

```bash
psql "$HUPI_ADMIN_DATABASE_URL" -c "ALTER ROLE hupi_app WITH PASSWORD 'pick-a-real-secret-here';"
```

`hupi_app` is a non-owner role — this matters beyond least-privilege
hygiene: row-level security (schema `0005`) is bypassed by table owners
and superusers by default, so the application must connect as `hupi_app`
for that protection to actually apply. See
[HARDENING_PLAN.md](HARDENING_PLAN.md) D4 for why.

## Step 4 — Build the binaries

```bash
go build -o bin/hupi             ./cmd/hupi
go build -o bin/hupi-consolidate ./cmd/hupi-consolidate
go build -o bin/hupi-selfcheck   ./cmd/hupi-selfcheck
go build -o bin/hupi-trace       ./cmd/hupi-trace
go build -o bin/hupi-correct     ./cmd/hupi-correct
go build -o bin/hupi-audit       ./cmd/hupi-audit
go build -o bin/hupi-export      ./cmd/hupi-export
go build -o bin/hupi-import      ./cmd/hupi-import
go build -o bin/hupi-rotate-key  ./cmd/hupi-rotate-key
go build -o bin/hupi-admin       ./cmd/hupi-admin
go build -o bin/hupi-admin-ui    ./cmd/hupi-admin-ui  # optional — browser alternative to hupi-admin, see ADMIN_UI.md
```

All six share one `go.mod` — a single `go build ./...` from the repo root
also works if you'd rather not name each one.

**`hupi-admin-ui` embeds a React frontend built separately with npm** —
`cmd/hupi-admin-ui/assets.go` does `//go:embed web/dist`, and
`cmd/hupi-admin-ui/web/dist` is npm build output, not source-controlled
(only a placeholder `index.html` is committed there so a plain `go build`
never fails on a fresh clone — see `cmd/hupi-admin-ui/web/README.md`).
`install.sh` and the Dockerfile both run the npm build automatically
before compiling this binary; if you're building `hupi-admin-ui` by hand
from a fresh clone, run the npm build first or you'll just get the
placeholder page at `/`:

```bash
cd cmd/hupi-admin-ui/web && npm ci && npm run build && cd -
go build -o bin/hupi-admin-ui ./cmd/hupi-admin-ui
```

## Step 5 — `providers.yaml`

```bash
cp providers.yaml.example providers.yaml
```

Edit it: set `active_chat_provider`, `active_consolidation_provider`,
`active_grounding_provider`, and `active_embedding_provider` to real
profile names, each with a real `api_base`, `api_key_env`, and `model`.
These four roles can point at the same vendor or different ones — see
[ARCHITECTURE.md § Provider abstraction](../ARCHITECTURE.md) for why
`active_consolidation_provider` is deliberately kept separate and more
stable than the one you chat with day to day.

## Step 6 — Environment variables

These are the same for every tier:

```bash
export HUPI_APP_DATABASE_URL='postgres://hupi_app:pick-a-real-secret-here@localhost:5432/hupi?sslmode=disable'
export HUPI_KEK=$(head -c32 /dev/urandom | base64)
export HUPI_PROVIDERS_CONFIG=./providers.yaml
export OPENAI_API_KEY=...   # whatever your providers.yaml actually references, per profile
```

**`HUPI_KEK` is the one value you cannot lose and cannot regenerate.** It
wraps every scope's individual encryption key
([HARDENING_PLAN.md](HARDENING_PLAN.md) D5-D7) — losing it makes every
stored conversation permanently unreadable. Generate it once, store it in
a real secrets manager or password manager immediately, and use the exact
same value on every subsequent start of every binary.

You do **not** need to set `HUPI_WRAPPED_DEK_PATH` for a fresh install —
that variable only matters when migrating a pre-Tier-3 deployment that
has an old, single-file encryption key (see "Upgrading an existing
deployment" below). A brand-new install generates each scope's key
on demand in the database, automatically, the first time it's needed.

At this point the shared setup is done. What differs per tier is what you
do next.

---

## Tier 1 — Personal

Nothing further. Start it:

```bash
./bin/hupi
```

By default it listens on `127.0.0.1:8787`. No `Authorization` header is
required or checked by any client — every request resolves to a single
built-in identity, `user:default`. Point any OpenAI-compatible client at
`http://127.0.0.1:8787/v1/chat/completions` and it works immediately.

To confirm it's actually up:

```bash
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model": "gpt-4.1", "messages": [{"role": "user", "content": "hello"}]}'
```

A `502` here means the gateway itself is fine but the configured upstream
provider call failed (check `providers.yaml` and your API key); a
connection error means the gateway isn't running or isn't listening where
you expect (check `HUPI_LISTEN_ADDR` if you set one).

## Tier 2 — Professional (Single)

**Identical install to Tier 1.** There is no separate binary, flag, or
code path for Tier 2 — it describes *who operates the deployment and how*
(IT-managed rather than self-managed), not a different install procedure.
If your organization wants centrally managed secrets, pull `HUPI_KEK`
from a real KMS instead of a local `export` in Step 6; if you want
org-mandated retention policy, that's a process you enforce around this
install, not a switch inside it. None of that wrapper tooling ships with
HUPI itself today.

## Tier 3 — Professional (Shared)

Two additions on top of the Tier 1 install.

**1. Require authentication.** Without this, team routes exist but always
return `403` — there's no way to prove team membership without real auth
configured:

```bash
export HUPI_REQUIRE_AUTH=true
```

**2. Provision identities before anyone uses a team workspace:**

```bash
./bin/hupi-admin create-user -id user:alice -email alice@example.com
./bin/hupi-admin create-key  -user user:alice
# prints a raw API key exactly once — save it now; it cannot be shown again,
# only revoked and replaced with a new one

./bin/hupi-admin create-team -id team:acme-eng -name "Acme Engineering"
./bin/hupi-admin add-member  -team team:acme-eng -user user:alice -role admin
```

Start the gateway the same way as Tier 1 (`./bin/hupi`). From here:

- Alice's clients send `Authorization: Bearer <the key from create-key>`.
- Her private conversations go to `/v1/chat/completions`, exactly as in
  Tier 1.
- Team conversations go to `/v1/team/team:acme-eng/chat/completions`
  instead — reachable only because she's a member; any other
  authenticated user gets `403` on that path.

Repeat `create-user`/`create-key`/`add-member` for every additional team
member. There is no self-service signup or invite flow — every identity
is provisioned by whoever runs `hupi-admin`.

**Alternative to the CLI**: `./bin/hupi-admin-ui` does the same
provisioning (plus listing and API key revocation) over a browser instead
of a shell. It has no shared secret to set — provision a named operator
credential first (each operator's actions are individually recorded in
`audit_log`, see below):

```bash
./bin/hupi-admin create-operator -name alice
# prints a raw token exactly once — save it now; it cannot be shown again,
# only revoked (hupi-admin revoke-operator) and replaced with a new one
./bin/hupi-admin-ui
```

Visit `http://127.0.0.1:8788/` and authenticate as `alice` with that
token as the password. See [ADMIN_UI.md](ADMIN_UI.md) for what it does
and does not cover, and its exposure model (binds to `127.0.0.1` by
default, same as the gateway).

**Audit trail**: every capture, retrieval, `hupi-trace` investigation, and
admin action (CLI or UI) is recorded in `audit_log`. Query it with:

```bash
./bin/hupi-audit tail -n 20
./bin/hupi-audit query -scope-owner team:acme-eng -since 2026-09-01T00:00:00Z
```

## Export / import (any tier)

Move a scope's memory somewhere else — a personal backup, a migration to
a new deployment, or (with `-all`) a full-deployment restore — see
[MEMORY_FORMAT.md](MEMORY_FORMAT.md) for the on-disk format itself:

```bash
export HUPI_EXPORT_PASSPHRASE='pick a real passphrase, not this one'
./bin/hupi-export -scope-kind private -scope-owner user:alice -passphrase -out alice.age

export HUPI_IMPORT_PASSPHRASE="$HUPI_EXPORT_PASSPHRASE"
./bin/hupi-import -in alice.age -scope-kind private -scope-owner user:alice -passphrase
```

For a real deployment, prefer `-recipient <age1...public key>` /
`-identity <keyfile>` over a passphrase — generate a keypair once with
`age-keygen`, keep the private half somewhere at least as protected as
`HUPI_KEK`, and never put the passphrase on the command line (only in
the env var, so it never lands in shell history or `ps`). `-all` instead
of `-scope-kind`/`-scope-owner` exports or restores every scope; `-merge`
on import allows loading into a scope that already has data instead of
requiring an empty one. Every export and import writes an `audit_log`
entry, queryable the same way as above.

## Key rotation (any tier)

Rotate a scope's encryption key without downtime — the gateway keeps
serving that scope's traffic the whole time, since every new write
switches to the new key immediately:

```bash
./bin/hupi-rotate-key -scope-kind private -scope-owner user:alice
```

This runs to completion in one invocation, migrating already-encrypted
rows in batches. If it's killed partway through, re-running the exact
same command resumes rather than restarting or corrupting anything:

```bash
./bin/hupi-rotate-key -scope-kind private -scope-owner user:alice -status
```

Old key material is kept, not deleted, once a rotation completes — that
costs nothing and means a missed row can never become permanently
unreadable. Once you're confident nothing needs it:

```bash
./bin/hupi-rotate-key -scope-kind private -scope-owner user:alice -prune-old-versions
```

**If this was prompted by a suspected compromise**, pruning only removes
the persisted key from the database — it can't reach into a
still-running process's memory. Restart the gateway (and
`hupi-consolidate`, if it's mid-run) after pruning, not just the terminal
you ran `hupi-rotate-key` from.

---

## Cron jobs (every tier, strongly recommended)

```cron
# nightly — turns that day's raw conversations into searchable summaries
0 2 * * * /path/to/bin/hupi-consolidate

# weekly — regression-checks retrieval against hand-written probes
0 3 * * 1 /path/to/bin/hupi-selfcheck /path/to/probes.yaml
```

Both need the same environment variables as `hupi` itself
(`HUPI_APP_DATABASE_URL`, `HUPI_KEK`, `HUPI_PROVIDERS_CONFIG`) — set them
in the cron environment, not just your interactive shell.

Without `hupi-consolidate` running, conversations are still captured and
encrypted, but never summarized — retrieval falls back to raw-episode
search only, which the system is not primarily designed around. This is
the single most important background job to get running correctly.

`hupi-selfcheck` needs a `probes.yaml` you write by hand — a small,
fixed list of "does memory still know this" questions. See the example
`probes.yaml` in the repo root for the shape.

## Upgrading an existing (pre-Tier-3) deployment

If you already had HUPI running before the Tier 3/hardening work — i.e.
you have an existing `hupi.dek.wrapped` file and a single-scope database
— the same Step 2-6 procedure applies, plus:

- Keep `HUPI_WRAPPED_DEK_PATH` pointed at your existing file (default
  `hupi.dek.wrapped` in the working directory) through the first start
  after upgrading. `bootstrap.Load` detects there's no `scope_keys` row
  yet for the default identity, reads that file, and migrates its
  contents in as `user:default`'s key — verbatim, no re-encryption, so
  none of your existing data needs to change.
- After that first successful start, the file is no longer read (every
  scope's key now lives in the `scope_keys` table) — you can archive it
  as a backup but the running system doesn't need it anymore.

## Containerized deployment

One image (`sujithsamuel/hupi` on Docker Hub, or build your own with
`docker build -t your-repo/hupi .`) contains every HUPI binary — the
gateway is its default `ENTRYPOINT`; everything else
(`hupi-consolidate`, `hupi-selfcheck`, `hupi-export`, `hupi-rotate-key`,
etc.) runs from the same image via a `command:` override, so there's one
thing to build, version, and scan, not eleven. See the
[Dockerfile](../Dockerfile)'s own comments for the build.

As with the bare-metal path, Postgres is bring-your-own — nothing here
bundles a database. TLS is also bring-your-own: the manifests and chart
deliberately have no TLS/certificate configuration, on the assumption
your ingress controller already terminates TLS in front of the Service.

**Plain manifests** — [deploy/k8s/](../deploy/k8s/), numbered in
apply order:

```bash
kubectl apply -f deploy/k8s/00-configmap-providers.yaml
# copy the two *.example.yaml Secrets, fill in real values, apply those
# (never commit the filled-in versions)
cp deploy/k8s/01-secret-admin-db.example.yaml deploy/k8s/01-secret-admin-db.yaml
cp deploy/k8s/02-secret-app.example.yaml deploy/k8s/02-secret-app.yaml
# edit both, then:
kubectl apply -f deploy/k8s/01-secret-admin-db.yaml
kubectl apply -f deploy/k8s/02-secret-app.yaml
kubectl apply -f deploy/k8s/03-migrate-job.yaml
kubectl wait --for=condition=complete job/hupi-migrate --timeout=120s
kubectl apply -f deploy/k8s/04-deployment.yaml -f deploy/k8s/05-service.yaml -f deploy/k8s/06-ingress.yaml
# write your own probes (see probes.yaml at the repo root for the shape), then:
cp deploy/k8s/07-configmap-probes.example.yaml deploy/k8s/07-configmap-probes.yaml
kubectl apply -f deploy/k8s/07-configmap-probes.yaml -f deploy/k8s/08-cronjob-selfcheck.yaml -f deploy/k8s/09-cronjob-consolidate.yaml
# optional — the admin UI (docs/ADMIN_UI.md); no Ingress by default, reach
# it with `kubectl port-forward svc/hupi-admin-ui 8788:80`
kubectl apply -f deploy/k8s/10-admin-ui-deployment.yaml -f deploy/k8s/11-admin-ui-service.yaml
```

`01-secret-admin-db.yaml` and `02-secret-app.yaml` intentionally hold
different-privilege Postgres roles — the migrate Job runs as the
superuser/owner (creates roles/extensions/tables), the gateway and
CronJobs run as `hupi_app` (RLS-scoped) — matching `install.sh`'s Step 2
vs. Step 6 split. Migrations are idempotent (`schema/migrate.sh`), so
re-running the Job on every deploy is safe.

**Helm chart** — [deploy/helm/hupi/](../deploy/helm/hupi/), same
resources, parameterized via `values.yaml`:

```bash
helm install hupi deploy/helm/hupi \
  --set secrets.app.existingSecretName=hupi-app \
  --set secrets.adminDB.existingSecretName=hupi-admin-db \
  --set ingress.host=hupi.example.com
```

By default the chart expects those two Secrets to already exist (see
`values.yaml`'s comments for why — Helm release history keeps values in
plaintext, so anything holding real credentials is better pre-created
and referenced by name); set `secrets.app.create=true` /
`secrets.adminDB.create=true` with inline values if that trade-off is
acceptable for your environment (fine for a quick try, not recommended
otherwise). Migration runs automatically as a `pre-install,pre-upgrade`
hook. `adminUI.enabled` and `selfcheck.enabled` (on by default, ships a
placeholder probe you must replace — see `values.yaml`) gate their
resources. Run `helm template` or `helm install --dry-run` first to
review what it would create.

## Troubleshooting

| Symptom | Cause |
|---|---|
| Gateway logs `HUPI_APP_DATABASE_URL not set, falling back to HUPI_DATABASE_URL` | You set `HUPI_DATABASE_URL` instead of `HUPI_APP_DATABASE_URL`. It'll still run, but row-level security won't actually be enforced if that fallback connection isn't the `hupi_app` role. |
| Any binary exits immediately with `required environment variable ... is not set` | Check `HUPI_APP_DATABASE_URL`/`HUPI_DATABASE_URL` and `HUPI_KEK` are exported in the shell/cron environment actually running the binary, not just your login shell. |
| `read providers.yaml: no such file or directory` | Either run the binary from the directory containing `providers.yaml`, or set `HUPI_PROVIDERS_CONFIG` to its absolute path. |
| Team route always returns `403` | `HUPI_REQUIRE_AUTH` isn't set to `true`, or the requesting user genuinely isn't a member of that team (`hupi-admin add-member`). |
| `insert into entities ... new row violates row-level security policy` from your own tooling | Something is writing to Postgres directly instead of through the application — everything that touches `episodes`/`summaries`/`entities` must go through a scoped transaction (`internal/dbscope`), including any ad hoc scripts you write yourself. |

## What this document doesn't cover

- TLS termination in front of the gateway — not built into `cmd/hupi`
  itself; put a reverse proxy (bare-metal) or ingress controller
  (Kubernetes) in front of it if you need HTTPS.
- Running Postgres itself in Kubernetes — the chart and manifests expect
  an external instance, same bring-your-own-database philosophy as the
  bare-metal path.
- Backups — this document doesn't prescribe a Postgres backup strategy;
  use whatever you already trust for your database, and back up `HUPI_KEK`
  with equal or greater care than the database itself.
