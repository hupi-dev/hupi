# Changelog

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
