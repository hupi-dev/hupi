# Changelog

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
