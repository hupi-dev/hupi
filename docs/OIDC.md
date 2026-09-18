# OIDC / SSO authentication for Tier 3

Status: **built and verified** — real Postgres JIT-provisioning tests
(`internal/auth/oidc_test.go`, `hupi-t3`) plus two live end-to-end runs
against a real Azure AD (Entra ID) tenant: first, three test identities
(single-team, multi-team, and no-team) each produced a correctly-scoped
`identity.Identity` via directly-obtained tokens, with the negative cases
(wrong audience, expired token, malformed token) confirmed to fail
closed; second, a full interactive sign-in through the VS Code extension
(see [VSCODE_EXTENSION.md](VSCODE_EXTENSION.md)) — real browser login,
real loopback redirect, real JIT provisioning observed in Postgres, real
chat response back. Two real configuration gotchas surfaced during that
second run and are called out inline below (§ Setting it up against
Azure AD) rather than left as something the next person rediscovers the
hard way.

This reverses [TIER3_PLAN.md](TIER3_PLAN.md)'s original non-goal —
*"No SSO/OIDC. Federated identity is a later concern, layered on top of
the same `users` table."* — now that it's exactly that: a second way to
resolve a bearer token into an `identity.Identity`, layered on top of the
same `users`/`teams` tables Tier 3 already had, not a redesign of them.

Like the rest of Tier 3, this is part of the private, commercially-licensed
`hupi-t3` extension — see [ARCHITECTURE.md § Licensing and the open-core
split](../ARCHITECTURE.md) for why. This doc explains what it does and
how it's configured; the implementation lives in `hupi-t3`.

## Why OIDC, and why it changes nothing in the free core

Tier 3 authentication was originally just a bearer API key
(`hupi_sk_...`) hashed and looked up against a `users` row — every
identity had to be provisioned by hand via `hupi-admin`. OIDC support
lets a real identity provider (Azure AD / Entra ID, Okta, Auth0, Google
Workspace — anything that speaks standard OpenID Connect) authenticate
users and define team membership instead, so team membership changes in
your existing directory rather than requiring a separate
`hupi-admin add-member` for every change.

This was possible with zero changes to how the public `hupi` repo's
gateway resolves identity, because `gateway.Authenticator`'s interface
was already exactly the right shape:

```go
type Authenticator interface {
    Resolve(ctx context.Context, apiKey string) (identity.Identity, error)
}
```

`Resolve` takes an opaque bearer-token string and returns an `Identity`.
It doesn't care whether that string was a `hupi_sk_...` key or an
Azure-AD-issued JWT — OIDC is a second implementation of the same
interface, not a fork of it. A small composite authenticator (in
`hupi-t3`) looks at a token's shape — three dot-separated segments means
JWT, anything else falls through to the existing hash lookup — and
dispatches accordingly. Nothing above that point (gateway routing, audit
logging, `resolveTeamScope`) needs to know or care which path a given
request took.

## How verification works

At startup, if OIDC is configured, `hupi-t3` fetches the identity
provider's discovery document once:

```
GET {issuer}/.well-known/openid-configuration
```

That document hands back the provider's `jwks_uri` — where its public
signing keys live. From then on, verifying a token is pure local
cryptography, no live call to the IdP per request: the token's signature
is checked against the cached public key matching its `kid` header
(refreshed automatically on a `kid` miss, i.e. a key rotation), and three
more standard checks run in the same pass:

- **Issuer (`iss`)** must match the discovery document's issuer exactly —
  rejects a token from a different tenant or provider.
- **Audience (`aud`)** must contain HUPI's own configured client ID —
  rejects a token that was issued for a *different* API and replayed
  against HUPI.
- **Expiry (`exp`)** — rejects anything past its lifetime.

One deliberate note for anyone extending this to another provider: OIDC
technically distinguishes an *ID token* (proves authentication happened,
meant for the client app) from an *access token* (meant to be presented
to an API — HUPI's case). Azure AD v2.0 access tokens are shaped
identically to ID tokens (signed JWTs with the same standard claims), so
the same verifier works correctly as long as the configured client ID is
HUPI's own application ID (the expected audience), not the ID of
whatever client obtained the token.

## Claims → Identity mapping

Once a token verifies, two claims become the `Identity`:

- **Subject** — Azure AD's `oid` claim (a stable per-tenant user
  identifier) if present, otherwise the standard OIDC `sub` claim →
  `user:oidc:<id>`.
- **Team membership** — whichever claim `HUPI_OIDC_TEAMS_CLAIM` names
  (default `"roles"`, since Azure AD App Roles land in an access token's
  `roles` array) → each value becomes `team:oidc:<value>`.

Neither of those is an OIDC standard requirement — `roles` is an Azure AD
convention for App Roles, not a spec-defined claim, which is why it's
configurable rather than hardcoded. A provider that puts group membership
in a `groups` claim instead just needs `HUPI_OIDC_TEAMS_CLAIM=groups`, no
code change.

The first time a given user or team is seen, `hupi-t3` JIT-provisions it
through the *same* `auth.Store.CreateUser`/`CreateTeam` calls
`hupi-admin` uses — same idempotent insert, same eager encryption-key
provisioning. OIDC supplies *who* and *which teams*; the local
`users`/`teams` rows (and their per-scope encryption keys) are still the
one source of truth everything else in HUPI reads from.

## Configuration

All optional — set none of them and Tier 3 behaves exactly as before
(API keys only):

| Variable | Meaning | Default |
|---|---|---|
| `HUPI_OIDC_ISSUER_URL` | OIDC discovery base URL, e.g. `https://login.microsoftonline.com/<tenant-id>/v2.0` | — (required to enable OIDC) |
| `HUPI_OIDC_CLIENT_ID` | Expected token audience — HUPI's own application/client ID registered with the IdP | — (required to enable OIDC) |
| `HUPI_OIDC_TEAMS_CLAIM` | Which claim carries team membership | `roles` |
| `HUPI_OIDC_TEAM_PREFIX` | How a claim value becomes a HUPI team id | `team:oidc:` |

Both `HUPI_OIDC_ISSUER_URL` and `HUPI_OIDC_CLIENT_ID` must be set to turn
OIDC on; if discovery fails at startup (network issue, bad issuer URL),
`hupi-t3` logs it loudly and falls back to API-key-only rather than
refusing to start — a token shaped like a JWT then correctly fails the
existing hash lookup instead of being silently accepted.

## Setting it up against Azure AD (Entra ID)

1. **Entra ID → App registrations → New registration.** Single tenant,
   no redirect URI needed — this is a resource API, not an interactive
   client. Note the **Application (client) ID** and **Directory (tenant)
   ID**.
2. **Set the resource app's accepted token version to 2 — easy to miss,
   and the tokens still *look* valid without it.** App registration →
   **Manifest** → find `"api": { "requestedAccessTokenVersion": null,
   ... }` → change `null` to `2` → Save. Left at the default `null`, Azure
   AD issues *v1*-format access tokens for this app's audience (issuer
   `https://sts.windows.net/<tenant>/`) even when a client requests them
   through the `/v2.0` endpoint — token version is controlled by the
   *resource* app's manifest, not by which endpoint the client used. Since
   `HUPI_OIDC_ISSUER_URL` here is the v2.0 issuer
   (`https://login.microsoftonline.com/<tenant>/v2.0`), a v1 token fails
   verification with `oidc: id token issued by a different provider` —
   a real error this project hit during its own VS Code extension testing,
   only visible server-side after [handler.go's resolveIdentity started
   logging the underlying rejection reason](../internal/gateway/handler.go)
   (it was previously swallowed into a generic 401, by design, so an
   attacker can't probe rejection reasons — but that also hid this from
   whoever's setting it up).
3. **App registration → App roles → Create app role**, once per team
   (e.g. `Team.AcmeEng`, `Team.Research`), member type "Users/Groups."
4. **Enterprise Applications → find your app → Users and groups → Add
   assignment** to put real users in one or more roles.
5. Set:
   ```bash
   export HUPI_OIDC_ISSUER_URL="https://login.microsoftonline.com/<tenant-id>/v2.0"
   export HUPI_OIDC_CLIENT_ID="<application-client-id>"
   ```
6. A client authenticates against Azure AD however it normally would
   (device code, auth-code flow, client credentials, etc.) and presents
   the resulting access token as `Authorization: Bearer <token>` to
   HUPI, same as an API key would be. For the VS Code extension
   specifically, see [VSCODE_EXTENSION.md](VSCODE_EXTENSION.md) — it
   needs a *second*, separate app registration (a public client that
   signs users in) plus its own redirect-URI gotcha.

Any standards-compliant OIDC provider works the same way — Azure AD is
just the one this was built and tested against; the only Azure-specific
default is `HUPI_OIDC_TEAMS_CLAIM=roles`, and even that's a config value,
not a code path.

## Known limitations

- **No instant revocation.** Removing a user from an app role in Azure AD
  doesn't invalidate a token they already hold — it stays valid until
  natural expiry (typically well under two hours). This is inherent to
  stateless JWT verification; closing it fully would mean a live
  revocation check against the IdP on every request, a latency/complexity
  tradeoff not taken in this version.
- **JIT provisioning trusts the token.** Any team referenced in a valid
  token's claims becomes a real, live HUPI team the first time it's seen
  — there's no admin-approval step in between. This is a deliberate
  tradeoff for fast setup; an explicit admin-approved mapping step is a
  reasonable follow-up for a production rollout, not something this
  version enforces.
