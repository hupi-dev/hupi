# VS Code Extension

`vscode-extension/` is a chat sidebar and an inline-edit command (Cmd+K
style) for VS Code, backed by a HUPI gateway instead of an LLM vendor
directly — the same category of tool as Cursor/Continue/Cody, pointed at
your own memory-aware backend.

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
- **Inline edit** (`Ctrl+K`/`Cmd+K` with a selection) — describe a change,
  get a diff preview (original vs. proposed) before anything is applied.

**Explicitly out of scope for v1** (a scope decision, not an oversight):

- Inline autocomplete / Tab-style ghost text completions.
- Agentic multi-file edits (planning and editing across several files
  autonomously).
- Marketplace publishing — for now this runs from source via VS Code's
  Extension Development Host, not an installable `.vsix`/Marketplace
  listing.

## Setup

1. Have a HUPI gateway running and reachable — see
   [INSTALL.md](INSTALL.md).
2. Get an API key: `hupi-admin create-key -user <your-user-id>`, or via the
   admin UI's user detail page ([ADMIN_UI.md](ADMIN_UI.md)) — shown once,
   save it.
3. In VS Code, run **HUPI: Set API Key** from the Command Palette and
   paste it — stored via `SecretStorage`, never in plain settings.
4. In Settings, search "hupi" and set `hupi.baseUrl` (the gateway's root
   URL, no `/v1` suffix), optionally `hupi.model` (a `providers.yaml`
   profile name — blank uses HUPI's default chat provider) and
   `hupi.teamId` (only for a Tier 3 shared-team deployment).

## Building and running it

Not published to the Marketplace — build and run from source:

```bash
cd vscode-extension
npm install
```

Then open the `vscode-extension/` folder in VS Code and press **F5**
(`.vscode/launch.json`'s `Run HUPI Extension` config) — this builds the
extension (via `.vscode/tasks.json`'s pre-launch task) and opens a second
VS Code window with it loaded. Set your API key and settings in *that*
window, then try the chat sidebar and select-some-code-then-Ctrl+K.

`npm run watch` rebuilds on save while iterating; reload the Extension
Development Host window (`Ctrl+R`/`Cmd+R` in it) to pick up changes. See
[vscode-extension/README.md](../vscode-extension/README.md) for the full
day-to-day development workflow.

## What it looks like

- An activity bar icon opens the **Chat** panel — a message log plus an
  input box. Responses stream in and render as markdown (code blocks
  included).
- Selecting code and pressing `Ctrl+K`/`Cmd+K` prompts for an instruction,
  then opens VS Code's built-in diff view (current code on the left,
  HUPI's proposed rewrite on the right) with **Accept**/**Reject**
  presented as a follow-up prompt — nothing is written to your file until
  you accept.

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
