# Changelog

## 0.1.11

- Added `hupi.inlineSuggestions.model` — lets ghost-text completions use
  a distinct, cheaper/faster model profile from your `providers.yaml`,
  independent of `hupi.model` (the sidebar/`@hupi`/inline-edit model).
  Completions fire on every typing pause, so reusing a large chat model
  by default means real per-request cost on a paid hosted provider —
  this was previously only documented as a known gap (0.1.10's
  README/VSCODE_EXTENSION.md note), now it's a real setting. Leave blank
  (the default) to keep reusing `hupi.model`, unchanged from before.

## 0.1.10

- **Fix**: 0.1.9's inline completions (ghost text) were sending every
  debounced completion request through HUPI's normal capture pipeline —
  the gateway wrote a durable memory record for every one, since capture
  ran unconditionally with no way to opt out. Turning on
  `hupi.inlineSuggestions.enabled` would have flooded a user's memory
  store with near-meaningless single-line completions, one per typing
  pause. Fixed at the source: the gateway gained a new, separate
  `X-Hupi-Capture: off` opt-out (distinct from the pre-existing
  `X-Hupi-Memory: off`, which only ever controlled retrieval), and the
  ghost-text client now sends both headers on every request — no
  retrieval, no capture, for a request that was never a real
  conversation turn to begin with. Requires a HUPI gateway build that
  includes this fix; against an older gateway, the new header is
  harmlessly ignored and the old (unwanted) capture-everything behavior
  applies — upgrade the gateway if you use ghost text.

## 0.1.9

- Added ghost-text-style inline completions: as you type, HUPI can suggest
  a continuation inline (Tab to accept), the same interaction Copilot/
  Continue/Cursor use. **Off by default** (`hupi.inlineSuggestions.enabled`)
  — unlike the sidebar/inline edit/`@hupi` (all explicitly invoked), this
  sends a request to your HUPI gateway on every typing pause, a real
  latency/cost trade-off the others don't have. A debounce
  (`hupi.inlineSuggestions.debounceMs`, default 300ms) keeps a fast typist
  from firing a request per keystroke; config/sign-in errors fail silently
  here rather than popping a message on every pause (the other three paths
  already surface those clearly when explicitly used).
- Added multi-file edit (`HUPI: Multi-File Edit`, `Ctrl+Alt+M`/`Cmd+Alt+M`):
  pick from your currently-open files, describe a change, and HUPI proposes
  new content for whichever of those files actually need it. A review panel
  lists every proposed file with a per-file diff (VS Code's own diff view)
  and a checkbox, so you can apply some, all, or none — nothing touches
  your files until you hit Apply. Scoped to open editors rather than the
  whole workspace, a deliberate v1 bound: predictable in scope and cost,
  no background workspace scan.
- These two were explicitly out of scope for v1 as of 0.1.0-0.1.8 (see
  past versions of this file / the extension's README) — reversed by
  request, now that the sidebar, inline edit, and `@hupi` are all
  established and working well.

## 0.1.8

- Added `@hupi` as a chat participant in VS Code's native Chat view
  (`vscode.chat.createChatParticipant`), alongside the existing
  dedicated HUPI sidebar rather than replacing it — a third-party
  participant can't be made the Chat view's default, unqualified
  handler (that's reserved for the host's own default participant), so
  the always-visible sidebar and `@hupi` genuinely serve different
  moments. Marked `isSticky` so a conversation stays routed to HUPI
  after the first `@hupi` mention. Reuses the exact same
  config/client/streaming code the sidebar already used — conversation
  history and cancellation now come from the platform
  (`context.history`, the request's `CancellationToken`) instead of
  this extension's own hand-rolled state.

## 0.1.7

- Fixed `AADSTS50011` on OIDC sign-in: the loopback redirect URI was
  built as `http://127.0.0.1:<port>/callback`, but Azure AD's loopback
  exception for a registered `http://localhost` matches only on that
  literal hostname (not the `127.0.0.1` IP) and only lets the *port*
  vary, not the path. Redirect URI is now `http://localhost:<port>`, no
  path — found on the first real interactive sign-in test against a live
  Azure AD tenant.

## 0.1.6

- Added OIDC/SSO sign-in (`HUPI: Sign In` / `HUPI: Sign Out`) as an
  alternative to pasting an API key, for Tier 3 deployments with OIDC
  configured (see `docs/OIDC.md`). Authorization Code + PKCE via a local
  loopback redirect — device-code flow was deliberately not implemented,
  since it's commonly blocked by Azure AD's Security Defaults. New
  settings: `hupi.oidc.issuerUrl`, `hupi.oidc.clientAppId`,
  `hupi.oidc.scope`. No new dependency.

## 0.1.5

- Fixed a real concurrency bug in the chat sidebar: nothing previously
  stopped a second message (or "New Chat") from being sent while a
  response was still streaming. Two overlapping streams both wrote into
  the same in-progress assistant bubble, corrupting whatever was on
  screen and leaving stray text arriving after you'd already moved on —
  plausibly presenting as the sidebar "freezing"/acting unresponsive
  reported after switching focus away and back. Fixed two ways: the
  input box now disables while a response is streaming (so the UI can't
  trigger the overlap), and the extension host now cancels any in-flight
  request (`AbortController`) before starting a new one or clearing the
  conversation, as defense in depth.
- Note: this was investigated without being able to reproduce the
  original freeze report directly (no display in this environment) —
  this fixes a genuine bug found by code review, not a confirmed root
  cause. Please retest and report back if the freeze still happens.

## 0.1.4

- Found the actual root cause of the invisible activity bar icon: with
  VS Code connected via Remote-SSH/WSL/Containers, this extension was
  installing and running on the *remote* host (`.vscode-server`) by
  default, since `extensionKind` was never declared. Custom activity bar
  icons are a known weak spot for remotely-run extensions — the icon
  file itself (in every one of 0.1.0-0.1.3's variants) was never the
  problem. Added `"extensionKind": ["ui"]` so this extension always
  installs and runs on the local/client side, where VS Code's own UI
  process can load the icon directly, regardless of whether you're
  connected to a remote workspace.
- **If you were reaching the gateway via an SSH tunnel to a remote
  HUPI instance, that tunnel is now required again** (previously,
  running remotely meant `hupi.baseUrl=http://localhost:8787` resolved
  on the remote host directly, which happened to work without one).

## 0.1.3

- Switched the activity bar icon from SVG to a PNG. 0.1.2's SVG (a
  single filled path, explicit color) still rendered blank on at least
  one real install despite matching every documented requirement and
  rendering correctly in isolation — switching format entirely to rule
  out an SVG-specific rendering path.

## 0.1.2

- 0.1.1's icon fix wasn't enough — the activity bar icon was still
  rendering blank. Replaced the multi-shape SVG with a single unified
  `<path>` and an explicit fill color instead of `currentColor`.

## 0.1.1

- Fixed the activity bar icon rendering as a blank/invisible square — it
  was a stroke-only SVG (`fill="none"`, outline only), which VS Code's
  icon masking doesn't reliably render; switched to solid filled shapes.
- Added a "New Chat" button to the chat sidebar — starting a fresh
  conversation no longer requires reloading the whole VS Code window.
- Restyled the chat sidebar: role-labeled message cards, a header bar,
  and general spacing/typography cleanup.

## 0.1.0

Initial release.

- Chat sidebar with automatic active-file/selection context.
- Inline edit (`Ctrl+K`/`Cmd+K` on a selection) with a diff preview before
  applying.
- API key stored via VS Code's `SecretStorage`.
