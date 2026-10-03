# Dashboard

`cmd/hupi-dashboard` is a read-only analytics surface over one scope's
own memory: conversation volume, a theme word cloud, entity
relationships, memory health, retrieval governance transparency, and a
security posture panel. It answers the questions users actually ask about
a memory system: what themes come up, how much history exists, whether
keyword search is still running at full strength for this scope, and
whether the deployment looks secure (key rotation status, recent
export/import/correction events).

## Tier 1/2 vs Tier 3

Tier 1/2 (this OSS build) needs no login at all — every request resolves
straight to `identity.DefaultScope`, the same zero-auth posture
`cmd/hupi`'s own gateway already has when `h.Auth` is `nil` (see
`internal/gateway/handler.go`'s `resolveIdentity`). There is only ever one
scope to look at, so there is nothing a login screen would protect.

Tier 3 is genuinely multi-tenant, so it adds real sign-in — password or
OIDC SSO — via `hupi-t3`'s `cmd/hupi-dashboard/team.go` overlay, the same
nil-by-default hook-var pattern `cmd/hupi-admin-ui/handlers.go` already
uses for `mountTeamRoutes`:

```go
var mountDashboardAuthRoutes func(mux *http.ServeMux, s *server) // nil in OSS
var resolveDashboardSession  func(r *http.Request) (identity.Scope, bool) // nil in OSS
```

When both are `nil`, `cmd/hupi-dashboard/auth.go`'s `requireDashboardSession`
resolves every request to `identity.DefaultScope` with no header checked
at all. The frontend is one bundle for both tiers — it tries
`GET /api/whoami` unauthenticated first; a 401 (only possible on Tier 3)
shows the login screen, success goes straight to the dashboard. This
mirrors how `cmd/hupi-admin-ui`'s frontend already handles team-vs-solo
differences purely from what the API returns, with no separate frontend
build per tier.

### Login mechanics (Tier 3 only)

No cookies — the frontend holds an opaque bearer session token in
`sessionStorage` and sends `Authorization: Bearer <token>` on every
request, exactly how `cmd/hupi-admin-ui`'s frontend already holds its own
Basic Auth credential. This is deliberate: a bearer token set by this
SPA's own JavaScript is never auto-resent by the browser cross-site the
way a cookie or Basic Auth credential would be, so none of
`cmd/hupi-admin-ui`'s `Sec-Fetch-Site`-based CSRF mitigation is needed
here at all.

Two credential types converge on minting the same session token
(`dashboard_sessions`, `schema/0023_dashboard_sessions.sql`):

- **Password**: `POST /auth/login/password {username, password}` —
  bcrypt-verified against `users.password_hash`, set only via a new
  `hupi-admin set-password` subcommand (admin-driven, no self-service
  reset, matching `create-user`/`create-key`/`create-operator`'s existing
  admin-only provisioning model).
- **OIDC/SSO**: `GET /auth/login/oidc/start` redirects into a genuine
  Authorization Code + PKCE flow against the configured IdP;
  `/auth/login/oidc/callback` completes it. The JWT *verification* half
  reuses `internal/auth/oidc.go`'s existing issuer/audience/JWKS logic
  directly — only the redirect/callback flow-initiation code is new, since
  that package previously only verified an already-issued JWT passed as
  `Authorization: Bearer`.

## Panels and their data sources

Every Tier 1/2 panel reads only plaintext columns — no decryption
anywhere in `cmd/hupi-dashboard/queries.go`:

| Panel | Source |
|---|---|
| Conversation volume | `episodes.ts`/`.type` |
| Theme word cloud | `entities.name`/`.kind` × `summaries.entities_touched` |
| Entity relationships | `entity_relationships` (all columns plaintext) |
| Memory health | `scope_corpus_size`, `summaries.created_at`/`.correction_reason` |
| Retrieval governance | `scope_corpus_size` + the same thresholds `internal/store/retrieve.go`'s `keywordSearchTierForScope` uses |
| Security posture | `key_rotations`, `audit_log` (export/import/key_rotation/correct/dashboard_login events) |

`audit_log` has **no row-level-security restriction on `SELECT`** (its
own migration's comment: "audit_log's entire purpose is cross-scope
visibility for admin tools") — `securityPosture`'s call to
`audit.Query` always passes an explicit `ScopeKind`/`ScopeOwner` filter;
never call it without both, or a Tier 3 user would see every other user's
audit trail. `hupi_app` also has no `DELETE` grant on `audit_log` at all
(append-only by design) — relevant if you're ever cleaning up test data
against a real database, not just this package's own tests.

## Phase 2 — decrypt-on-view themes (not yet built)

A deliberately separate, opt-in feature: real topic extraction over
decrypted conversation content, gated by
`HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS` (default `false`, same honest
opt-in convention as `HUPI_ENABLE_KEYWORD_SEARCH`). Decrypts episodes/
summaries for the requested window via the scope's `KeyStore` (the same
pattern `internal/store/retrieve.go` already uses), runs local TF-IDF/
keyword-frequency extraction in Go — no LLM call, decrypted content never
leaves the server process — and returns only the aggregated term list,
never raw text, never cached. A second, separately-gated toggle
(`HUPI_ENABLE_DASHBOARD_LLM_THEMES`) will later add an LLM-powered
narrative summary on top, reusing the same provider registry retrieval/
consolidation already trust with plaintext.

## Env vars

| Var | Meaning | Default |
|---|---|---|
| `HUPI_DASHBOARD_LISTEN_ADDR` | Listen address | `127.0.0.1:8790` |

Same bind-to-localhost posture as `cmd/hupi-admin-ui` — this reads real
data about one scope's memory and should sit behind a trusted user's own
machine or a reverse proxy, not be exposed directly to the internet.
