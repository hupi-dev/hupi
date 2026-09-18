# TODO — product review findings

A point-in-time review of the whole product (engineering, VS Code extension,
marketing site, business positioning), done by auditing the actual repo state
rather than assumption. Ordered roughly by priority; each item names the
evidence behind it, not just the recommendation, so it can be re-verified
later rather than taken on faith.

If you fix one of these, delete it from this list rather than checking it
off and leaving it — a stale TODO is worse than no TODO.

## 1. No CI/CD exists at all

`.github/` doesn't exist anywhere in the repo. No GitHub Actions, no other CI
config, no Makefile `ci` target, no pre-commit hooks — nothing runs the test
suites automatically, on any commit, ever.

**Why this is the top priority**: this isn't theoretical risk. A single
session of live-testing OIDC/SSO support found and shipped three real bugs —
the loopback redirect URI format (`AADSTS50011`), the resource app's token
version (`sts.windows.net` vs the expected v2.0 issuer), and three missing
spaces on the live marketing site — none of which any existing test caught,
because nothing runs the tests except a human remembering to. A workflow
running `go build ./... && go vet ./... && go test ./...` (Go side) and
`npm run typecheck && npm run lint && npm test` (`vscode-extension/`) on
every push/PR would have caught real classes of what took hours to manually
diagnose.

## 2. Test coverage has specific, known holes

- 7 of 15 `internal/` packages have zero test files: `bootstrap`, `dbscope`,
  `identity`, `pgfmt`, `selfcheck`, `audit`, and `auth` (this last one is
  more defensible — its real logic is covered in the private `hupi-t3`
  overlay's `team_test.go`/`oidc_test.go`, not this repo).
- **`cmd/hupi` — the actual gateway binary — has no test file at all.** The
  wiring that decides Tier 1/2 vs 3, resolves auth, and starts the server has
  only ever been exercised by hand.
- VS Code extension: only `hupiClient.ts` and `oidc.ts` have tests (7 and 21
  cases respectively) — the only two files kept free of a `vscode` import,
  which is *why* they're testable outside a real VS Code host.
  `chatViewProvider.ts`, `config.ts`, `extension.ts`, `inlineEdit.ts`,
  `oidcAuth.ts`, `secrets.ts` all have zero coverage.

**Why**: these aren't random gaps — they're exactly the surfaces most likely
to regress silently (main-wiring, auth precedence, session state machines)
and least likely to be caught by manual testing before a release.

## 3. `extractJSON` is a known-fragile hack in the product's core differentiator

Flagged as open in `docs/DESIGN_VS_BUILT.md` #5 — a bare `{`/`}` substring
search over an LLM's structured output, described in that doc's own words as
"a pragmatic stand-in." This sits directly inside consolidation, the
fact-extraction step the entire "fact-checked, auditable memory" pitch
depends on.

**Why**: a malformed or wrapped LLM response silently breaking this is a
correctness bug in the one thing the product claims to be trustworthy about,
not a cosmetic issue.

## 4. No vulnerability disclosure policy, no contributor scaffolding

Confirmed absent at repo root: `SECURITY.md`, `CONTRIBUTING.md`,
`CODE_OF_CONDUCT.md`, and any `.github/ISSUE_TEMPLATE`.

**Why**: Tier 1/2's whole pitch is "free, open source, read the code," and
Tier 3's pitch is built on security/isolation — not having a documented way
to report a vulnerability is a credibility gap the moment a serious
enterprise evaluator looks for one. No `CONTRIBUTING.md` means every
external contributor has to reverse-engineer conventions from reading code
first, which suppresses the exact community momentum the open-core model
depends on.

## 5. Tier 3's entire commercial funnel is a cold email, with no stated price

Confirmed: every mention of Tier 3 licensing (`docs.astro`, `faq.astro`)
ends in `mailto:work@hupi.dev`. No price, no range, no form, no self-serve
path anywhere.

**Why**: for a developer audience, "email us" with no price is a well-known
conversion killer — people self-qualify against a number before ever
wanting to talk to a human, and most otherwise-interested evaluators leave
rather than cold-email a stranger. This directly limits how much revenue the
OIDC/SSO work (aimed squarely at enterprise buyers) can actually convert
into. Doesn't need full self-serve checkout — even a stated starting price
or range would remove most of the friction.

## 6. Zero analytics installed anywhere

Confirmed via grep across the whole site: no `gtag`, GA, Plausible,
PostHog, Segment, or Mixpanel anywhere.

**Why now, not later**: this directly blocks measuring the Google Ads
campaign plan already drafted (`/tmp/.../hupi-google-ads-campaign.md`) — you
cannot tell if any of those keyword groups convert without it. It also means
there's no way to know whether the new OIDC content (the Security-section
strip, the two new FAQ entries, the Editor.astro callout) is actually being
seen or clicked. Install this before spending money on ads, not after.

## 7. No hosted demo — self-hosting is the only way to ever see the product work

Traced the real first-run path: even the fastest documented route (Docker
Compose) needs a real Postgres, a real LLM provider API key, and 3+ manual
steps before anyone sees a single response. No sandbox, no live instance,
nothing beyond the homepage's demo GIF.

**Why**: for an infra/dev-tool product, that's a lot of commitment to ask
before someone's confirmed they like it. A rate-limited hosted sandbox (even
heavily caged) would substantially shorten time-to-"this actually works."

## 8. No `/metrics` endpoint — only binary health checks and audit-log queries

`/healthz`/`/readyz` exist (`cmd/hupi/main.go`); no Prometheus integration,
no `/metrics` route anywhere in `internal/` or `cmd/`.

**Why**: ops teams evaluating self-hosted infrastructure for Tier 3 expect
to plug into their existing Grafana/Prometheus stack, not rely solely on
`hupi-audit` queries. This is a natural extension of the auditability story
already being sold — right now it only covers "what happened," not "how is
it performing right now."

## 9. No privacy policy — becomes an actual blocker the moment #6 ships

Confirmed zero matches for "privacy"/"terms" anywhere in `site/src`. Once
analytics/ad conversion tracking goes in, running that in front of EU
visitors without a privacy/cookie notice is a compliance gap, not a
nice-to-have. Cheap to add now, before it's urgent.

## Smaller items worth naming

- **Team-voice consolidation quality is self-flagged as "unvalidated"**
  (`docs/TIER3_PLAN.md` §10 Risks) — worth real validation now that OIDC
  lowers the friction to get real teams actually using shared workspaces.
- **No case studies/testimonials/about page anywhere** — for Tier 3 buyers
  specifically, social proof matters, and there's currently zero on the
  site.
- **OIDC's "no instant revocation"** (`docs/OIDC.md` Known limitations) is a
  reasonable, already-documented tradeoff, but it's exactly the kind of
  question an enterprise security review will ask — worth having the
  mitigation (short token lifetime) ready to explain rather than it being a
  surprise mid-sales-conversation.

## If picking just three to start

1. **CI/CD** — protects everything else built from here on.
2. **A stated Tier 3 price or price range** — directly unblocks revenue from
   the feature just shipped.
3. **Analytics** — makes the marketing work already underway measurable
   instead of guesswork.
