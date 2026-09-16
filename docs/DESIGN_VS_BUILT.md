# Design vs. Built

A detailed accounting of every place the design docs
([ARCHITECTURE.md](../ARCHITECTURE.md), [MEMORY_FORMAT.md](MEMORY_FORMAT.md))
describe something the code doesn't actually do yet. [HOW_IT_WORKS.md §9](HOW_IT_WORKS.md#9-design-vs-what-s-actually-built--read-this-before-trusting-the-rest)
has the short version; this is the long version, with what it would take
to close each gap and a priority order. Items are ordered by that
priority, not by where they appear in the other docs.

For each gap: what the design says, what the code actually does today
(file/function-level), why the gap exists, what breaks if it's never
closed, and rough effort to close it.

**Update**: #1, #2, and #3 below are now closed — see the "Closed" note at
the top of each. #4-#7 remain open, as originally described.

---

## 1. Entity attributes are replaced, not merged

**Closed.** `upsertEntities` (`internal/consolidation/store.go`) now reads
the existing row `for update`, merges the incoming attribute map over it
(`mergeAttributes` — new keys win, keys this run didn't mention survive),
and writes the merged result. The description below is kept as a record of
the original bug.

**Later refinement**: this section's title is now only accurate for
normal consolidation (`RunDaily`/`RunRollup`). `Runner.Correct` went back
to replacing a touched entity's attributes wholesale — deliberately, not
a regression — because merge let a correction that re-described a fact
under a different attribute key than the original run used leave the
stale key sitting right next to the corrected one. `cmd/hupi-correct
-dump-template` exists specifically to make wholesale replacement safe to
author by hand: it dumps the entity's current attributes as a starting
point, so correcting one fact means editing one line, not reconstructing
the whole set from memory. See `EntityUpdate`'s doc comment
(`internal/consolidation/types.go`) and `upsertEntities`' for the full
reasoning.

**Design**: implicit in treating entities as a knowledge graph that
accumulates facts about a person/project/preference over time.

**Built**: `internal/consolidation/store.go`, `upsertEntities` —
```sql
insert into entities (id, kind, name, attributes)
values ($1, $2, $3, $4)
on conflict (id) do update set
    name = excluded.name,
    attributes = excluded.attributes,
    last_updated = current_date
```
Whatever the consolidation LLM returns for an entity's `attributes` this
run **completely overwrites** whatever was stored before. If yesterday's
run recorded `{"vector_index": "pgvector", "repo": "~/repos/hupi"}` and
today's run only mentions the repo path (because today's episodes didn't
discuss the vector index), `vector_index` is silently gone.

**Why**: the simplest possible upsert, written to get entities persisting
at all; merge semantics were deferred, not forgotten (flagged in a code
comment at the time).

**Risk**: silent, compounding data loss on the one part of the system
that's supposed to be durable "current state" — worse than a summary
being wrong, because there's no `supersedes`-style trail for entities at
all (see #2), so an overwritten attribute leaves no record it ever
existed.

**Effort to close**: small. Read the existing row (if any) inside the
transaction, merge the new attribute map over the old one (new keys win,
old keys not mentioned this run survive), write the merged result. ~20
lines in `upsertEntities`.

---

## 2. Corrections (`supersedes`/`correction_reason`) are schema-ready but never written

**Closed.** `Runner.Correct` (`internal/consolidation/runner.go`) reloads
the old summary's source material (episodes for a daily correction,
lower-level summaries for a rollup correction), re-runs the grounding
check against the human-supplied replacement content, and stores it via
the existing `storeSummary` path with `supersedes`/`correction_reason`
set. `cmd/hupi-correct` is the CLI entry point. The description below is
kept as a record of the original gap.

**Design**: MEMORY_FORMAT.md § Grounding & correction — "the fix is never
to edit the summary file in place... a new summary record is written for
the same period/level with an incremented id, `supersedes` pointing at the
old record's id, and `correction_reason` filled in."

**Built**: the schema fully supports this (`summaries.supersedes`,
`summaries.correction_reason`, both nullable, both filtered on —
`retrieve.go`'s `buildAnchor` and `vectorSearchSummaries`,
`runner.go`'s `loadSummaries`, all say `where ... supersedes is null`).
**Nothing writes a non-null `supersedes` anywhere in the codebase** —
`storeSummary` always inserts with an implicit `supersedes = NULL`. If a
summary is wrong today, there is no code path to fix it; the schema is
ready for a feature that doesn't exist yet.

**Why**: consolidation was built to write forward (new periods), not to
revise the past — correction is a distinct, smaller feature that was
sequenced after the main pipeline in the original build order and hasn't
been picked up yet.

**Risk**: this is the single biggest gap relative to how much the design
docs lean on it. The entire "grounding check catches fabrication" story
(ARCHITECTURE.md § Consolidation integrity safeguards) explicitly assumes
a correction mechanism exists for the cases grounding *doesn't* catch —
right now, a human who spots a wrong memory has no supported way to fix it
short of a manual `UPDATE`, which is exactly the silent-mutation failure
mode the append-only design was built to avoid.

**Effort to close**: small-medium. A new `Runner.Correct(ctx, oldSummaryID
string, output consolidationOutput, reason string) error` that loads the
old summary's `period`/`level`, runs it through the existing
`storeSummary` path with `supersedes` and `correctionReason` threaded in,
plus a thin CLI (`cmd/hupi-correct`) to invoke it by hand. Most of the
machinery (`storeSummary`, `groundingCheck`, `nextSummaryID`) already
exists and is reusable as-is.

---

## 3. Vector search covers `summaries` only, not high-importance episodes

**Closed.** Write side: `Runner.embedHighImportanceEpisodes`
(`internal/consolidation/runner.go`), called from `RunDaily` after the
day's summary is stored, embeds any episode clearing
`episodeEmbedImportanceThreshold` (`0.6`) that doesn't have an embedding
yet. Read side: `Store.vectorSearchEpisodes`
(`internal/store/retrieve.go`) mirrors `vectorSearchSummaries` against
`episodes`, both now reuse a single query embedding computed once in
`Retrieve`. A new `retrieved_episode_ids` column/field/trace path carries
this through capture and `hupi-trace` alongside the existing summary/entity
ones. The description below is kept as a record of the original gap.

**Design**: ARCHITECTURE.md's retrieval-engine table and component diagram
both describe vector search running over "summaries + high-importance
episodes."

**Built**: `internal/store/retrieve.go`'s `vectorSearchSummaries` queries
`summaries` exclusively. `episodes.embedding` (the column exists in
`schema/0001_init.sql`) is **never written by anything** — no code path
embeds an episode, ever. The column is dead weight right now.

**Why**: flagged explicitly as "an identical query pattern, omitted from
this sketch for brevity" when retrieval was first built — but the
prerequisite (something has to *write* `episodes.embedding` first) was
never circled back to either, so it's a two-sided gap: no writer, no
reader.

**Risk**: a genuinely important single exchange that never gets folded
into a daily summary (e.g., something said and then not revisited before
that day's consolidation, or a day where consolidation itself fails) is
invisible to vector search until/unless it resurfaces via a summary.
Moderate risk — summaries are the primary retrieval surface by design, so
this degrades recall for recent/edge-case content, it doesn't remove
retrieval entirely.

**Effort to close**: medium, and touches two places:
- *Write side*: episode embedding can't happen inside `Capture` (that
  would reintroduce the network-call-in-the-hot-path problem Capture was
  specifically redesigned to avoid). It belongs in consolidation instead —
  `RunDaily` already loads and touches every episode for the day; the
  natural spot is to also embed and write back any episode whose
  `importance` clears a threshold.
- *Read side*: a `vectorSearchEpisodes` mirroring `vectorSearchSummaries`,
  and merging/re-ranking its results with the summary results in
  `Retrieve`.

---

## 4. No calendar scheduling for weekly/monthly/yearly rollups — RESOLVED

**Design**: ARCHITECTURE.md's consolidation engine describes a full
daily→weekly→monthly→yearly chain.

**Built** (as of `docs/GAP_CLOSURE_PLAN.md` §4.1, `cmd/hupi-consolidate/rollup.go`):
`cmd/hupi-consolidate`'s nightly run now checks, after `RunDaily`, whether
the date it just consolidated crosses a weekly (Monday), monthly (the
1st), or yearly (Jan 1) boundary, and calls `Runner.RunRollup` with the
computed period/sourcePeriods when it does. `RunRollup` itself gained an
idempotency guard (skip if that level+period already has a summary) so a
missed or repeated cron run is harmless rather than producing duplicate
rollups. No new cron entry — one binary now does both jobs. Covered by
`cmd/hupi-consolidate/rollup_test.go` (calendar-boundary logic) and
`internal/consolidation/runner_test.go`'s `TestRunRollup_IdempotentAcrossReruns`
(real Postgres).

**Historical risk, now closed**: `summaries.level` no longer stays
`'daily'`-only in practice — the hierarchical retrieval-scaling property
the design leans on ("retrieval never has to linearly scan years of logs")
now actually gets built up over time instead of requiring a manual
`hupi-correct`-style intervention.

**Effort to close**: medium. Needs: ISO week/month/year boundary
calculation, a query for "which daily (or weekly, for monthly; weekly, for
... ) periods exist and aren't yet rolled into the next level up," and a
scheduling decision (a `-level` flag on `hupi-consolidate`, or a separate
small command, invoked by a second/third cron line).

---

## 5. Consolidation's structured output parsing is fragile

**Design**: implicit — MEMORY_FORMAT.md's schema assumes reliably
structured `key_facts`/`entities_touched` output from the consolidation
LLM.

**Built**: `internal/consolidation/prompts.go`'s `extractJSON` does a bare
`strings.IndexByte(s, '{')` / `LastIndexByte(s, '}')` substring extraction
on the model's free-text response, explicitly flagged in its own doc
comment as "a pragmatic stand-in."

**Why**: getting the pipeline working end-to-end first; every major
provider now supports a real structured-output or tool-calling mode that
guarantees valid JSON, but wiring that per-adapter is real, adapter-specific
work that was deferred.

**Risk**: low-to-moderate and provider-dependent. A model that wraps JSON
in extra prose with stray braces, or that fails to produce valid JSON at
all, breaks that day's consolidation run outright (`json.Unmarshal` error
surfaces as a `RunDaily` failure) rather than degrading gracefully.

**Effort to close**: medium, and it's per-adapter: add a `ChatJSON`-style
method (or a mode flag on `ChatCompletion`) to `provider.Provider` that
uses OpenAI's `response_format: json_schema` / Anthropic's tool-use mode
under the hood, and switch `generateSummary`/`groundingCheck` to call it
instead of parsing free text.

---

## 6. No integration testing against real Postgres or a real LLM

**Design**: n/a — an engineering-process gap, not a design/code gap.

**Built**: every claim of correctness so far rests on `go build`,
`go vet`, `gofmt -l`, and one manual run of the gateway binary confirming
it fails loud without config. No test has ever executed a real SQL
statement against Postgres, called a real embedding model, or exercised
the `pgvector` distance operators, the check constraints, or the FK
behavior on live data.

**Why**: no Postgres instance available in the environment these sketches
were built in.

**Risk**: real. Several things are *plausible but unverified*: the
`substring(id from 'v([0-9]+)$')` regex in `nextSummaryID`, the exact
behavior of `pgvector`'s `<=>` operator and whether `hnsw` index creation
syntax matches the installed `pgvector` version, whether the `1 - distance`
similarity conversion in `vectorSearchSummaries` is the right formula for
whatever distance metric the index was actually built with
(`vector_cosine_ops` should mean this is right, but it's never been
checked against real output).

**Effort to close**: medium. `docker-compose.yml` with a
`pgvector/pgvector` image, a test harness that runs `schema/0001_init.sql`
against it, and at least: one test proving `Capture` + `Trace` round-trip
correctly through real encryption, one proving the gate thresholds behave
as documented against a small seeded dataset, one proving
`nextSummaryID`'s regex versioning actually increments correctly.

---

## 7. Tier 3 (Professional Shared) doesn't exist in code

**Design**: ARCHITECTURE.md § Tiers — a `scope` column
(`private:<user_id>` / `shared:<team_id>`), permission-filtered retrieval,
workspace routing, team-voice consolidation.

**Built**: nothing. No `scope` column, no auth of any kind, no per-user
concept anywhere in the schema or code. Every table and every query
assumes exactly one owner.

**Why**: explicitly sequenced last in ARCHITECTURE.md's build order on
purpose — "extending a proven single-user system rather than being built
simultaneously with it." Everything built so far is that single-user
system; Tier 3 hasn't been started because its prerequisite (a working
Tier 1/2) only just became real.

**Risk**: none yet — it's not a regression, it's unstarted future work.
Worth flagging only so it isn't mistaken for a smaller gap than it is: this
is a genuine schema migration plus a new auth/access-control layer, not an
incremental fix like #1-#5.

**Effort to close**: large. Out of scope for "close a gap," this is its
own project.

---

## Priority order and rationale

| # | Gap | Status | Effort | Why this order |
|---|---|---|---|---|
| 1 | Entity attribute merge | **Closed** | Small | Actively losing data today, on every consolidation run that touches an existing entity. Fixed first because it's cheap and the damage compounds the longer it's left. |
| 2 | Corrections (`supersedes`) | **Closed** | Small–Medium | The design's core integrity story (ARCHITECTURE.md § Consolidation integrity safeguards) is incomplete without it — grounding catches fabrication *at write time*; correction is the only mechanism for catching it later. |
| 3 | Episode-level vector search | **Closed** | Medium | Real recall gap, but summaries remain the primary surface — lower urgency than #1/#2, both of which were correctness/integrity issues rather than a completeness gap. |
| 4 | Rollup scheduling | **Closed** (`GAP_CLOSURE_PLAN.md` §4.1) | Medium | Was fine to defer while history was short; closed once multi-month deployments made it matter. |
| 5 | Structured-output hardening | Open | Medium | A robustness improvement, not a correctness bug — today's approach works when the model behaves; this makes it work when it doesn't. |
| 6 | Integration testing | Open | Medium | Should really happen *alongside* 1-5, not after — noted last only because it needs an external Postgres instance this environment doesn't have on hand. |
| 7 | Tier 3 | Open | Large | Deliberately last per the original build order; nothing above depends on it, and it depends on everything above being solid first. |

**#1, #2, and #3 are now closed** — the two active correctness bugs plus
the recall gap that was small enough to close without a scheduling
subsystem. #4-#7 remain open, scoped future work as described above.
