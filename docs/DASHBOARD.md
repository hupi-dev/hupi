# Dashboard

`cmd/hupi-dashboard` is a read-only analytics surface over one scope's
own memory: conversation volume, a theme word cloud, entity
relationships, memory health, retrieval governance transparency, a
security posture panel, and a Memory Map — an interactive graph of
conversations, entities, relationships, and topics (see "Memory Map"
below). It answers the questions users actually ask about
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
  `username` is the user's own id (e.g. `user:alice`, not a separate
  username field — `users` has none), bcrypt-verified via
  `internal/auth.Store.VerifyPassword` against `users.password_hash`,
  set only via `hupi-admin set-password -user <id>` (admin-driven, no
  self-service reset, matching `create-user`/`create-key`/
  `create-operator`'s existing admin-only provisioning model —
  `set-password` itself lives in the **public** repo's `cmd/hupi-admin`
  since the underlying operation needs nothing team-specific, unlike
  `add-member`/`create-key`).
- **OIDC/SSO**: `GET /auth/login/oidc/start` redirects into a genuine
  Authorization Code + PKCE flow against the configured IdP;
  `/auth/login/oidc/callback` completes it, then redirects back to `/`
  with the minted session token in a one-time `?dashboard_token=...`
  query param — `App.tsx`'s `consumeOidcCallbackToken` reads it, saves it,
  and strips it from the URL on mount. The JWT *verification* half reuses
  `internal/auth/oidc.go`'s existing issuer/audience/JWKS logic directly —
  only the redirect/callback flow-initiation code is new, since that
  package previously only verified an already-issued JWT passed as
  `Authorization: Bearer`.

### Workspace routing (Tier 3 only)

`dashboard_sessions` is keyed on `user_id` alone, not a fixed
`scope_kind`/`scope_owner` pair — which workspace a request actually
reads is resolved fresh on every request, not baked in at login time. No
`X-Hupi-Workspace` header means the signed-in user's own private scope;
naming a team id resolves to that team's shared scope only after a live
`team_members` check (never cached), so a membership change takes effect
on the very next request rather than whenever the session happens to be
re-minted. A request naming a team the user isn't actually a member of is
rejected outright (401) — not a silent fallback to their private scope,
which would hide the caller's own mistake. This is what makes panels 1-7
genuinely "team-wide" without needing a second, shared-scope-specific
copy of each query: the frontend's workspace selector (`Dashboard.tsx`)
just sets the header and reloads, and every existing panel reads whichever
scope the header resolved to. `GET /api/workspaces` lists the teams a
signed-in user can switch into.

### Admin security posture (Tier 3 only)

`GET /api/admin/security-posture` — stale API keys (`last_used_at` older
than 90 days or never set), key rotation status across every scope, and
a 24-hour failed-dashboard-login count — is gated behind the same named
*operator* Basic Auth `cmd/hupi-admin-ui` uses (`auth.Store.ResolveOperator`),
not a regular dashboard session: this view spans every user/team in the
deployment (`api_keys`/`key_rotations`/`users` carry no row-level
security at all — confirmed against `schema/*.sql`), which is an operator
concern, not something any one signed-in user should see regardless of
their own team memberships.

## Panels and their data sources

Every Tier 1/2 panel reads only plaintext columns — no decryption
anywhere in `cmd/hupi-dashboard/queries.go`:

| Panel | Source |
|---|---|
| Conversation volume | `episodes.ts`/`.type` |
| Theme word cloud | `entities.name`/`.kind` × `summaries.entities_touched` |
| Entity relationships | `entity_relationships` (all columns plaintext) |
| Memory health | `scope_corpus_size`, `summaries.created_at`/`.correction_reason` |
| Forgotten but important | `episodes.importance`/`.ts` (stale + high-importance), `entities.last_updated` (stale) |
| Retrieval governance | `scope_corpus_size` + the same thresholds `internal/store/retrieve.go`'s `keywordSearchTierForScope` uses |
| Security posture | `key_rotations`, `audit_log` (export/import/key_rotation/correct/dashboard_login events) |
| Export | `GET /api/export` — not a read-only plaintext panel like the rest, this one decrypts: it reuses `internal/store.Store.ExportMemory` exactly as `cmd/hupi-export-memory` does (same `store.New(db, keys, embedder)` construction), which already writes its own `audit_log` entry |
| Memory Map (graph) | `GET /api/memory-map` — `episodes.ts`/`.importance`, `entity_relationships` joined to `entities.name`/`.kind`, and the owning daily summary's `entities_touched` for conversation→entity mention edges. Its topics overlay is decrypt-gated — see "Memory Map" below |

`audit_log` has **no row-level-security restriction on `SELECT`** (its
own migration's comment: "audit_log's entire purpose is cross-scope
visibility for admin tools") — `securityPosture`'s call to
`audit.Query` always passes an explicit `ScopeKind`/`ScopeOwner` filter;
never call it without both, or a Tier 3 user would see every other user's
audit trail. `hupi_app` also has no `DELETE` grant on `audit_log` at all
(append-only by design) — relevant if you're ever cleaning up test data
against a real database, not just this package's own tests.

## Memory Map

A "Memory Map" tab next to the panel overview renders one scope's memory
as an interactive [cytoscape.js](https://js.cytoscape.org/) graph:
entities (circles, colored by `entities.kind`, sized by edge count),
conversations (squares), topics (diamonds), relationship edges
(entity → entity, labeled with the predicate), mention edges
(conversation → entity), and `tagged_with` edges (conversation → topic).
Three layouts: Network (`cytoscape-fcose` force-directed), Timeline
(fcose with conversation nodes pinned left-to-right by timestamp), and
Circular (cytoscape's built-in `concentric`, most-connected nodes toward
the center). Clicking a node opens a detail panel built from the
already-fetched data — no extra request.

It's split into two endpoints by cost profile, the same split as
`/api/entity-relationships` (plaintext, always on) vs.
`/api/content-themes` (decrypt-gated):

- **`GET /api/memory-map`** — plaintext only, always on. Entity and
  conversation nodes, relationship and mention edges
  (`cmd/hupi-dashboard/memory_map.go`'s `handleMemoryMap`, built on
  `queries.go`'s `conversationsInRange`, `entityRelationshipGraphInRange`,
  and `conversationEntityMentions`). Relationships are filtered by their
  own bi-temporal validity window (`valid_from`/`valid_until` overlapping
  the requested range, nulls treated as open-ended), not by when they were
  recorded — a long-valid fact doesn't disappear just because the view is
  zoomed into a narrow recent window. Mention edges come from the owning
  **daily** summary's `entities_touched`, the only plaintext link from an
  individual episode to an entity (weekly/monthly/yearly summaries don't
  carry `source_episode_ids`).
- **`GET /api/memory-map/topics`** — gated by the same
  `HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS` flag as `/api/content-themes`
  (returns `{"enabled": false}` when off; the graph still renders, just
  without topics). Decrypts the same conversation set the graph endpoint
  returns (`decryptConversations`), runs the same local keyword extraction
  per conversation, and aggregates the results into topic nodes. **One
  deliberate exception** to Phase 2's "never returns raw text" rule
  below: each conversation gets a short excerpt (first 280 characters)
  of its own decrypted text, shown in the detail panel. A term must occur
  at least twice across the whole range to become a topic node, so
  one-off words on short conversations don't clutter the graph.

Both take `?days=` (default 30) or explicit `?from=YYYY-MM-DD&to=YYYY-MM-DD`
(inclusive start, inclusive end day). Defaults are reasoned, not measured:
the graph endpoint caps at 500 conversations and 500 relationships
(`?max_conversations=`/`?max_relationships=`); the topics endpoint caps at
150 conversations, 2,000 decrypted characters per conversation, and
200,000 per request — the per-conversation cap stops one very long
conversation from consuming the whole budget. Either endpoint sets
`truncated: true` when a cap was hit, and the frontend shows a "narrow the
date range" banner; separately, the frontend declines to lay out more
than 1,500 elements at once rather than stall the browser. Nothing is
cached, matching Phase 2's existing posture.

## Phase 2 — decrypt-on-view themes

A deliberately separate, opt-in feature (`cmd/hupi-dashboard/content_analysis.go`):
real topic extraction over decrypted conversation content, the one place
in this product where plaintext touches a request/response path outside
the retrieve-or-consolidate-then-reencrypt loop.

- **`GET /api/content-themes`** (2a) — gated by
  `HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS` (default `false`, same honest
  opt-in convention as `HUPI_ENABLE_KEYWORD_SEARCH`). Decrypts episode
  input/output text and current-summary prose for the requested window
  via the scope's `KeyStore` (the same per-row `key_version` → `GetVersion`
  pattern `internal/store/retrieve.go` already uses), runs local
  keyword-frequency extraction in Go — no LLM call, decrypted content
  never leaves the server process — and returns only the aggregated term
  list, never raw text, never cached. When the flag is off, returns
  `{"enabled": false}` (not an error), so the frontend can treat it as a
  normal, hideable state.
- **`GET /api/content-themes/narrative`** (2b) — a separate toggle,
  `HUPI_ENABLE_DASHBOARD_LLM_THEMES` (also default `false`), decrypting
  the same window and asking `internal/provider.Registry.Chat()` (the
  same provider profile retrieval/consolidation already trust with
  plaintext) for a short narrative paragraph instead of a bare term list.

Both decrypt via `decryptRecentText`, bounded by
`defaultContentAnalysisMaxChars` (200,000 characters, reasoned not
measured) so one request can't decrypt an entire unbounded scope's
history — this is a display feature, not a retrieval path.

## Env vars

| Var | Meaning | Default |
|---|---|---|
| `HUPI_DASHBOARD_LISTEN_ADDR` | Listen address | `127.0.0.1:8790` |
| `HUPI_ENABLE_DASHBOARD_CONTENT_ANALYSIS` | Phase 2a local keyword themes, and the Memory Map's topics/excerpts overlay | `false` |
| `HUPI_ENABLE_DASHBOARD_LLM_THEMES` | Phase 2b LLM narrative themes | `false` |

Same bind-to-localhost posture as `cmd/hupi-admin-ui` — this reads real
data about one scope's memory and should sit behind a trusted user's own
machine or a reverse proxy, not be exposed directly to the internet.
