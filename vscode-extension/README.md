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

## What's included (v1)

- **Chat sidebar** — a HUPI icon in the activity bar opens a chat panel.
  Every message automatically includes the active file (or your current
  selection) as context, the same way Cursor's chat references open files.
- **Inline edit** (`Ctrl+K` / `Cmd+K` with a selection) — describe a change,
  HUPI rewrites the selected code, you get a diff preview before anything
  is applied to your file.

Not included in v1 (by explicit scope decision, not an oversight): inline
autocomplete/Tab-style ghost text, and agentic multi-file edits.

## Setup

1. Have a HUPI gateway running and reachable (`../docs/INSTALL.md`).
2. Get an API key: `hupi-admin create-key -user <your-user-id>`, or via the
   admin UI's user detail page (`../docs/ADMIN_UI.md`) — shown once, save it.
3. In VS Code, run **HUPI: Set API Key** from the Command Palette and paste it.
4. In Settings (`Ctrl+,`), search "hupi" and set:
   - `hupi.baseUrl` — your gateway's root URL, e.g. `http://localhost:8787`
     (no trailing slash, no `/v1`).
   - `hupi.model` — a profile name from your deployment's `providers.yaml`,
     or leave blank to use HUPI's configured default chat provider.
   - `hupi.teamId` — only if you're using a Tier 3 shared-team HUPI
     deployment and want requests routed through the team endpoint instead
     of your private one.

## Running it locally (development)

There's no published extension yet — run it from source via VS Code's
Extension Development Host:

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

## Publishing to the Marketplace

Not published yet. To do it:

1. **Create a publisher** at https://marketplace.visualstudio.com/manage
   (sign in with a Microsoft account) — the publisher ID must match
   `package.json`'s `"publisher"` field (currently `hupi`; change it here
   first if that ID is taken or you want a different one).
2. **Get a Personal Access Token** from https://dev.azure.com — an
   organization → User Settings → Personal Access Tokens → New Token,
   scope: **Marketplace: Manage**.
3. `npx vsce login <publisher>` and paste the token when prompted (only
   needed once per machine).
4. `npm run package` — builds and produces `hupi-vscode-<version>.vsix`
   locally, with no network calls. **Install and try this file yourself
   first**: Extensions view → `...` menu → "Install from VSIX...". This is
   the actual artifact that would ship, so it's the last real check before
   anyone else sees it.
5. `npm run publish` — builds and pushes the current version live on the
   Marketplace. Bump `"version"` in `package.json` (and add a
   `CHANGELOG.md` entry) before publishing again — the Marketplace
   rejects re-publishing the same version number.

None of steps 1-2 can be done by an agent — they need your own Microsoft/
Azure identity and a real payment-free but human-verified account. Steps
3-5 are plain CLI commands once those exist.

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
