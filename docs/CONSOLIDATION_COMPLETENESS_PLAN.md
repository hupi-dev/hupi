# Consolidation completeness: four real gaps behind LongMemEval's remaining failures

## Context

`docs/LONGMEMEVAL_ACCURACY_PLAN.md` set out to fix three LongMemEval
category regressions (`single-session-preference`, `temporal-reasoning`,
`knowledge-update`) with cheap, retrieval-side and prompt-side changes.
Real, per-question re-verification against actual failing scopes — not
theory — progressively disproved that framing: almost none of what
looked like retrieval or reasoning bugs actually were. This document
consolidates everything that investigation found, names the real
mechanism behind it, and lays out an exhaustive plan to fix all of it.
It supersedes categories 2 and 3 of the earlier plan doc as the
authoritative statement of what's actually wrong and what to do about
it; category 1 (`single-session-preference`) is unaffected and stays as
documented there.

**The one-sentence version**: HUPI's consolidation engine
(`internal/consolidation`) makes a single LLM pass per calendar period
(day, then week, then month, then year), each pass overwriting or
merging over whatever came before with no explicit model of "did I just
drop something," "does this contradict an earlier period," or "does
this fact actually span more than one period." Four distinct, real
failure modes fall out of that one architectural gap. None of them are
retrieval bugs — retrieval was re-verified working correctly against
whatever consolidation actually handed it, every time.

## The four gaps

### Gap 1 — Single-day dilution (omission)

**What it is**: a calendar day with many separate sessions gets
compressed by `generateSummary` (`internal/consolidation/runner.go`)
into one narrative summary. The model isn't told how much happened that
day carries any weight on how much it should write, so it picks
whatever topic reads as most prominent and silently drops the rest —
including real, specific facts a later question asks about directly.

**Real evidence, all independently confirmed via direct
`hupi-export-memory` reads of the actual consolidated scope, not
inference**:
- `gpt4_45189cb4` ("what's the order of the sports events I watched in
  January"): the NFL playoffs mention — a passing aside inside an
  unrelated food-recommendation message on a busy day — never appears
  in any of that scope's 5 daily summaries. Confirmed twice, before and
  after a prompt-level fix attempt (see below).
- `gpt4_e072b769` ("how many weeks ago did I start using Ibotta"): the
  real source session (2023-04-16, "I've just downloaded Ibotta")
  exists in the raw haystack, but that day's consolidated summary is
  entirely about an unrelated word problem ("Jacob has $30..."). Ibotta
  is completely absent, not misremembered.
- `b46e15ed` ("two charity events in a row, on consecutive days"): both
  2023-02-14 and 2023-02-15 (10 sessions each) have a real charity
  mention in the raw haystack; neither day's consolidated summary
  mentions charity at all.

**Not a token-budget/truncation problem**: `generateSummary`'s
`provider.ChatRequest` never sets `MaxTokens` (confirmed by reading the
call site — it's `nil`), and `loadDailyEpisodes` has no `LIMIT` — every
episode for the day reaches the model. The model has all the raw
material and no output-length ceiling. It chooses to compress toward
one topic because nothing in `summarySystemPrompt` says not to.

**What was tried and its real result**: a prompt-only fix
(`internal/consolidation/prompts.go`'s `summarySystemPrompt`, adding an
explicit instruction to write multiple paragraphs for multiple distinct
topics and extract at least one key_fact per source with a checkable
fact) was implemented and re-verified against fresh, uncontaminated
replays of all 3 examples above plus a 4th (`gpt4_e061b84f`, sports
event ordering). **3 of 4 came back byte-for-byte unchanged in outcome**
— same missing facts, same wrong or abstained answers — despite the new
instruction being present in the system prompt for every one of those
runs. This is a clean, real, negative result: a soft instruction added
to an already-large prompt doesn't reliably change a 45-50-session
day's compression behavior. (4th result pending at time of writing —
see Status below.)

### Gap 2 — Cross-day / cross-period stitching

**What it is**: even a hypothetically perfect per-day consolidation
doesn't answer a question whose answer depends on combining facts from
two different days. `b46e15ed`'s actual question ("two charity events
in a row, on consecutive days") needs Feb 14's fact **and** Feb 15's
fact **and** the realization that they're consecutive — and nothing in
the current architecture is responsible for making that connection.

**Why fixing Gap 1 doesn't fix this by itself**: `RunDaily` processes
one date at a time, with no visibility into any other date. The only
place a cross-day connection could ever get made automatically is a
rollup (`RunRollup`, same file) — a weekly rollup consolidates several
days' summaries together, so it's the one mechanism that *could* notice
"charity events happened on two consecutive days this week." But:
- It only works if both days' underlying facts survived Gap 1 in the
  first place (currently they don't).
- `RunRollup` reuses the exact same `generateSummary` call as daily
  consolidation — the same single-pass compression risk applies one
  level up, untested so far because Gap 1 already blocks it from
  mattering.
- `RunRollup` is write-once: it early-returns via `summaryExists` if a
  summary for that level+period already exists (confirmed by reading
  the function). Unlike daily summaries, which explicitly support
  re-consolidation and supersession when `RunDaily` runs again for an
  already-consolidated day, a rollup that's already been generated
  never gets a chance to pick up a later correction to its source days.

**Where else the connection could be made**: at retrieval/answer time,
if retrieval deliberately pulled in multiple relevant daily summaries
for a detected multi-event question shape, instead of the single best
match. This was actually already attempted (`docs/LONGMEMEVAL_ACCURACY_PLAN.md`'s
original Category 2 cause 1 fix — widening `fusedSearchSummaries`'
final-selection count for detected ordering/counting questions) and
found to have **zero effect**, but for a now-understood reason: the
candidate pool was already empty (`HUPI_DEBUG_FUSION` showed
`vectorRank=-1` for every candidate — nothing to widen into, since the
underlying facts weren't in any summary to begin with, per Gap 1). That
result doesn't mean retrieval widening is useless for this problem — it
means it was tested against a broken foundation. It needs re-testing
once Gap 1 has a real fix and there's actually something to widen into.

### Gap 3 — Entity/summary contradiction (no supersession across periods)

**What it is**: HUPI's real, deployed data — not a benchmark artifact —
already surfaced this: a Wells Fargo mortgage pre-approval amount has
**two real summaries in the database, dated months apart, referencing
the same entity, neither superseding the other.** Both stay permanently
"current," and retrieval has no signal for which one is right.

**Two distinct, both-confirmed root causes, precisely located**:

1. **Entity attribute merge is last-write-wins per-JSON-key, with no
   precedence between differently-named keys for the same real fact.**
   `internal/consolidation/store.go`'s `upsertEntities` (non-`replace`
   path) calls `mergeAttributes(existing, incoming)`, which is a flat
   key overlay (`store.go:600` onward) — a new key is added, an
   existing key with the *same name* is overwritten, but if a later
   consolidation run extracts the same real-world fact under a
   *different* key name (the file's own doc comment cites a real prior
   incident: `concurrency_limit` vs. `concurrent_jobs_per_node`), the
   old key is never touched. Both sit side by side in the same entity's
   attributes forever.
2. **Summary supersession is scoped to the exact same `(level, period)`
   pair, never across periods.** `RunDaily` looks up
   `r.currentSummaryID(ctx, tx, scope, "daily", period)` — only a
   summary for that *same calendar day* can ever be marked as
   superseded by a new one. A later day's summary that directly
   contradicts an earlier day's summary has no mechanism to say so;
   they're different `period` values by construction, so the
   supersession check never even considers the earlier one.

**Why Gap 1's fix direction doesn't help here, and could hurt**: Gap 1
is about *omission* — getting more real facts written down. Gap 3 is
about *contradiction* — two real facts already got written down, and
nothing resolves which one wins. A fix that only makes consolidation
preserve more facts, without also addressing precedence, increases the
surface area for Gap 3 rather than reducing it.

### Gap 4 — Two mechanisms, one now confirmed real, one still unobserved

1. **Retrieval has no temporal-relevance signal.** Ranking is pure
   embedding similarity to the question's words; nothing boosts a
   candidate because its `period` label actually overlaps a timeframe
   the question implies ("last month," "since my trip"). A
   textually-similar but temporally-wrong summary could in principle
   outrank the correct one even with perfect, non-contradictory
   consolidation. Still unobserved — every real failure traced this
   session happened to be explained by Gap 1 or a retrieval-admission
   gap first, so this mechanism has never been isolated. Not designed or
   built; would need a real example before doing either, per this
   document's own discipline.
2. **Raw aggregation errors at answer time — ✅ confirmed real** (not
   merely plausible). Real-verified against the charity-events case
   once Phase D items 1-2 made both needed facts reliably present in
   context (`HUPI_DEBUG_FUSION` and direct string search both confirm
   this): the model still answers "no information available" even
   though everything it needs — both dated charity-event facts — is
   there for it to find. This isn't a retrieval or consolidation problem
   anymore; it's the model failing to (a) recognize which two of
   several charity-event mentions are "the pair on consecutive days" and
   (b) compute the requested duration from that pair.

   **A prompt-only fix was tried and reverted, twice, escalating
   specificity each time** — matching the outcome of every other
   prompt-only attempt at a consolidation/retrieval-adjacent problem in
   this document:
   - First addition: a general instruction to scan every retrieved
     memory block for each separate occurrence of a multi-part question
     before answering, rather than stopping at the first one found. No
     measured effect — answer unchanged.
   - Second addition (compounding on the first, not replacing it): an
     explicit, structured instruction — for "in a row"/"consecutive
     days" phrasing specifically, list every occurrence found with its
     own date, then check each pair for exactly one calendar day apart.
     Still no measured effect.
   
   Both reverted (`git checkout`) rather than left in place with zero
   demonstrated benefit, the same discipline as every other disproven
   change in this document. This looks like a genuine model-capability
   limit on a real multi-step task (find N candidates across a large,
   generically-similar context, pair them by date, then compute a
   derived quantity from the winning pair) rather than something prompt
   wording alone can close — consistent with this mechanism's own
   original framing as "partly a raw-model-arithmetic reliability limit
   no prompt fully closes." Not chasing a third prompt iteration; if
   this needs fixing, the honest next step is a different mechanism
   entirely (e.g., a dedicated multi-fact-aggregation reasoning pass
   with visible intermediate steps, not a single-shot concise-answer
   prompt) — not scoped further here.

## Why these are one family of problem, not four unrelated ones

Every one of Gaps 1-3 is the same architectural fact wearing a
different face: **consolidation makes one irreversible, non-idempotent
LLM judgment call per period, with no mechanism to notice its own
omissions, no mechanism to detect when a new judgment call contradicts
an old one, and no mechanism to connect judgment calls across period
boundaries.** Gap 1 is that judgment call under-including. Gap 3 is two
judgment calls disagreeing with neither one aware of the other. Gap 2 is
a question needing two judgment calls combined when nothing combines
them. Fixing the shared root cause — giving consolidation a way to
check its own completeness and consistency, not just produce a single
best-effort pass — is the real fix; Gaps 1-3 are three symptoms of
skipping that.

## Exhaustive counter-plan

Ordered by dependency, not just cost — later phases assume earlier ones
have actually landed, the same lesson Gap 2's retrieval-widening
re-test already teaches (don't widen retrieval into a pool that's
empty for a structural reason).

### Phase A — cheap, no architecture change, ship regardless of what else happens

1. **Revert the failed Gap 1 prompt-only fix**
   (`internal/consolidation/prompts.go`'s `summarySystemPrompt` change)
   once the 4th verification instance confirms the pattern — a change
   with a confirmed null effect on 3 (likely 4) of 4 real tests
   shouldn't stay in the codebase as apparent, unproven mitigation.
2. **Category 3 Phase 1 (recency-preference answer prompt)** — not yet
   implemented. When multiple retrieved sources present different
   values for what looks like the same fact, instruct the answering
   model to prefer the most recently dated one. Uses date labels
   already attached to every source in context today — no new
   plumbing, same pattern as the existing "double-check WHO it's about"
   instruction in `qaConcisenessPrompt`. Doesn't fix Gap 3's underlying
   data model (both stale and current values still sit in the same
   entity/summary set), but it's a real, same-day mitigation for
   exactly the failure mode Wells Fargo demonstrates.
3. **Gap 4's defensive date-arithmetic prompt instruction** — ship
   alongside (2) since it's the same class of change (answer-time
   prompt, zero architecture risk) even without a confirmed example
   driving it.

Verification: real re-check against the Wells Fargo scope and the
temporal-reasoning examples already identified, same `-answer-only`
methodology used throughout this investigation.

### Phase B — Gap 1's real fix: structural, not a prompt nudge

The prompt-only attempt's clean failure across 3-4 real tests means the
next attempt needs to change what the model is actually asked to do,
not just what it's told. Two candidate designs, cheaper first:

1. **Topic-clustered batch consolidation for busy days.** When a day's
   session count exceeds a threshold (needs real calibration — the
   failing examples ranged 10-50+ sessions; light days with 1-3
   sessions clearly don't need this), cluster sessions by topic
   (reuse existing embedding infrastructure — sessions already get
   embedded for other purposes) before summarizing, and run
   `generateSummary` once per cluster instead of once over the whole
   day. Each real topic gets its own dedicated LLM call with no other
   topic competing for space in the same output, directly attacking the
   confirmed mechanism (the model picks one dominant topic when
   everything is in one call). More LLM calls per busy day (real,
   bounded cost — proportional to distinct topics, not session count).
   Daily summary storage stays one row per day; the clusters' outputs
   would need merging into that row's `key_facts`/`summary` without a
   further LLM compression pass (a mechanical concatenation, not
   another summarization call, so this step doesn't reintroduce the
   same dilution risk it's fixing).
2. **Decoupled per-episode fact extraction. Confirmed needed, not yet
   built.** (1)'s cap-based clustering was real-verified to recover 3 of
   4 known cases cheaply, but hit a real, confirmed ceiling on the
   Ibotta case specifically — see Phase D item 3's own status for the
   real evidence (raising the cap from 6 to 10 still wasn't enough; that
   day's real topic diversity exceeds even the raised cap). A
   lightweight pass per episode — not per day, not per cluster —
   asking "does this contain anything a future question might need,"
   entirely independent of whatever the day's narrative summary
   becomes. Most robust against dilution by construction (every episode
   gets its own dedicated consideration regardless of how busy its day
   was, or how many genuinely distinct topics that day held), but
   doubles episode-processing cost across every day, not just busy
   ones, and is a bigger structural change to how `key_facts` get
   produced. Not started.

Verification: re-run the exact same 4 real examples this document's Gap
1 section documents as failing (`gpt4_45189cb4`, `gpt4_e072b769`,
`b46e15ed`, `gpt4_e061b84f`) from fresh scopes, same methodology as the
now-completed prompt-only test. A real fix should recover the NFL
playoffs, Ibotta, and both charity-event facts into their respective
daily summaries — check via `hupi-export-memory`, not just the final
QA answer, so a retrieval-side gap doesn't get conflated with a
consolidation-side one the way the original Category 2 diagnosis did.

### Phase C — Gap 3's real fix: contradiction detection at consolidation time

Two sub-problems, matching the two root causes found:

1. **Same-fact-different-key-name detection for entity attributes.**
   Before merging a new extraction's attributes over existing ones,
   check whether an incoming key plausibly restates an existing key
   under different naming (semantic similarity between key names,
   possibly combined with checking whether the *values* are consistent
   with "an update to the same fact" vs. "a genuinely different fact").
   High false-positive risk if done naively — needs its own design pass
   specifically on the detection heuristic, not just the mechanical
   "supersede when detected" plumbing, which can mostly reuse
   `upsertEntities`'s existing `replace` path.
2. **Cross-period summary supersession.** Extend `RunDaily`'s
   supersession check beyond the exact same `(level, period)` pair —
   when a new day's consolidation produces a key_fact that plausibly
   contradicts a key_fact in a *different* day's current summary
   (same entity/topic, conflicting value), mark that relationship
   explicitly, the same `supersedes`/`correction_reason` mechanism
   `Runner.Correct` already uses for human corrections, but system-
   triggered. This needs a definition of "plausibly contradicts" that's
   specific enough not to false-positive on two summaries that just
   happen to both mention the same entity without conflicting (e.g. two
   different, both-true facts about the same person, exactly the
   multi-candidate problem already flagged in Category 1's own
   ranking gap) — likely the same underlying detection mechanism as
   (1), applied to `key_facts` instead of entity attributes.

This phase is explicitly the most invasive in this document — both
sub-problems need a real design pass on detection precision before any
code, matching this document's own "verify before design, design before
code" discipline. Not scoped down to a concrete implementation here.

### Phase D — Gap 2's real fix: cross-day stitching, now confirmed needed

Phase B's real verification (see Status above) directly confirmed this
phase is necessary, not speculative — recovering the previously-missing
facts exposed three concrete, distinct follow-ups, the first two
belonging here:

1. **Budget-aware context assembly, not naive tail-truncation. ✅
   Implemented and real-verified.** `fusedSearchSummaries` now renders
   in two real passes over every picked summary — every summary's one
   guaranteed fact (`guaranteedFact`) is written first, across all
   picked summaries, before any summary's "extra depth" (`depthText`);
   depth itself writes every key fact uncapped (they're already short
   and atomic by design) followed by a tightly-capped prose snippet
   (`proseDepthCap=500`), not the other way around. Getting there took
   two real, disproven intermediate designs, both found by testing
   against the actual charity-events scope rather than trusting the
   design on paper:
   - First attempt interleaved each summary's guarantee immediately
     followed by its own depth, one summary at a time — still let an
     earlier summary's depth section push a *later* summary's guarantee
     past the truncation point once several summaries were picked.
     Fixed by making guarantee and depth two genuinely separate passes
     (`loadKeyFacts`/`writeKeyFacts` split out of the old
     `appendKeyFacts` to make this possible without querying twice; pass
     2 doesn't repeat the "related memory (summary id...)" header, so
     `TestRetrieve_FusedSearchLabelsSummaryFoundByBothMechanisms`'s
     exactly-once invariant still holds).
   - Second attempt capped prose+facts together per summary
     (`summaryDepthCap`) — still let prose alone exhaust the cap before
     ever reaching a fact several bullets down a Phase-B-clustered day's
     20+-fact list, even though every fact was individually far cheaper
     than the prose crowding it out. Fixed by uncapping facts entirely
     and only capping prose.

   Real re-verification against the charity-events case (`HUPI_CONTEXT_CHAR_BUDGET=20000`,
   `HUPI_MAX_VECTOR_RESULTS=12` so both needed daily summaries are even
   candidates — see item 2 below for why the widened pick count itself
   is still a separate, unfixed gap): both previously-truncated facts
   (the Feb 14 and Feb 15 charity events) are now confirmed present in
   the assembled context, verified by locating their exact text and
   surrounding position in the retrieved string, not just a final
   answer check. The NFL-playoffs case (a single-summary question,
   default settings) remains fully fixed, confirming no regression.

   **This exposed a new, final-stage, genuinely different gap**: with
   both facts now reliably in context, the answer is still "No
   information available" — a real answer-time reasoning/aggregation
   failure (recognizing two facts as "the two consecutive-day events"
   and computing the month difference), not a retrieval or
   context-assembly problem anymore. Not yet designed or fixed — this
   is Gap 4's "raw aggregation/arithmetic errors at answer time"
   mechanism, now confirmed real (previously only flagged as plausible
   but unobserved) rather than a new item.
2. **Re-test retrieval breadth widening for detected multi-event/
   cross-day query shapes. ✅ Implemented and real-verified.** The
   original Category 2 cause 1 attempt widened the wrong stage —
   `mmrSelect`'s final pick count — when the real bottleneck (confirmed
   by `HUPI_DEBUG_FUSION`: every candidate had `vectorRank=-1`) was
   upstream: the vector fetch's own `similarityThreshold` filter, and to
   a lesser extent the keyword/fetch caps, discarding candidates before
   they ever became eligible to be picked. This attempt widens the
   threshold and caps themselves, mirroring
   `recommendationEntitySimilarityThreshold`'s exact, already-proven
   pattern but applied to summaries: `fusedSearchSummaries` now takes
   explicit `similarityThreshold`/`maxResults` params (default
   `vectorSimilarityThreshold`/`maxVectorResults()`, unchanged for every
   ordinary query); a new `looksLikeOrderingRequest` detector
   (`orderingKeywords`, same cheap substring-match pattern as
   `looksLikeRecommendationRequest`, grounded in the real failing
   questions) switches in `orderingSummarySimilarityThreshold=0.25` /
   `orderingSummaryMaxResults=15` when a multi-event/ordering-shaped
   question is detected. 3 new unit tests (`ordering_test.go`).

   Real verification against the sports-order case: `HUPI_DEBUG_FUSION`
   confirmed both previously-invisible summaries (`2023-06-02`, the
   triathlon; `2023-06-10`, the 5K) now enter the candidate pool via
   vector search (`vectorRank=0` and `1` respectively — meaningfully
   similar to the query in embedding space all along, just below the
   normal 0.40 threshold) and get picked. **The final answer is now
   fully correct**: all 3 events, in the right chronological order,
   matching the gold answer exactly. Re-checked the NFL-playoffs and
   charity-events cases for regressions — both unchanged (NFL still
   fully fixed; charity still blocked purely by the separate,
   already-documented answer-time reasoning gap, confirmed via
   `HUPI_DEBUG_FUSION` that this item's widening now engages
   automatically for that query too, without needing the manual
   `HUPI_MAX_VECTOR_RESULTS` override item 1's verification used).

   **Phase D items 1 and 2 together now fully resolve 2 of the original
   4 Phase B follow-up cases** (NFL playoffs, sports-order) end to end;
   the charity-events case is retrieval-complete but blocked on
   answer-time reasoning (Gap 4); Ibotta (item 3, `maxClustersPerDay`)
   remains unaddressed.
3. **Calibrate `maxClustersPerDay`. ✅ Investigated — real ceiling found,
   not a simple tuning fix.** Made configurable
   (`HUPI_MAX_CLUSTERS_PER_DAY`, same override pattern as
   `contextCharBudget`/`maxVectorResults`) so calibration doesn't need a
   rebuild — real re-verification against the Ibotta case at
   `HUPI_MAX_CLUSTERS_PER_DAY=10` (up from the default 6) confirmed the
   cap change took effect (10 real clusters fired, one grounding-check
   hiccup on the larger fact batch — 42 facts extracted against 40
   expected — self-corrected by consolidation's existing retry, all 40
   facts landed grounded) — but **the Ibotta fact was still completely
   absent from all 40 surviving key facts.** Counting the topics that
   *did* survive (Camille Claudel/Rodin, the user's daily schedule,
   Sikhism, a birthday party, Osprey vocalizations, bike maintenance,
   a content-moderation prompt, NYC photography, GPU hardware, BBQ/hot
   sauce, a math word problem — roughly 11 distinct topics), this
   specific day's real topic diversity is higher than even the raised
   cap, not just higher than the original default.

   **Conclusion: fixed-cap clustering has a real ceiling on extremely
   topic-diverse days, and this is one of them** — not a case where the
   right constant just hasn't been found yet. This LongMemEval day looks
   structurally different from the charity/NFL/sports-order days
   clustering already fixed: it's ~26 sessions that are each genuinely
   about a *different* single topic (LongMemEval's own "needle in
   haystack" design deliberately piles up many single-topic distractor
   sessions onto a handful of real calendar dates), not a few recurring
   themes repeated across many sessions. Clustering's core assumption —
   group by topic because a few themes repeat — doesn't hold when there's
   little-to-no real repetition to exploit; pushing the cap higher for a
   day like this converges toward "one cluster per source," which stops
   being clustering at all and just becomes Phase B's other, not-yet-built
   alternative below. Not chasing a specific cap value further — the
   plan doc's own Phase B design already named the real next step for
   exactly this failure mode (decoupled per-episode fact extraction),
   and that's where this should go, not into more calibration attempts
   on a mechanism that's shown its ceiling. Left `HUPI_MAX_CLUSTERS_PER_DAY`'s
   default at 6 (the value already real-verified to recover 3 of 4 cases
   cheaply); the override exists for whoever wants to trade real cost for
   a somewhat higher ceiling, with this finding on record that it isn't a
   full fix for the most extreme days.
4. **Make rollups re-run-aware, not write-once.** `RunRollup`'s
   `summaryExists` early-return means a week's rollup, once generated,
   never incorporates a later correction to one of its source days.
   Once Phase B and Phase C exist, a day's summary can legitimately
   change after the fact (a busy day gets re-clustered and recovers a
   previously-dropped fact; a contradiction gets resolved) — the rollup
   that already consumed the old version of that day needs the same
   re-consolidation-on-change treatment `RunDaily` already has for
   itself.

### Phase E — Gap 4's residual mechanisms, only investigate if failures remain after B/C/D

1. **Retrieval date-relevance boosting** — rank candidates partly by
   whether their `period` overlaps a timeframe the question implies,
   not embedding similarity alone. Only worth designing once Phases B/C
   have removed the confirmed dominant causes; testing this against
   data still corrupted by Gap 1/Gap 3 would repeat the same mistake
   Gap 2's original retrieval-widening attempt made.
2. Nothing else scoped here — if a real, isolated arithmetic/reasoning
   failure surfaces after the above, it's a genuine model-capability
   limit rather than an architecture gap, and the honest answer may be
   "no further fix, documented limitation," not another intervention.

## Status at time of writing

- **Phase A item 1 (revert): done.** 3 of 4 real verification instances
  (`b46e15ed`, `gpt4_e072b769`, `gpt4_45189cb4`) came back as clean,
  zero-effect failures — the exact facts each was checking for were
  still completely absent from the consolidated summaries afterward,
  unchanged from before the prompt fix. The 4th (`gpt4_e061b84f`) was
  cancelled partway through a very slow re-consolidation (each of its 24
  fabricated dates was batching 50+ accumulated test scopes from this
  session's own earlier verification runs, since consolidation batches
  by date across the whole deployment, not per-scope) rather than
  waited out — 3 independent, fully-completed real examples showing the
  identical zero-effect pattern was treated as conclusive. Reverted
  `internal/consolidation/prompts.go` back to its pre-fix state
  (`git checkout`); `go build`/`go vet`/`go test ./internal/consolidation/...`
  all clean afterward.
- Phase A items 2-3: not started.
- **Phase B (topic-clustered batch consolidation): implemented, real-verified,
  shipped.** `internal/consolidation/cluster.go` — `generateDailySummary`
  clusters a day's sources by cosine similarity (union-find, 0.60
  threshold, capped at `maxClustersPerDay=6` for real LLM-call cost)
  once session count exceeds `clusterEpisodeThreshold=8`; each cluster
  gets its own `generateSummary` call, mechanically merged (no further
  LLM pass) into the day's stored output. 7 pure unit tests
  (`cluster_test.go`), full `go test ./...` clean.

  Real verification (fresh scopes, real GPT-4.1, a clean test database
  set up specifically to avoid this session's earlier accumulated-scope
  slowdown) against all 4 known failing examples: **3 of 4 previously
  100%-absent facts were fully recovered** into their consolidated daily
  summaries — the NFL playoffs game, both charity events (2023-02-14 and
  -15), and all 3 sports events (triathlon/5K/soccer) for the ordering
  question. This is a real, structural win the Phase A prompt-only
  attempt completely failed to achieve on the same examples.

  Recovering the facts exposed three further real, distinct gaps that
  were previously invisible (there was nothing to retrieve before):
  1. **Context assembly is naive tail-truncation, not budget-aware.**
     The NFL case was fully fixed end-to-end by combining clustering
     with `HUPI_CONTEXT_CHAR_BUDGET=20000` (a single-summary case).
     The charity case (needs 2 summaries combined) still failed even
     at 20,000 chars — with more, richer summaries now qualifying,
     the lowest-ranked-but-still-needed summary gets cut off
     mid-sentence rather than every selected summary getting a
     guaranteed minimum share of the budget.
  2. **Retrieval candidate selection can still miss relevant summaries
     entirely for multi-event questions**, independent of budget — the
     sports-order case's `HUPI_DEBUG_FUSION` trace showed 2 of the 3
     needed summaries never entering the candidate pool at all (not
     merely losing a ranking; `vectorRank=-1` for every candidate, the
     same signature that originally motivated Category 2's already-
     reverted retrieval-widening attempt in
     `docs/LONGMEMEVAL_ACCURACY_PLAN.md`). Now that real facts exist to
     retrieve, that widening is worth re-testing — it was only ever
     disproven against an empty candidate pool.
  3. **`maxClustersPerDay=6` is a real, separate limit.** The Ibotta
     case's day has more genuinely distinct topics than the cap allows,
     so its fact still got merged into an under-cap, still-diluted
     cluster — confirmed via `hupi-export-memory`, the only one of the
     4 where the fact is still fully absent post-Phase-B.

  None of these three require reversing Phase B — it's a confirmed,
  real, net-positive foundation. They're follow-up work, tracked
  separately below, not blocking this commit.
- **Phase D item 1 (budget-aware context assembly): ✅ implemented,
  real-verified, shipped.** See that item's own writeup above for the
  two disproven intermediate designs before landing on the working
  one. Both charity-event facts confirmed present in context now; NFL
  case confirmed no regression. Exposed Gap 4 mechanism 2 as real (see
  below).
- **Phase D item 2 (retrieval breadth widening, re-attempted): ✅
  implemented, real-verified, shipped.** Widened the vector fetch's own
  similarity threshold and caps (the actual bottleneck, per real
  tracing), not just final-selection count (the original, correctly-
  reverted attempt's mistake). Sports-order case now fully correct
  end-to-end, gold-matching order. No regressions on NFL or charity.
- **Phase D item 3 (`maxClustersPerDay` calibration): ✅ investigated —
  real ceiling found, not a tuning miss.** Made configurable
  (`HUPI_MAX_CLUSTERS_PER_DAY`); raising it from 6 to 10 for the Ibotta
  case took effect (confirmed 10 real clusters, a grounding hiccup on
  the larger batch self-corrected via existing retry) but still didn't
  recover the fact — that day's real topic diversity exceeds even the
  raised cap. Left the default at 6; per-episode fact extraction (Phase
  B's own second design option) is the real next step for this specific
  failure mode, not further cap tuning.
- **Gap 4 mechanism 2 (raw aggregation errors at answer time): ✅
  confirmed real** (previously only flagged as plausible). Two
  escalating prompt-only fix attempts against the now-retrieval-complete
  charity case both showed zero measured effect and were reverted. Looks
  like a genuine model-capability limit on a real multi-step task, not
  something prompt wording alone closes — not chasing further prompt
  iterations; a different mechanism (a dedicated multi-fact-aggregation
  reasoning pass) would be the honest next step if this needs fixing,
  not scoped further here.
- **Net picture across the 4 original Phase B follow-up cases**: NFL
  playoffs and sports-order are now fully fixed end-to-end. Charity
  events is retrieval-complete but blocked on Gap 4's now-confirmed
  answer-time reasoning limit. Ibotta remains unfixed — blocked on
  per-episode fact extraction, not yet built.
- Phase C (contradiction detection): not started — still needs a real
  design pass on detection precision before any code, per this
  document's own note on why it's the most invasive item here.
- Phase D item 4 (rollup re-run-awareness): not started — explicitly
  gated on Phase C existing first (its own premise is "a contradiction
  gets resolved," which needs Phase C's mechanism to exist).
- Phase E: not started — gated on Phases B/C/D, per its own section.
- Per-episode fact extraction (Phase B's second design option): not
  started — confirmed necessary for the Ibotta case specifically (see
  Phase D item 3 above), not yet designed in detail.
- Category 1 (`single-session-preference`) work is separate, already
  shipped (PR #11), and unaffected by this document — see
  `docs/LONGMEMEVAL_ACCURACY_PLAN.md` for its own status and the
  still-open multi-candidate ranking follow-up (a re-rank attempt for
  that problem was tried and reverted this session after a real,
  measured regression — lexical overlap with the query can't bridge a
  vocabulary gap that's the whole reason the fact needed a widened
  search in the first place; a different mechanism, likely LLM-based
  re-ranking, is still needed there and is out of scope for this
  document).

## Non-goals

- A general-purpose "detect and resolve any factual contradiction"
  system — Phase C is scoped to the same-entity/same-topic,
  different-period case actually observed (Wells Fargo), not an
  open-ended reasoning system.
- Rewriting consolidation to be a single always-correct pass — every
  phase here is a bounded, specific mitigation for a specific confirmed
  (or, for Phase E, plausible) mechanism, not a ground-up redesign.
- Any change to `internal/store`'s retrieval-selection code before
  Phase B lands — Gap 2's own history is the concrete argument for why
  that ordering matters.
