# HUPI for VS Code

A VS Code extension: a chat sidebar and an inline-edit command (Cmd+K
style), both backed by your own [HUPI](../README.md) gateway instead of
calling an LLM vendor directly — the same idea as Cursor/Continue/Cody, but
pointed at a memory-aware, self-hosted backend you control.

This is a plain VS Code extension built on the public extension API — not a
fork of VS Code itself. See [docs/BUSINESS_PROCESS.md](../docs/BUSINESS_PROCESS.md)
for what HUPI actually does underneath (memory capture, grounding,
retrieval); this extension is just a client of HUPI's OpenAI-compatible
`/v1/chat/completions` endpoint, using the official `openai` npm SDK.

**Published**: search "HUPI" in VS Code's Extensions view, or install from
https://marketplace.visualstudio.com/items?itemName=hupi.hupi-vscode.

## What's included (v1)

- **Chat sidebar** — a HUPI icon in the activity bar opens a chat panel.
  Every message automatically includes the active file (or your current
  selection) as context, the same way Cursor's chat references open files.
- **`@hupi` in VS Code's native Chat view** — HUPI is also registered as a
  chat participant (`vscode.chat.createChatParticipant`), so `@hupi <message>`
  works from the same Chat view other participants (including GitHub
  Copilot Chat, if installed) use. Marked sticky, so once a conversation
  starts with `@hupi` it stays routed there without re-typing the mention
  each turn. This is additional, not a replacement for the sidebar above —
  a third-party participant can't be made that view's default, unqualified
  handler, so the two cover different moments (always-visible vs. inside
  the shared Chat view alongside other participants/tools).
- **Inline edit** (`Ctrl+K` / `Cmd+K` with a selection) — describe a change,
  HUPI rewrites the selected code, you get a diff preview before anything
  is applied to your file.

Not included in v1 (by explicit scope decision, not an oversight): inline
autocomplete/Tab-style ghost text, and agentic multi-file edits.

## Setup

1. Install the extension (Marketplace link above, or search "HUPI" in the
   Extensions view).
2. Have a HUPI gateway running and reachable (`../docs/INSTALL.md`).
3. Get an API key: `hupi-admin create-key -user <your-user-id>`, or via the
   admin UI's user detail page (`../docs/ADMIN_UI.md`) — shown once, save it.
4. In VS Code, run **HUPI: Set API Key** from the Command Palette and paste it.
5. In Settings (`Ctrl+,`), search "hupi" and set:
   - `hupi.baseUrl` — your gateway's root URL, e.g. `http://localhost:8787`
     (no trailing slash, no `/v1`).
   - `hupi.model` — a profile name from your deployment's `providers.yaml`,
     or leave blank to use HUPI's configured default chat provider.
   - `hupi.teamId` — only if you're using a Tier 3 shared-team HUPI
     deployment and want requests routed through the team endpoint instead
     of your private one.

### Signing in with OIDC/SSO instead of an API key

If your HUPI deployment has OIDC turned on (`docs/OIDC.md`), skip steps 3-4
above and instead set three more settings under "hupi":

- `hupi.oidc.issuerUrl` — the OIDC discovery URL, e.g.
  `https://login.microsoftonline.com/<tenant-id>/v2.0`.
- `hupi.oidc.clientAppId` — the extension's own public-client app
  registration ID (**not** the same value as your deployment's server-side
  `HUPI_OIDC_CLIENT_ID`, which identifies the resource API, not this client).
- `hupi.oidc.scope` — e.g. `api://<resource-client-id>/access_as_user
  offline_access openid profile`. Must include `offline_access` or you'll
  be prompted to sign in again every time your access token expires
  instead of it refreshing silently.

Once set, run **HUPI: Sign In** — your browser opens to your IdP's real
login page, and control returns to VS Code automatically once you finish.
**HUPI: Sign Out** clears the stored session. With both `issuerUrl` and
`clientAppId` set, OIDC is authoritative for the workspace — the extension
won't fall back to a pasted API key.

Setting up the client app registration (Azure AD/Entra ID example): create
a **second** app registration distinct from the one behind
`HUPI_OIDC_CLIENT_ID` (that one's the resource API; this one's the client
that signs users in), enable "Allow public client flows", add a
"Mobile and desktop applications" platform with the literal redirect URI
`http://localhost` (no port — Azure AD matches that against any loopback
port at sign-in time), and pre-authorize it for the resource app's
`access_as_user` scope (Enterprise Applications → your resource app →
Expose an API → Authorized client applications). See `docs/OIDC.md` for
the resource-app side.

### Using Remote-SSH / WSL / Dev Containers?

This extension always runs on your **local** machine (`extensionKind:
["ui"]`), even when VS Code is connected to a remote workspace — so
`hupi.baseUrl` needs to be reachable from there, not from the remote
host. See [docs/VSCODE_EXTENSION.md § Using VS Code's Remote-SSH / WSL /
Dev Containers](../docs/VSCODE_EXTENSION.md#using-vs-codes-remote-ssh--wsl--dev-containers)
for how to reach a gateway running on or behind a remote connection
(short version: VS Code's own "Forward a Port" command).

## Running it locally (development)

The Marketplace install above is all you need to just use the extension.
This section is only for working on the extension's own code — run it from
source via VS Code's Extension Development Host:

```bash
cd vscode-extension
npm install
```

Then open this `vscode-extension/` folder in VS Code and press **F5**
(`Run HUPI Extension`, wired up in `.vscode/launch.json`) — this builds the
extension and opens a second VS Code window with it loaded. Set your API
key and settings in *that* window (Command Palette → `HUPI: Set API Key`),
then try the chat sidebar and select-some-code-then-Ctrl+K.

`npm run watch` rebuilds automatically on save if you're iterating —
reload the Extension Development Host window (`Ctrl+R`/`Cmd+R` in it) to
pick up changes.

## Publishing an update to the Marketplace

Already published once (publisher `hupi`, extension id `hupi.hupi-vscode`).
To ship a new version:

1. Bump `"version"` in `package.json` and add a `CHANGELOG.md` entry — the
   Marketplace rejects re-publishing the same version number.
2. `npm run package` — builds and produces `hupi-vscode-<version>.vsix`
   locally, with no network calls. **Install and try this file yourself
   first**: Extensions view → `...` menu → "Install from VSIX...". This is
   the actual artifact that would ship, so it's the last real check before
   anyone else sees it.
3. `npm run publish` — builds and pushes the new version live. Needs
   `vsce` to already be logged in as the `hupi` publisher
   (`npx vsce login hupi`, prompts for a Personal Access Token from
   https://dev.azure.com with **Marketplace: Manage** scope — a one-time
   setup per machine, and not something an agent should do on your
   behalf: run the login step yourself in a terminal so the token never
   passes through a chat transcript).

## Verification

```bash
npm run typecheck   # tsc --noEmit
npm run lint        # eslint
npm run build       # esbuild, produces dist/extension.js + dist/webview/chat.js
npm test            # vitest — pure-logic unit tests, no vscode import needed
npm run smoke       # live check against a real running HUPI gateway
```

`npm run smoke` needs a HUPI gateway actually running
(`HUPI_SMOKE_BASE_URL`, defaults to `http://localhost:8787`; optionally
`HUPI_SMOKE_API_KEY`/`HUPI_SMOKE_MODEL`) — it sends one real streamed chat
request and confirms a non-empty response comes back, proving the actual
HTTP/SSE round trip works, not just that the code compiles.

**What this doesn't verify**: `npm test`/`npm run smoke` don't exercise the
actual chat sidebar or inline-edit UI inside a real editor — that needs a
real VS Code window (the `F5` flow above). Neither this project's CI nor
an automated agent can drive that without a display.
