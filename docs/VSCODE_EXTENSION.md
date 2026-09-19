# VS Code Extension

`vscode-extension/` is a chat sidebar and an inline-edit command (Cmd+K
style) for VS Code, backed by a HUPI gateway instead of an LLM vendor
directly — the same category of tool as Cursor/Continue/Cody, pointed at
your own memory-aware backend.

Published on the VS Code Marketplace: search "HUPI" in the Extensions
view, or install directly from
https://marketplace.visualstudio.com/items?itemName=hupi.hupi-vscode.

## Why an extension, not a fork

Cursor is a full fork of VS Code/Code-OSS, maintained by a dedicated team
that continuously rebases onto upstream VS Code releases — a large,
ongoing engineering commitment, not a one-time change. This is deliberately
**not** that: `vscode-extension/` is a plain VS Code extension built
entirely on the public extension API (webview views, commands,
keybindings, `SecretStorage`, the diff command) — no forking, no rebasing
burden, and it stays compatible with new VS Code releases automatically.
The trade-off, stated plainly: a fork gives more control over the whole
editor; an extension is constrained to what the extension API exposes —
which covers chat and inline edit well, but not deeper editor surgery.

## How it talks to HUPI

HUPI's gateway (`cmd/hupi`) is byte-for-byte OpenAI-compatible —
`internal/gateway/types.go`'s `chatCompletionRequest`/`chatCompletionResponse`/
`chatCompletionChunk` are the standard OpenAI wire shapes, and
`internal/provider/openai_compat.go` forwards upstream in that same shape.
So the extension's HUPI client (`vscode-extension/src/hupiClient.ts`) is
just the official `openai` npm SDK, pointed at a different `baseURL`/`apiKey`
— no hand-rolled HTTP or SSE parsing anywhere in this extension.

Because the SDK always POSTs to `${baseURL}/chat/completions`, and HUPI's
two routes are `/v1/chat/completions` (private,
`internal/gateway/handler.go`'s `HandleChatCompletions`) and
`/v1/team/{team_id}/chat/completions` (`HandleTeamChatCompletions`), the
extension computes `baseURL` as `${root}/v1` or `${root}/v1/team/${teamId}`
depending on whether `hupi.teamId` is set — see `resolveBaseUrl` in
`hupiClient.ts`. Nothing else about routing differs between the two modes.

## Scope

**In scope (v1)**:

- **Chat sidebar** — a webview panel; every message automatically includes
  the active file or current selection as context.
- **`@hupi` chat participant** (`chatParticipant.ts`) — HUPI registered via
  `vscode.chat.createChatParticipant` in VS Code's own native Chat view,
  alongside the sidebar rather than instead of it (a third-party
  participant can't be made that view's default handler, so the two cover
  different moments). Reuses the sidebar's exact same
  `loadConfig`/`createClient`/`streamChat` call path; conversation history
  and cancellation come from the platform (`ChatContext.history`, the
  request's `CancellationToken`) instead of hand-rolled state.
- **Inline edit** (`Ctrl+K`/`Cmd+K` with a selection) — describe a change,
  get a diff preview (original vs. proposed) before anything is applied.
- **Inline completions** (`inlineCompletionProvider.ts`) — Copilot-style
  ghost text as you type, via `vscode.languages.registerInlineCompletionItemProvider`.
  Off by default (`hupi.inlineSuggestions.enabled`) since it's the one
  feature here that fires on every typing pause rather than an explicit
  action — debounced (`hupi.inlineSuggestions.debounceMs`) and fails
  silently on config/auth errors rather than popping a message mid-type.
- **Multi-file edit** (`multiFileEdit.ts`, `HUPI: Multi-File Edit`) — pick
  from currently-open files, describe a change, and a review panel (a
  webview) shows a per-file diff with a checkbox before anything is
  applied. Scoped to open editors, not a whole-workspace scan.

**Explicitly out of scope for v1** (a scope decision, not an oversight):

- Fully agentic multi-file edits with no review step — the multi-file
  edit above always shows a diff before applying anything.
- A `.vsix`/Marketplace release process for updates is manual — see
  [vscode-extension/README.md § Publishing](../vscode-extension/README.md#publishing-to-the-marketplace)
  for how a new version actually goes out (bump the version, `npm run
  publish` — needs the publisher's own Marketplace credentials, which no
  agent holds).

## Setup

1. Install the extension — search "HUPI" in VS Code's Extensions view, or
   install from
   https://marketplace.visualstudio.com/items?itemName=hupi.hupi-vscode.
2. Have a HUPI gateway running and reachable — see
   [INSTALL.md](INSTALL.md).
3. Get an API key: `hupi-admin create-key -user <your-user-id>`, or via the
   admin UI's user detail page ([ADMIN_UI.md](ADMIN_UI.md)) — shown once,
   save it.
4. In VS Code, run **HUPI: Set API Key** from the Command Palette and
   paste it — stored via `SecretStorage`, never in plain settings.
5. In Settings, search "hupi" and set `hupi.baseUrl` (the gateway's root
   URL, no `/v1` suffix), optionally `hupi.model` (a `providers.yaml`
   profile name — blank uses HUPI's default chat provider) and
   `hupi.teamId` (only for a Tier 3 shared-team deployment).

### OIDC/SSO sign-in instead of an API key

For a Tier 3 deployment with OIDC configured ([OIDC.md](OIDC.md)), skip
steps 3-4 above and instead set `hupi.oidc.issuerUrl`,
`hupi.oidc.clientAppId`, and `hupi.oidc.scope`, then run **HUPI: Sign In**.
The extension runs a standard Authorization Code + PKCE flow (RFC 8252's
"native app" pattern — the same one VS Code's own built-in Microsoft/GitHub
auth uses): it opens your system browser at the IdP's real login page and
listens on an ephemeral local port for the redirect back, so credentials
never pass through the extension itself. Device-code flow was deliberately
not implemented — it's commonly blocked by Azure AD's "Security Defaults"
(a real result from this project's own Azure AD validation run: every
device-code attempt failed with `AADSTS530035`, while the browser-redirect
flow was not affected), and a browser-redirect flow doesn't have that
failure mode.

`clientAppId` here is a **separate** app registration from your gateway's
`HUPI_OIDC_CLIENT_ID` — that one identifies the resource API the server
validates tokens against; this one is the public client that signs users
in and must be pre-authorized for the resource's scope (see
[vscode-extension/README.md](../vscode-extension/README.md) for the exact
Azure AD app-registration steps, including the `http://localhost`
no-port redirect URI Azure AD's "Mobile and desktop applications" platform
type expects).

**Troubleshooting, from real errors hit during this project's own live
testing**:

- `AADSTS50011: The redirect URI ... does not match` — the extension's
  redirect URI must be exactly `http://localhost:<port>` (hostname
  `localhost`, no path) to match Azure's loopback exception for a
  registered value of bare `http://localhost`; fixed in the extension as
  of v0.1.7, so this should only come up if you registered the redirect
  URI with a path or as the literal `127.0.0.1` instead of `localhost`.
- `oidc: id token issued by a different provider, expected ".../v2.0" got
  "https://sts.windows.net/..."` (visible in the gateway's own log, not
  the client error, per [handler.go's resolveIdentity](../internal/gateway/handler.go))
  — the *resource* app (`HUPI_OIDC_CLIENT_ID`, not this extension's
  `clientAppId`) needs its manifest's `api.requestedAccessTokenVersion`
  set to `2`; see [OIDC.md § Setting it up against Azure
  AD](OIDC.md#setting-it-up-against-azure-ad-entra-id).

Once both `hupi.oidc.issuerUrl` and `hupi.oidc.clientAppId` are set, OIDC
is authoritative for the workspace: the extension prompts sign-in rather
than silently falling back to any previously-stored API key, since that
key could belong to the wrong identity/team once OIDC is turned on.
Sessions refresh silently in the background as long as `scope` includes
`offline_access`; **HUPI: Sign Out** clears the stored session.

### Using VS Code's Remote-SSH / WSL / Dev Containers

This extension declares `"extensionKind": ["ui"]` (`package.json`), so it
always runs on your local machine, even when VS Code is connected to a
remote workspace — this is deliberate, not a limitation. Custom activity
bar icons and other UI contributions are a known weak point for
extensions that run on the remote side instead, and this extension has
no genuine need to run remotely anyway: it only makes outbound HTTP
calls and reads the active editor via the standard `vscode` API, neither
of which requires remote-host filesystem or process access.

The one consequence: `hupi.baseUrl` must be reachable from wherever the
extension actually runs — your **local** machine, not the remote host —
even if the HUPI gateway itself lives on that remote host or somewhere
else entirely. That's a networking question, independent of the
extension, with the same answer as for any other local tool that needs
to reach a service behind a remote connection:

- **Gateway running on the same remote host you're connected to** — use
  VS Code's built-in port forwarding: Command Palette → **"Forward a
  Port"** → the gateway's port (e.g. `8787`). VS Code forwards that port
  from the remote host to your local machine over the same SSH
  connection, automatically — no separate tunnel to manage. Then set
  `hupi.baseUrl` to `http://localhost:<forwarded-port>`.
- **Gateway running somewhere else** (a separate VM, on-prem server,
  etc.) — reach it the same way any local client would: a direct SSH
  tunnel from your own machine (`ssh -L <port>:localhost:<port>
  user@gateway-host`), a VPN, or a real routable address/DNS name for
  the gateway. Running that tunnel *inside* a remote SSH session's
  integrated terminal doesn't help — that terminal executes on the
  remote host, not your local machine, so it can't make `localhost`
  resolve correctly for a `ui`-kind extension running locally.

## Building it from source (for development, not needed to just use it)

The Marketplace install above is all you need as a user. Build from source
only if you're modifying the extension itself:

```bash
cd vscode-extension
npm install
```

Then open the `vscode-extension/` folder in VS Code and press **F5**
(`.vscode/launch.json`'s `Run HUPI Extension` config) — this builds the
extension (via `.vscode/tasks.json`'s pre-launch task) and opens a second
VS Code window with it loaded, running your local changes instead of the
published version. Set your API key and settings in *that* window, then
try the chat sidebar and select-some-code-then-Ctrl+K.

`npm run watch` rebuilds on save while iterating; reload the Extension
Development Host window (`Ctrl+R`/`Cmd+R` in it) to pick up changes. See
[vscode-extension/README.md](../vscode-extension/README.md) for the full
day-to-day development workflow, and its **Publishing to the Marketplace**
section for how a new version actually ships.

## What it looks like

- An activity bar icon opens the **Chat** panel — a message log plus an
  input box. Responses stream in and render as markdown (code blocks
  included).
- `@hupi <message>` also works from VS Code's own **Chat** view
  (`Ctrl+Alt+I`/`Cmd+Alt+I`), backed by the same gateway — useful
  alongside other chat participants/tools already living in that view.
- Selecting code and pressing `Ctrl+K`/`Cmd+K` prompts for an instruction,
  then opens VS Code's built-in diff view (current code on the left,
  HUPI's proposed rewrite on the right) with **Accept**/**Reject**
  presented as a follow-up prompt — nothing is written to your file until
  you accept.
- With `hupi.inlineSuggestions.enabled` turned on, pausing while typing
  shows a greyed-out ghost-text suggestion inline — `Tab` accepts it, same
  interaction as Copilot/Continue.
- `Ctrl+Alt+M`/`Cmd+Alt+M` (or **HUPI: Multi-File Edit**) lets you pick
  from your open files, describe a change, and opens a review panel
  listing every file HUPI proposed a change for — click a file to see its
  diff, uncheck any you don't want, then **Apply Selected**.

## Verification

- `npm run typecheck` (`tsc --noEmit`), `npm run lint` (ESLint), and
  `npm run build` (esbuild, two bundles: the extension host and the
  webview) are all clean.
- `npm test` (Vitest) covers the pure logic that doesn't need a real VS
  Code host: `resolveBaseUrl`'s private-vs-team routing, and streamed-delta
  accumulation — see `vscode-extension/src/hupiClient.test.ts`.
- `npm run smoke` sends one real streamed chat request to a HUPI gateway
  you already have running, and confirms a non-empty response comes back
  — proving the actual HTTP/SSE round trip, not just that the code
  compiles. Verified during development against a real `cmd/hupi` gateway
  plus a throwaway fake OpenAI-shaped upstream, confirming the streamed
  SSE path works end to end, and that the client fails cleanly (not a
  hang or crash) against an unreachable gateway.
- **What automated verification doesn't cover**: actually clicking through
  the chat sidebar or inline-edit UI inside a real editor. That needs VS
  Code's Extension Development Host (the `F5` flow above) running
  somewhere with a display — neither a CI job nor an agent without a GUI
  can drive that.
