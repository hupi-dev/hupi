# TODO — product review findings

A point-in-time review of the whole product (engineering, VS Code extension,
marketing site, business positioning), done by auditing the actual repo state
rather than assumption. Ordered roughly by priority; each item names the
evidence behind it, not just the recommendation, so it can be re-verified
later rather than taken on faith.

If you fix one of these, delete it from this list rather than checking it
off and leaving it — a stale TODO is worse than no TODO.

Closed since the last pass (CI/CD, `SECURITY.md`, the `extractJSON` fix,
Tier 3 pricing, the `/metrics` endpoint, the privacy policy, and both Go
and VS Code extension test-coverage gaps) — deleted per the rule above,
not left as a checked-off ledger. The VS Code extension closure needed a
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
along the way.

## 1. Zero analytics installed anywhere

Confirmed via grep across the whole site: no `gtag`, GA, Plausible,
PostHog, Segment, or Mixpanel anywhere.

**Why now, not later**: this directly blocks measuring the Google Ads
campaign plan already drafted — you cannot tell if any of those keyword
groups convert without it. It also means there's no way to know whether the
OIDC content (the Security-section strip, the FAQ entries, the
Editor.astro callout) is actually being seen or clicked. Install this
before spending money on ads, not after. Once it ships, `/privacy` needs a
cookie/tracking disclosure added — it currently correctly says "we collect
nothing," which stops being true the moment this lands.

## 2. No hosted demo — self-hosting is the only way to ever see the product work

Traced the real first-run path: even the fastest documented route (Docker
Compose) needs a real Postgres, a real LLM provider API key, and 3+ manual
steps before anyone sees a single response. No sandbox, no live instance,
nothing beyond the homepage's demo GIF.

**Why**: for an infra/dev-tool product, that's a lot of commitment to ask
before someone's confirmed they like it. A rate-limited hosted sandbox (even
heavily caged) would substantially shorten time-to-"this actually works."

## Smaller items worth naming

- **No case studies/testimonials/about page anywhere** — for Tier 3 buyers
  specifically, social proof matters, and there's currently zero on the
  site.
- **OIDC's "no instant revocation"** (`docs/OIDC.md` Known limitations) is a
  reasonable, already-documented tradeoff, but it's exactly the kind of
  question an enterprise security review will ask — worth having the
  mitigation (short token lifetime) ready to explain rather than it being a
  surprise mid-sales-conversation.
- **No `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, or `.github/ISSUE_TEMPLATE`**
  — `SECURITY.md` is done, but the rest of the contributor-scaffolding gap
  from the original review is still open.

## If picking just one to start

**Analytics.** CI/CD, pricing, and the vulnerability-disclosure gap from the
original top-3 are all closed now — of what's left, analytics is the one
blocking something already in motion (measuring the Google Ads campaign),
not just a general improvement.
