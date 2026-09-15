# Admin UI

`cmd/hupi-admin-ui` is a JSON API server over Tier 3 provisioning — the
browser-reachable equivalent of `cmd/hupi-admin`, the CLI described in
[TIER3_PLAN.md](TIER3_PLAN.md) Phase 6. It used to render server-side
HTML directly (`html/template`); it was rewritten to a pure JSON API so a
separate React single-page app (a later, separate piece of work) can call
it instead. See "What it looks like" below for the current state of that
frontend (not yet built).

## Why this exists despite the original non-goal

`TIER3_PLAN.md`'s non-goals list originally said: *"no team-management
UI. Team/membership changes are direct DB writes or a thin CLI in this
plan, not a product surface."* That was the right call at the time — Tier
3 needed to prove out identity, scoping, and RLS correctness first, and a
UI on top of an unstable data model would have been wasted work. Once
that foundation was built and hardened (see HARDENING_PLAN.md), the
tradeoff changed: the CLI works but requires shell access to wherever
`hupi-admin` runs, which doesn't fit every operator's workflow (e.g.
someone provisioning teams who isn't also an infrastructure operator).
This document — and `cmd/hupi-admin-ui` — is that reversal, made
explicitly rather than by quietly shipping a UI and leaving the non-goal
looking still-true. `TIER3_PLAN.md` itself is left unedited except for a
pointer here, since it's a record of a decision made at a point in time,
not a living spec.

The CLI is not deprecated or replaced. Both talk to the same
`internal/auth.Store`; either works standalone.

## Scope

**In scope**: everything `cmd/hupi-admin` already does (create user,
create team, add member, issue an API key), plus the read/revoke
operations a UI needs that a create-only CLI never did:

- List users and teams
- View a user's team memberships and API keys
- View a team's member list
- Revoke an API key
- List, create, and revoke admin-UI operator credentials (previously only
  reachable via `hupi-admin create-operator`/`revoke-operator`)
- Query the `audit_log` table (`hupi-audit`'s job until now) over HTTP,
  filtered the same way `hupi-audit query` is

**Deliberately out of scope**:

- **Memory content.** The admin UI never queries `episodes`, `summaries`,
  or `entities`. `hupi-trace` — a CLI tool, unchanged — is still the only
  way to inspect what was actually retrieved or stored for a request.
  Keeping that CLI-only is a deliberate line, not an oversight: a
  provisioning surface that made it one click away from reading someone's
  private conversation history would change what kind of tool this is.
- **Billing/quota enforcement** — still not built, in either form, per
  `TIER3_PLAN.md`'s non-goals.
- **Key rotation** — still not built; see `BUSINESS_PROCESS.md` §12 and
  [GAP_CLOSURE_PLAN.md §4.4](GAP_CLOSURE_PLAN.md) (planned).
- **Self-service.** There is no signup, no invite flow, no
  user-facing password reset. This is an operator tool for whoever runs
  `hupi-admin`/`hupi-admin-ui` today, not a customer-facing surface.

## Authentication and exposure model

Each operator has their own named credential — a row in the `operators`
table (`schema/0008_admin_operators.sql`), provisioned with
`hupi-admin create-operator -name <name>`, checked via HTTP Basic Auth
(the password field carries the raw token; `auth.Store.ResolveOperator`
does the hash lookup, same pattern `api_keys`/`Resolve` already uses for
end users). This replaced an earlier single shared
`HUPI_ADMIN_UI_TOKEN` — see [GAP_CLOSURE_PLAN.md §4.3](GAP_CLOSURE_PLAN.md)
for why: the audit log (below) records who did what, which is meaningless
if every operator is an indistinguishable holder of the same token.
There's still no session store — every request re-authenticates via
Basic Auth, which is what lets a stateless audit hook read the
authenticated operator straight off the request context on every call.

Every view and provisioning action here writes an `audit_log` row
(`event_type` `admin_ui_view` or `admin_provision`, `actor` = the
resolved operator's name) — see
[GAP_CLOSURE_PLAN.md §4.3](GAP_CLOSURE_PLAN.md) and query it with
`hupi-audit`. `hupi-admin`'s own subcommands log the same way, attributed
to whoever the `-actor` flag (default `$USER`) says ran them.

Like `cmd/hupi`, it binds to `127.0.0.1` by default
(`HUPI_ADMIN_UI_LISTEN_ADDR` to change that) — it is not designed to be
placed directly on the open internet. If you need remote access, put it
behind a reverse proxy or VPN you already trust, the same guidance
`INSTALL.md` gives for the gateway's TLS story.

**Basic Auth's CSRF gap, and the mitigation**: browsers cache Basic Auth
credentials per origin and resend them automatically on same-origin
requests — including ones triggered by a form on a *different* site,
since there's no session cookie to scope a token to. `hupi-admin-ui`
rejects any non-GET request whose `Sec-Fetch-Site` header (sent by all
current browsers) is present and not `same-origin`. This is
defense-in-depth, not a substitute for the exposure model above — a
client that doesn't send `Sec-Fetch-Site` (older browsers, plain `curl`)
isn't blocked by it.

## Running it

`./install.sh --tier=3` builds and configures this automatically
(`--admin-ui`/`--no-admin-ui` to override the tier-3 default) — see
[INSTALL.md](INSTALL.md). The rest of this section is what that
automates, for running it by hand.

Same environment as every other binary (`HUPI_APP_DATABASE_URL`,
`HUPI_KEK`, `HUPI_PROVIDERS_CONFIG` — the UI doesn't call any LLM
provider itself, but `bootstrap.Load` wires up the registry regardless).
Provision at least one operator before starting it — there's no bootstrap
account:

```bash
go build -o bin/hupi-admin    ./cmd/hupi-admin
go build -o bin/hupi-admin-ui ./cmd/hupi-admin-ui
./bin/hupi-admin create-operator -name alice
# prints a raw token exactly once — save it now; it cannot be shown
# again, only revoked (hupi-admin revoke-operator, once that exists) and
# replaced with a new one
./bin/hupi-admin-ui
```

Then visit `http://127.0.0.1:8788/` and authenticate as `alice` with that
token as the password (the username is checked against the token's
resolved operator if you supply one, but isn't required to log in).

Treat every operator token with the same care as an API key — anyone
holding it can create users, teams, and API keys, revoke existing ones,
and everything they do is attributed to that operator's name in
`audit_log`. Revoke one with `hupi-admin revoke-operator -name alice`.

## What it looks like

`GET /` and everything not under `/api/` serves a React single-page app
(source in `cmd/hupi-admin-ui/web`, built with Vite/TypeScript/Tailwind)
embedded into the binary via `//go:embed web/dist`
(`cmd/hupi-admin-ui/assets.go`), with a server-side fallback to
`index.html` for any path that isn't a real static file, so React
Router's client-side routes resolve on a hard refresh (e.g. navigating
straight to `/users/alice`). Unlike everything under `/api/`, this
route is deliberately **not** behind `requireOperatorAuth` — the login
screen has to be reachable before there's a credential to send.

The app is a dark-surface admin console:

- **`/login`** — operator name + token. Verifies the credential against
  `GET /api/whoami` before storing anything, in `sessionStorage` (cleared
  when the tab closes — deliberately not `localStorage`).
- **`/`** — dashboard with searchable Users and Teams tabs, counts, and
  "new user"/"new team" modals.
- **`/users/:id`** — profile, team memberships, API keys table with
  revoke (inline confirm, not a browser `confirm()`), and "issue new
  key" — the raw key is shown exactly once in a modal with a
  copy-it-now warning, same posture as `hupi-admin`'s stdout output.
- **`/teams/:id`** — team info, member list, add-member form.
- **`/operators`** — list with active/revoked status, create (raw token
  shown once, same as an API key), revoke with inline confirm.
- **`/audit`** — a filter bar (scope kind/owner, actor, event type,
  since/until, limit) over `GET /api/audit`, results table with the raw
  `Detail`/`TargetRef` JSON text rendered as an expandable monospace
  blob rather than parsed into a bespoke view per event type.

Every screen has real loading/empty/error states and toast notifications
after actions, instead of the old server-rendered templates' no-feedback,
full-page-reload flow.

**Local frontend development**: `cd cmd/hupi-admin-ui/web && npm install
&& npm run dev` starts Vite's dev server with hot reload, proxying any
`/api/*` request to a real running `hupi-admin-ui` instance (default
`http://127.0.0.1:8788`; override with `HUPI_ADMIN_UI_DEV_PROXY_TARGET`)
— see `cmd/hupi-admin-ui/web/README.md`. This lets frontend iteration
happen without rebuilding the Go binary on every change.

`cmd/hupi-admin-ui/web/dist` (the npm build output the Go binary embeds)
is not source-controlled beyond a minimal placeholder `index.html`,
committed so a plain `go build ./cmd/hupi-admin-ui` never fails with a
`go:embed` error on a fresh clone that hasn't run the npm build yet.
`install.sh` and the Dockerfile both run `npm ci && npm run build`
automatically before compiling this binary — see
[INSTALL.md](INSTALL.md)'s Step 4 for the manual-build equivalent.

Everything else is a JSON API under `/api/`, behind the same
Basic-Auth-plus-CSRF-mitigation stack described above:

| Method | Path | Does |
|---|---|---|
| GET | `/api/users` | list all users |
| POST | `/api/users` | create a user (`{id, email}`), 201 |
| GET | `/api/users/{id}` | one user bundled with their teams and API keys; 404 if unknown |
| POST | `/api/users/{id}/keys` | issue a new API key, returns `{"raw_key": "..."}` once |
| POST | `/api/keys/revoke` | revoke a key (`{key_hash, user_id}`) |
| GET | `/api/teams` | list all teams |
| POST | `/api/teams` | create a team (`{id, name}`), 201 |
| GET | `/api/teams/{id}` | one team bundled with its members; 404 if unknown |
| POST | `/api/teams/{id}/members` | add/update a member (`{user_id, role}`) |
| GET | `/api/operators` | list admin-UI operator credentials |
| POST | `/api/operators` | create an operator (`{name}`), returns `{"raw_token": "..."}` once, 201 |
| POST | `/api/operators/revoke` | revoke an operator (`{name}`) |
| GET | `/api/whoami` | `{"name": "..."}` — confirms the caller's own credentials still resolve |
| GET | `/api/audit` | query `audit_log` (`scope_kind`, `scope_owner`, `actor`, `event_type`, `since`, `until`, `limit` as query params) |

Every error response is `{"error": "<message>"}` with an appropriate
status code (400/404/500) — raw database errors are never reflected to
the client, only logged server-side, same posture as before this
rewrite. Key and operator-token creation still show the raw secret
exactly once, in that response body, with the same "save this now, it
cannot be shown again" guarantee as `hupi-admin` prints to stdout: only
the sha256 hash is ever persisted (`internal/auth.CreateAPIKey`,
`internal/auth.CreateOperator`).

## Verification

`cmd/hupi-admin-ui` has an automated test suite, split the same way the
rest of the project splits DB-dependent from pure tests:

- `middleware_test.go` — `requireCSRFSafe` in isolation, no database
  needed, always run as part of `go test ./...`. Covers every
  `Sec-Fetch-Site` case (`same-origin`/absent allowed,
  `cross-site`/`same-site`/`none` rejected on non-GET, GET always
  allowed).
- `handlers_test.go` — integration tests against a real Postgres instance
  (same `HUPI_TEST_DATABASE_URL` convention as
  `internal/store/scope_isolation_test.go` and `internal/auth/auth_test.go`;
  self-skips, not fails, when that variable isn't set). Drives the actual
  `srv.routes()` handler with `httptest`, decoding JSON responses instead
  of scraping rendered HTML: full create-user → issue-key → revoke
  lifecycle (including that a revoked key stops resolving), create-team →
  add-member, 404s for unknown users/teams (asserting the `{"error": ...}`
  body shape), 400s for missing required JSON fields, a round-trip test
  confirming operator-supplied text (a user's email) comes back
  byte-for-byte unmodified (the JSON-API replacement for the old
  HTML-escaping regression test — there's no escaping step left to
  regress), `TestRequireOperatorAuth` (correct/wrong/revoked token,
  username-mismatch safety check, missing header), operator
  create/list/revoke lifecycle coverage, `TestAuditQuery` (seeds audit
  events under two different actors and confirms an actor filter excludes
  the other actor's rows, not just that an unfiltered call returns
  everything), and `TestAdminUIActions_AreAudited` (confirms a view and a
  provisioning action each write the expected `audit_log` row, attributed
  to the authenticated operator).

Run it the same way as the rest of the suite:

```bash
HUPI_TEST_DATABASE_URL='postgres://hupi_app:hupi@localhost:15432/hupi?sslmode=disable' \
  go test -count=1 ./cmd/hupi-admin-ui/...
```

Connect as `hupi_app`, not `postgres` — some tests elsewhere in the suite
(`internal/store/rls_test.go`) specifically verify RLS enforcement, which
is bypassed for the table owner and superusers by design
(`schema/0004`'s own comment); a superuser connection would make those
checks pass for the wrong reason.

`-count=1` matters here for the same reason it does elsewhere in this
project (see `HARDENING_PLAN.md`) — it forces genuine re-execution against
the live database instead of a cached pass.
