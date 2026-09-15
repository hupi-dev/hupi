# HUPI — Business Process & Usage Guide

This document explains what HUPI is, who it's for, and walks through how
it's actually used, scenario by scenario. It's written for a non-code
audience — a product owner, an operator, or a new user deciding whether
and how to adopt it. For the technical internals behind any of this, see
[HOW_IT_WORKS.md](HOW_IT_WORKS.md), [CODE_GUIDE.md](CODE_GUIDE.md), and
[API_REFERENCE.md](API_REFERENCE.md).

**A note on accuracy**: this document describes what's actually built and
working today, verified against the code, not the original aspirational
design. Where something is designed but not yet built, it says so
explicitly rather than implying it exists.

---

## 1. What HUPI is

HUPI is a memory layer that sits between you and any AI language model.
Normally, an AI's memory of you resets the moment your conversation ends,
or is capped by how much text fits in one context window. HUPI fixes
that: every conversation is durably recorded, periodically distilled into
searchable summaries, and automatically re-injected into future
conversations — with any AI vendor, not just one.

Concretely, it's a small server (the "gateway") you point your existing
AI tools at instead of pointing them directly at OpenAI, Anthropic, or
whichever vendor you use. From the outside, it looks exactly like a
normal AI API. Underneath, every turn is enriched with relevant memories
before being forwarded to the real AI, and recorded afterward for future
turns to draw on.

The three properties this is built around:

1. **Provider-independence.** Switch from GPT to Claude to a locally
   hosted model — your memory doesn't move or need re-indexing beyond a
   one-time re-embed, because it's stored as plain decrypted text and
   metadata, not tied to any one vendor's format.
2. **Integrity over convenience.** Every "memory" the system asserts as
   fact is independently double-checked against its source text before
   being trusted (see "Grounding" in the glossary) — the system is
   designed to be honest about what it doesn't know, rather than
   confidently making things up about your own history.
3. **You can audit it.** Every retrieval decision is recorded. You can
   ask, for any past AI response, "what exactly did it remember, and from
   where" and get a real, decryptable answer — not a guess.

## 2. Who it's for — the three tiers

| Tier | Who | Status |
|---|---|---|
| **1 — Personal** | One person, self-hosted | **Built and working.** |
| **2 — Professional (Single)** | One person, deployed/managed by an organization | **Built and working** — architecturally identical to Tier 1; the difference is operational (SSO, centrally managed encryption keys, org retention policy would layer on top, but none of that ops tooling is built — today it's Tier 1 plus whatever your IT department wraps around the deployment). |
| **3 — Professional (Shared)** | A team, sharing some memory while keeping personal memory private | **Built and working**, including the security hardening described in §7 below. This is a substantive change from earlier design snapshots of this project ([DESIGN_VS_BUILT.md](DESIGN_VS_BUILT.md) is now out of date on this specific point) — real multi-user identity, team workspaces, and per-team encryption keys all exist and are tested. |

All three tiers run the exact same software (`cmd/hupi` and friends) —
which tier you're using is a function of whether you've configured
authentication and created any teams, not a different product.

## 3. Core concepts, in plain language

- **Episode**: one exchange — what you said, what the AI said back.
  Every episode is stored encrypted, forever, by default (deleting old
  history is a manual, deliberate action, not automatic).
- **Summary**: a distilled rollup of a day's (or week's/month's/year's)
  episodes, produced overnight by a separate process, not the live chat.
  This is what's actually searched when the AI "remembers" something —
  individual episodes are a fallback, not the primary path.
- **Entity**: a standing fact about a person, project, preference, or
  skill — the knowledge-graph part of memory, updated (not replaced) each
  time consolidation touches it.
- **self_model**: your own communication style and standing preferences —
  the one piece of memory you write by hand rather than the AI inferring
  it, and the one piece that's injected into *every* conversation
  regardless of what it's about.
- **Scope**: who a piece of memory belongs to — either your own private
  scope, or a team's shared scope. Every single record in the system
  belongs to exactly one scope, enforced at two independent levels (see
  §7).
- **Workspace**: which scope a given conversation is happening in.
  Talking to the gateway normally means your private workspace; a
  request routed through `/v1/team/<team>/...` means that team's shared
  workspace. Your own personal voice (`self_model`) stays yours even
  inside a team workspace — only the facts being searched change.
- **Retrieval gate**: whether, and how confidently, memory was pulled
  into a given turn — `skipped` (nothing looked relevant enough to even
  search), `partial` (searched, found nothing solid), or `full` (found
  something solid). Every conversation turn records which one applied.
- **Grounding**: an automatic, independent fact-check the system runs on
  every generated summary before trusting it — a second AI call verifies
  each claimed fact against the actual source conversation, and anything
  it can't verify is flagged and excluded from future recall rather than
  silently accepted.
- **Correction**: the only sanctioned way to fix a wrong memory. Nothing
  is ever silently edited — a correction is a brand new version that
  supersedes the old one, with a reason recorded, so there's always an
  audit trail of what was wrong and why it was changed.

## 4. Setting up a deployment (what an operator actually does)

1. Stand up Postgres with the `pgvector` extension, and apply the SQL
   files in `schema/` in numeric order (`0001` through `0006`).
2. Create the `hupi_app` database role (part of migration `0004`) and set
   its password out of band — never commit a real password to the
   migration files.
3. Write a `providers.yaml` (see `providers.yaml.example`) naming which
   AI vendor(s) to use for live chat, for consolidation, for the
   independent grounding check, and for embeddings. These can all be
   different vendors, or the same one.
4. Set the required environment variables: `HUPI_DATABASE_URL` (or,
   better, `HUPI_APP_DATABASE_URL` pointed at the `hupi_app` role — see
   §7), `HUPI_KEK` (the master encryption key), and optionally
   `HUPI_REQUIRE_AUTH=true` if you want Tier 3's team features available
   at all (without it, the gateway runs in single-user mode and nothing
   ever requires an API key).
5. Start `cmd/hupi` — the gateway is now reachable at the configured
   address (`127.0.0.1:8787` by default) and speaks the standard AI chat
   API.
6. Schedule `cmd/hupi-consolidate` to run once daily (cron), and
   optionally `cmd/hupi-selfcheck` weekly, once you have a few hand-written
   probe questions in `probes.yaml`.

That's a working Tier 1 deployment. Tiers 2/3 add identity provisioning
(§6) on top of the same running system.

## 5. Scenario: a solo personal user, day one through day thirty

**Day 1.** You point your usual AI chat tool at the gateway instead of
directly at your AI vendor. You mention you're starting a new project
called "Atlas" and that you prefer terse answers. Nothing special
happens yet from your side — the conversation feels completely normal.
Behind the scenes: the gateway checked for relevant memory (found none,
first day), forwarded your message to the real AI, and durably recorded
the exchange the moment the AI replied — before your screen even
finished rendering the response.

**That night.** A scheduled job reads everything you talked about that
day, asks an AI to summarize it into a handful of concrete, cited facts
("the project's name is Atlas"), and — critically — asks a *second*,
independent AI call to verify each of those facts actually appears in
what was said, before accepting them as real memory. It also updates a
standing record for "project: Atlas" with what it learned.

**Day 15.** You ask, "what was that project I mentioned a couple weeks
back?" The gateway recognizes the question is worth searching for,
searches your summarized history, finds the "Atlas" summary and entity,
and hands the AI a note containing that fact before the AI ever sees your
question. The AI answers correctly — not because it remembers in the
way a human would, but because the fact was handed to it just in time.

**Day 30.** You've had hundreds of exchanges. Retrieval keeps working the
same way regardless of history size, because it's always searching
distilled summaries, not scanning every raw conversation you've ever had.

## 6. Scenario: standing up a shared team (Tier 3)

1. An operator runs `hupi-admin create-user -id user:alice -email
   alice@example.com`, then `hupi-admin create-key -user user:alice` —
   this prints a raw API key exactly once; it's never recoverable after
   that, only revocable.
2. `hupi-admin create-team -id team:acme-eng -name "Acme Engineering"`,
   then `hupi-admin add-member -team team:acme-eng -user user:alice -role
   admin`.
3. Alice's everyday tool still points at `/v1/chat/completions` — her
   private history, exactly as in the solo scenario above.
4. For team knowledge, a client (or Alice, deliberately) sends requests to
   `/v1/team/team:acme-eng/chat/completions` instead. Everything discussed
   there is consolidated into the team's shared summaries — in a neutral,
   third-person "the team decided..." voice, not any one person's — and
   is visible to every team member, not just Alice.
5. Crucially: even inside the team workspace, if Alice's `self_model`
   (her communication style) exists, it's still applied — the AI doesn't
   suddenly talk like a committee just because the facts it's drawing on
   are shared.
6. A second team member, Bob, added the same way, sees the team's shared
   facts the moment he's added — but never sees Alice's private
   conversations, and Alice never sees his, even though they're both
   members of the same team. This isolation is enforced twice over (§7),
   not just by the application trusting itself to filter correctly.

## 7. Scenario: a security incident — what actually protects you

Say an API key leaks, or an attacker gets read access to the database
directly. What happens depends on which layer is doing the protecting:

- **Scope filtering** (built into every query the application makes):
  even with a leaked key, the attacker can only act as the user that key
  belongs to — they see that user's private memory and whatever teams
  that user belongs to, nothing else.
- **Row-level security** (enforced by the database itself, independent of
  the application code): even if a future bug in the application forgets
  to filter a query correctly, the database refuses to return rows
  outside the requesting session's declared scope. This is a second,
  independent safety net — verified with tests that deliberately try to
  break it.
- **Per-team encryption keys**: even someone with raw access to the
  encrypted database contents cannot read any team's data without that
  specific team's encryption key — every team's data is encrypted
  separately, so a compromised key exposes only that one team, not the
  whole deployment.
- **Audit log** (`audit_log`, `hupi-audit`, `docs/GAP_CLOSURE_PLAN.md` §4.3):
  "show me every time team X's data was accessed and by whom" is answered
  by `hupi-audit query -scope-owner team:acme-eng`. Every write (a chat
  turn's capture, a correction), every retrieval (a chat turn reading
  team memory back), every investigation (`hupi-trace`), and every admin
  action (`hupi-admin`/`hupi-admin-ui`, attributed to a named operator,
  not a shared credential) lands in the same table. It's insert-only for
  the running application by design — see the schema comment on
  `audit_log` — so an attacker who compromises the app's own database
  credential still can't erase their own trail from it.

## 8. Scenario: the AI misremembered something

1. You notice the AI confidently stated something wrong — say, it claimed
   you decided on a tool you never actually chose.
2. You (or your client tooling) submit `POST /v1/feedback` referencing the
   episode, with a rating of `memory_wrong` and an optional note. This is
   recorded immediately and durably — feedback submission is one of the
   few things in this system that fails loudly rather than quietly if it
   can't be saved.
3. An operator runs `hupi-trace` against the original episode id — this
   decrypts and prints exactly what the retrieval step saw and handed to
   the AI for that turn: which summary, which entity, at what confidence.
   This usually reveals whether the AI hallucinated on top of correct
   memory, or whether the memory itself was wrong.
4. If the memory itself was wrong (a bad summary), an operator writes a
   corrected version of the summary content by hand and runs
   `hupi-correct`. This does **not** edit the old, wrong summary — it
   writes a brand-new version that supersedes it, with the correction
   reason recorded. The system re-runs its independent fact-check against
   the corrected content before accepting it, the same as any new
   summary.
5. From this point on, retrieval only ever surfaces the corrected
   version — but the wrong one, and the reason it was wrong, both remain
   in the historical record rather than disappearing.

## 9. Scenario: changing which AI you use

Because the gateway forwards to whichever AI vendor is configured, not a
specific one hardcoded into the product, switching is a configuration
change, not a data migration:

- Editing `providers.yaml`'s `active_chat_provider` changes which AI
  answers your live conversations, immediately, on the gateway's next
  restart. Nothing about stored memory changes.
- `active_consolidation_provider` (the AI that writes your summaries
  overnight) is deliberately a **separate** setting from your day-to-day
  chat AI — kept stable on purpose, so switching your daily-driver model
  for unrelated reasons doesn't incidentally change the "voice" your
  memory gets written in. Changing it is a deliberate act, and the system
  records when and why it changed.
- `active_grounding_provider` (the independent fact-checker) can be a
  smaller, cheaper model than the one doing the actual writing — the
  fact-check task is simpler than prose generation.

## 10. Scenario: running it day to day (operations)

- **Daily**: `hupi-consolidate` runs once per active user and team,
  turning that day's raw conversations into checked, searchable
  summaries. A failure for one team doesn't block anyone else's — each
  is independent and the job reports which ones failed. The same run also
  checks whether the date it just consolidated crosses a weekly, monthly,
  or yearly boundary (Monday, the 1st, Jan 1 respectively) and rolls up
  the level below into the level above when it does — see
  [GAP_CLOSURE_PLAN.md §4.1](GAP_CLOSURE_PLAN.md) for the exact rule. No
  separate cron entry: one nightly job now covers both daily consolidation
  and rollup scheduling, and rolling up an already-rolled-up period is a
  no-op, not a duplicate, so a missed or repeated cron run is harmless.
- **Weekly (recommended)**: `hupi-selfcheck`, running a small,
  hand-written set of "does memory still know this" questions against the
  live system and failing loudly (for cron/alerting to catch) if
  retrieval quietly stops working for something it used to know. This is
  the regression test for the memory system itself, independent of
  whatever the underlying AI vendor does on their end.
- **On demand**: `hupi-trace` and `hupi-correct` for investigating and
  fixing specific problems, as in the scenario above.

## 11. Cost model

Every layer that costs money is an AI provider call, and there are four
distinct roles, each separately configurable and separately billed by
whichever vendor you point them at:

1. **Live chat** — one call per conversation turn (the AI actually
   answering you).
2. **Embedding** — one call per turn where retrieval decides a search is
   worth attempting (not every turn — trivial messages skip this
   entirely).
3. **Consolidation** — one call per active user/team, per day, to write
   that day's summary.
4. **Grounding** — a second call, alongside every consolidation call, to
   independently verify the facts it produced. This roughly doubles
   consolidation's cost, accepted deliberately in exchange for catching
   fabricated "memories" before they're trusted (see §3, "Grounding").

For a team, consolidation and grounding costs scale with the number of
active scopes (people *and* teams), not just headcount — a very active
team generates its own daily summary cost on top of each member's
personal one.

## 12. What's designed but not yet built

To avoid overstating the product, the following are described in this
project's design documents but do not exist in the running system today:

- ~~Data export/import~~ **Built** — `cmd/hupi-export`/`cmd/hupi-import`,
  per-scope or whole-deployment, merge-capable (see
  [MEMORY_FORMAT.md](MEMORY_FORMAT.md) and
  [GAP_CLOSURE_PLAN.md §4.2](GAP_CLOSURE_PLAN.md)). One known gap:
  nothing yet re-embeds imported historical data for vector search — see
  [MEMORY_FORMAT.md's workflow note](MEMORY_FORMAT.md#portability-workflow).
- ~~Weekly/monthly/yearly rollup scheduling~~ **Built** — see §10 above
  and [GAP_CLOSURE_PLAN.md §4.1](GAP_CLOSURE_PLAN.md).
- ~~An audit-log report surface~~ **Built** — see §7 above and
  [GAP_CLOSURE_PLAN.md §4.3](GAP_CLOSURE_PLAN.md).
- ~~Per-team encryption key rotation~~ **Built** — `hupi-rotate-key`,
  online (the gateway keeps serving that scope's traffic throughout) and
  resumable (killing and re-running the same command picks up where it
  left off). See [GAP_CLOSURE_PLAN.md §4.4](GAP_CLOSURE_PLAN.md). Old key
  material is kept, not deleted, until an explicit
  `-prune-old-versions` — and pruning only scrubs the database, not
  memory a still-running process already holds; restart every process
  sharing the database after a compromise-driven rotation, not just the
  one that ran the rotation.
- ~~Any admin UI~~ **Built** — `cmd/hupi-admin-ui` covers user/team/key
  provisioning and revocation over a browser, authenticated as a named
  operator (not a shared token, since every view and action is now
  audit-logged — see [GAP_CLOSURE_PLAN.md §4.3](GAP_CLOSURE_PLAN.md)).
  `cmd/hupi-admin` (the CLI) still exists unchanged; the UI wraps the
  same `internal/auth.Store` methods. It does **not** cover investigation
  (`hupi-trace` is still CLI-only — reading someone's retrieved memory
  content through a browser was a deliberate line not to cross, see
  [ADMIN_UI.md](ADMIN_UI.md#scope)), and key rotation above (`hupi-rotate-key`)
  is CLI-only too, by the same reasoning. See [ADMIN_UI.md](ADMIN_UI.md)
  for what changed and why.

For the complete, itemized accounting of every gap between design and
implementation — including ones not relevant to day-to-day usage — see
[DESIGN_VS_BUILT.md](DESIGN_VS_BUILT.md), though note its Tier 3 section
predates the work described in §6-§7 of this document and is now out of
date on that specific point; [TIER3_PLAN.md](TIER3_PLAN.md) and
[HARDENING_PLAN.md](HARDENING_PLAN.md) are the current, accurate record of
what Tier 3 and its security hardening actually include.
