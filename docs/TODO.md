# TODO — product review findings

A point-in-time review of the whole product (engineering, VS Code extension,
marketing site, business positioning), done by auditing the actual repo state
rather than assumption. Ordered roughly by priority; each item names the
evidence behind it, not just the recommendation, so it can be re-verified
later rather than taken on faith.

If you fix one of these, delete it from this list rather than checking it
off and leaving it — a stale TODO is worse than no TODO.

Closed since the last pass (CI/CD, `SECURITY.md`, the `extractJSON` fix,
Tier 3 pricing, the `/metrics` endpoint, the privacy policy, zero
analytics installed anywhere, and both Go and VS Code extension
test-coverage gaps) — deleted per the rule above, not left as a
checked-off ledger. Analytics landed as Google Ads conversion tracking,
website-only (`site/src/lib/analytics.js`, `ConsentBanner.astro`) — never
loaded, referenced, or shipped in HUPI the software, which stays fully
self-hosted with zero telemetry; nothing fires until a visitor explicitly
accepts the consent banner, and `/privacy` was rewritten to disclose
exactly what it collects instead of the previous "we track nothing" line,
which stopped being true the moment this landed. It also surfaced a real
gap this review didn't originally name: there was no way to capture a
lead at all (no signup funnel, HUPI's whole model doesn't have one) —
`/contact` (a real form, backed by Web3Forms since the site is fully
static with no backend of its own) and `/thank-you` (where the lead-form
conversion actually fires) didn't exist until this pass added them.
Worth knowing for next time: Web3Forms' own server-side `redirect` field
turned out unreliable in practice — the form now submits via `fetch` and
redirects client-side after confirming `{success: true}` in the response,
which is the version actually live now. The VS Code extension closure needed a
hand-written `vscode` module mock (`src/test/vscode-mock.ts`, wired in via
`vitest.config.ts`'s `resolve.alias`) since no real `vscode` package
exists outside a real VS Code process — every previously-zero-coverage
file (`chatViewProvider.ts`, `config.ts`, `extension.ts`, `inlineEdit.ts`,
`oidcAuth.ts`, `secrets.ts`) now has real tests, including an integration
test that drives the actual (unexported) loopback HTTP server code in
`oidcAuth.ts` end to end rather than mocking it away. `internal/pgfmt`
(the last zero-coverage Go package) turned up a real bug along the way:
`ParseTextArray` didn't actually reverse `TextArray` for any item
containing a literal comma — fixed and covered by a round-trip test.
Team-voice consolidation quality (`docs/TIER3_PLAN.md` §10) is now
validated with a real-LLM eval (`hupi-t3`'s `internal/consolidation/team_test.go`)
rather than just self-flagged as untested — it found and led to fixing a
real prompt-quality issue (the model narrating its own privacy redactions)
along the way. Item 2 (no hosted demo) is closed too: `cmd/hupi-demo`
serves anonymous, cost-capped guest sessions against the real
`gateway.Handler` (no mock, no separate code path), with a `/demo`
page wired up site-wide, live at `https://demo.hupi.dev` — DNS, nginx,
a real TLS cert, and systemd units for both it and the main gateway,
all verified end to end. Building it live surfaced two real, previously
undiscovered ops gaps on the production VM itself (`ANTHROPIC_API_KEY`/
`OPENAI_API_KEY` were only ever exported ad hoc in a shell session, never
persisted, and the Postgres container's restart policy was `no`) — both
fixed, not just worked around.

## Smaller items worth naming

- **No case studies/testimonials/about page anywhere** — for Tier 3 buyers
  specifically, social proof matters, and there's currently zero on the
  site.
- **OIDC's "no instant revocation"** (`docs/OIDC.md` Known limitations) is a
  reasonable, already-documented tradeoff, but it's exactly the kind of
  question an enterprise security review will ask — worth having the
  mitigation (short token lifetime) ready to explain rather than it being a
  surprise mid-sales-conversation.

## If picking just one to start

No major item is currently open — what's left are the two smaller,
named items above. Of those, **case studies/testimonials** is the one
with a clearer trigger: worth doing once the first real Tier 3
conversation from the Google Ads campaign (now actually measurable)
turns into a customer, not before there's a real story to tell.
