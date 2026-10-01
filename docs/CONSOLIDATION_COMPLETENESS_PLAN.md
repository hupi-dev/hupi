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

### Gap 4 — Two mechanisms, both now confirmed real

1. **Retrieval has no temporal-relevance signal — ✅ confirmed real,
   fixed (Phase E).** Ranking was pure embedding/keyword similarity to
   the question's words; nothing boosted a candidate because its
   `period` label actually overlapped a timeframe the question implied
   ("last month," "since my trip"). Confirmed via a deliberately
   adversarial synthetic test, not assumed: a textually-similar but
   temporally-wrong summary did outrank the correct one (fused 1.0 vs.
   0.667) for exactly this reason. See Phase E's own status writeup
   below for the fix (`internal/store/temporal.go`) and a second, real
   bug its verification exposed and fixed along the way (`mmrSelect`
   silently discarding relevance order whenever nothing needed to be
   discarded).
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

The most invasive item in this document — both sub-problems below now
have a concrete, grounded design (verified against the actual schema
and `Runner` code, not just described in the abstract), but neither is
built yet. Detection precision is the real risk in both: a false
positive silently hides a real, valid fact from retrieval by marking it
superseded; a false negative leaves Gap 3 unfixed. Both designs
deliberately reuse the *same* detection mechanism — asking the
consolidation LLM, which already has the right context loaded, rather
than a standalone heuristic — because this session's own "1b"
re-ranking attempt already showed plain lexical/heuristic scoring
doesn't reliably judge semantic sameness.

1. **Same-fact-different-key-name detection for entity attributes. ✅
   Implemented and real-verified.** `upsertEntities`
   (`internal/consolidation/store.go`) already loaded an entity's
   *existing* attributes before merging in a new extraction's — that
   read is exactly the context a human would need to judge "is this key
   restating an existing one under a different name." Implementation:
   `EntityUpdate` (`internal/consolidation/types.go`) gained an optional
   `SupersedesKeys []string` field in the consolidation LLM's structured
   output; a new `findKnownEntities` (`internal/consolidation/supersession.go`)
   does a cheap, read-only scan (mirroring `internal/store/retrieve.go`'s
   `stage1EntityMatches` pattern, just in the opposite direction) for
   existing entities whose name appears in a day's own raw episode text,
   run inside `RunDaily`'s existing quick-reads transaction; their
   current attributes are fed into the consolidation prompt
   (`buildSummaryPrompt`) as a new "KNOWN ENTITIES" block, the same
   continuity role `establishedRecord` already plays for a day's own
   prose. `upsertEntities`'s merge step deletes each named superseded key
   before merging, instead of the flat overlay that let both sit side by
   side forever. No new LLM call — folds into the single existing
   consolidation call, scoped to `RunDaily` only (not `RunRollup`, which
   summarizes summaries rather than raw episode text where entity names
   literally appear). 5 new unit tests, including a real
   `findKnownEntities` DB-integration test.

   **Real end-to-end verification with real GPT-4.1** (not just the
   hand-constructed unit tests): a synthetic two-day scenario mirroring
   the actual Wells Fargo case — day 1, "pre-approved for a $250,000
   mortgage from Wells Fargo"; day 2, "Wells Fargo... bumped my max home
   loan budget to $300,000" (deliberately different phrasing to avoid
   just testing whether the model reuses one key name by coincidence).
   The model genuinely populated `supersedes_keys` on day 2
   (`superseded_keys=[mortgage_preapproval_amount mortgage_preapproval_date]`,
   confirmed via a new permanent diagnostic log line), and the final
   entity attributes are clean — only the updated $300,000 value, no
   stale duplicate key sitting alongside it. This is a real, live
   confirmation of the exact failure mode Wells Fargo demonstrated, not
   a synthetic pass against hand-fed JSON.
2. **Cross-period summary key_fact supersession. ✅ Core mechanism
   implemented and real-verified; two real follow-up gaps found by that
   same verification.** Real schema check:
   `summaries.entities_touched` is already a stored, queryable column,
   so "which other current summaries touch the same entities as today's
   new one" is a real, existing query (`entities_touched && $1::text[]
   and supersedes is null`), not something needing a migration. Real
   mechanism check: `internal/store/retrieve.go`'s current-summary
   filter (`not exists (select 1 from summaries newer where
   newer.supersedes = s.id)`) never actually constrains the superseding
   row to the *same* period as what it supersedes — that constraint
   only exists in `RunDaily`'s own same-period lookup
   (`currentSummaryID`), not in the schema or in retrieval. Cross-period
   supersession is already structurally supported; nothing has ever
   exercised it.

   Implementation, revised from the original design during actual
   coding: `RunDaily` calls `checkCrossPeriodContradictions`
   (`internal/consolidation/contradiction.go`) after its own summary is
   durably stored. `findRelatedSummaries` looks up other current
   summaries (any level/period, capped at
   `maxRelatedSummariesForContradictionCheck=5`, most-recent-first) that
   share a touched entity. For each, a focused, separate LLM call
   (`contradictionCheckPrompt` — deliberately narrow, not full
   consolidation) compares today's grounded key facts against that
   period's grounded key facts and reports any real contradictions. The
   real implementation reuses `Runner.CurrentContent` +
   `Runner.Correct` directly, rather than calling `storeSummary` by hand
   as originally sketched — `CurrentContent` already produces exactly
   the "full existing output, ready to hand-edit" starting point
   `Correct` needs (same tool `cmd/hupi-correct` uses for human
   corrections), so this only has to surgically replace the
   contradicted fact(s) within that snapshot and hand it to the
   existing, already-safety-checked `Correct` path (it independently
   confirmed a real, useful side effect: `Correct`'s "reject an
   already-superseded target" guard, built for human operators, fired
   correctly here too when two unrelated seed scopes' summaries had
   already been corrected by an earlier test run — no code changes
   needed for that safety net to already cover this new, automatic
   caller). Best-effort: any failure in this whole step is logged, never
   propagated as a `RunDaily` error, matching
   `entitiesMissingEmbeddings`'s own "supplementary, not blocking"
   precedent.

   The explicit "not just relate to the same entity/topic" instruction
   is the real guard against Category 1's already-known false-positive
   shape (two different, both-true facts about the same person aren't a
   contradiction) — asking the model to distinguish "update" from
   "unrelated additional fact," the same judgment `establishedRecord`'s
   existing prompt already asks it to make for same-period continuity,
   just extended across periods.

   **Real end-to-end verification with real GPT-4.1**: the same
   synthetic Wells-Fargo-style two-day scenario used for sub-problem 1,
   run through the full real `RunDaily` pipeline (not a hand-constructed
   unit test). The mechanism fired correctly — the older day's summary
   was genuinely superseded by a real, system-authored correction, with
   the corrected `key_facts` reflecting the new $300,000 value instead
   of the stale $250,000 one. The safety-check side effect above was
   also real and unplanned, a genuine bonus from reusing `Correct`
   rather than a hand-rolled write path.

   **This same verification surfaced two real, honest follow-up gaps —
   both now ✅ closed, real-verified with real GPT-4.1:**
   - **The replacement fact failed re-grounding, fixed.** `Correct`'s
     `storeSummary` call re-runs `groundingCheck` against the *old*
     summary's own original source episodes (by design, for the human-
     correction case it was built for) — but a cross-period
     replacement fact is, by construction, actually grounded in the
     *other* period's sources, not the old summary's, and came back
     `grounded: false` in the first real test. Fixed: `Correct` gained
     an `extraGroundingSourceText` parameter, appended to what it
     already builds from the old summary's own sources; a new
     `loadGroundingSourceTextForSummary` (factored out of `Correct`'s
     own existing source-loading logic, so both share one
     implementation) loads the *triggering* period's own sources for
     `checkOneRelatedSummary` to supply. Re-verified on a fresh run of
     the same scenario: the corrected fact now comes back
     `"grounded": true`.
   - **The prose summary text wasn't touched, fixed.** The original
     implementation only edited `KeyFacts` within the `CurrentContent`
     snapshot, leaving the prose paragraph itself carrying the stale
     value (verified reading "...pre-approved for a $250,000
     mortgage..." in the first real test even after the fact-level
     correction). Fixed: `contradictionCheckPrompt`'s response schema
     gained a `corrected_prose` field — a full rewrite of the old
     paragraph, not just the changed clause — and
     `checkOneRelatedSummary` now loads `CurrentContent` *before* the
     LLM call (previously after, once a contradiction was already
     confirmed) specifically so the old prose can be included in the
     prompt for the model to rewrite. Re-verified on the same fresh
     run: the corrected summary's prose is now a complete, accurate
     rewrite with no trace of the stale value.

   Both fixes are covered by the existing/updated unit tests
   (`corrected_prose` exercised in
   `TestCheckCrossPeriodContradictions_AppliesCorrection`) and by this
   real, live GPT-4.1 re-verification — not just plausible in theory.

Sequencing: (1) first — lower risk, no new LLM call, fully
self-contained. (2) only after (1) is real-verified working, since (2)
depends on the same underlying "is this really the same fact updated"
judgment being trustworthy, and is the more invasive, higher-cost, more
consequential (marks historical data superseded) of the two.

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
- **Phase C sub-problem 1 (entity attribute key-aliasing): ✅ implemented,
  real-verified, shipped.** `EntityUpdate.SupersedesKeys`,
  `findKnownEntities`/`formatKnownEntities`
  (`internal/consolidation/supersession.go`), `buildSummaryPrompt`'s new
  "KNOWN ENTITIES" block, `upsertEntities`'s delete-before-merge — all
  folded into the existing single consolidation call, no new LLM call,
  scoped to `RunDaily` only. Real end-to-end verification with real
  GPT-4.1 against a synthetic scenario mirroring the actual Wells Fargo
  case: the model genuinely populated `supersedes_keys` when the same
  fact was restated under a different key name across two days
  (confirmed via a new permanent diagnostic log line), and the final
  entity attributes are clean — only the updated value, no stale
  duplicate key. 5 new unit tests, `go test ./...` clean.
- **Phase C sub-problem 2 (cross-period `key_fact` supersession): ✅ core
  mechanism implemented and real-verified.** `checkCrossPeriodContradictions`/
  `findRelatedSummaries`/`checkOneRelatedSummary`
  (`internal/consolidation/contradiction.go`), wired into `RunDaily` as a
  best-effort post-storage step. Reuses `Runner.CurrentContent` +
  `Runner.Correct` directly rather than a hand-rolled `storeSummary`
  call — a real, unplanned bonus: `Correct`'s existing
  already-superseded-target guard (built for human operators) correctly
  protected this new, automatic caller too, with no extra code, when a
  real test scenario hit it. 3 new tests including a real end-to-end
  correction-application test. Real GPT-4.1 verification via the full
  `RunDaily` pipeline (not just a unit test) against the same synthetic
  Wells Fargo scenario: the older day's summary was genuinely
  superseded, with the corrected `key_facts` reflecting the new value.

  Both follow-up gaps found by that verification are now **✅ closed,
  real-verified with real GPT-4.1**: `Correct` gained an
  `extraGroundingSourceText` parameter (the triggering period's own
  sources, loaded via a new `loadGroundingSourceTextForSummary`,
  factored out of `Correct`'s own existing source-loading logic so both
  share it); `contradictionCheckPrompt`'s response gained a
  `corrected_prose` field, with `checkOneRelatedSummary` now loading
  `CurrentContent` *before* the LLM call (not after) so the model can
  see and rewrite the old prose, not just the facts. Re-ran the same
  synthetic Wells Fargo scenario fresh: the corrected fact now comes
  back `"grounded": true` (previously `false`), and the corrected
  summary's prose is a complete, accurate rewrite with no trace of the
  stale $250,000 value. Phase C is now fully closed, not just
  core-mechanism-complete.
- **Phase D item 4 (rollup re-run-awareness): ✅ implemented,
  real-verified end to end with real GPT-4.1.** `RunRollup`'s blunt
  `summaryExists` no-op guard replaced with a real staleness check
  (`rollupIsStale` — any source period's *current* summary created after
  the rollup itself); when stale, regenerates and supersedes it the same
  way `RunDaily` already does for a day's own draft. A new
  `refreshRollupsCovering` (the real *trigger*, since nothing in the
  natural cron cadence ever revisits a past calendar period on its own)
  finds already-existing rollups covering a corrected period and
  re-invokes `RunRollup`; `RunRollup` itself calls this again after every
  successful store, so one trigger cascades upward through
  weekly → monthly → yearly automatically. Wired into both real trigger
  points: `RunDaily`'s own same-day re-consolidation, and
  `checkOneRelatedSummary` after a Phase C correction. 3 new tests
  (staleness-triggered regeneration, idempotency preserved when nothing
  changed, and the covering-rollup lookup itself).

  Real verification: a synthetic scenario spanning a real weekly and
  monthly rollup boundary (Wells Fargo's $250,000 fact on a Monday,
  filler content the following Monday to trigger the real weekly
  rollup, the contradicting $300,000 fact three weeks later to trigger
  Phase C on the already-rolled-up day) — every level of the resulting
  hierarchy (`2023-01-02` daily, `2023-W01` weekly, `2023-01` monthly)
  came back consistently correct, all stating $300,000 with zero trace
  of the stale $250,000 value anywhere. This was the last item Phase B's
  original follow-up list flagged before per-episode fact extraction and
  Phase E (both written up next) closed out the rest of this document's
  scope.
- **Per-episode fact extraction (Phase B's second design option): ✅
  implemented, real-verified.** `internal/consolidation/perepisode.go` —
  one independent, narrow LLM call per episode on days already past
  `clusterEpisodeThreshold`, appended to whatever clustering separately
  produces (`generateDailySummary`'s busy-day branch). 3 unit tests
  (`perepisode_test.go`: attribution to the episode's own id, best-effort
  skip on a failed/malformed call, empty input).

  Real verification against the Ibotta case (the exact scenario Phase D
  item 3 found clustering alone can't reach): the first prompt version
  extracted a fact from the target episode but consistently missed the
  one specific fact the LongMemEval question actually needed — 3 real
  GPT-4.1 calls against the exact episode text all reported "Ibotta
  partnered with Thrive Market for cashback," 0/3 ever mentioning "the
  user just downloaded Ibotta" (the temporal anchor the question's answer
  depends on), even though both statements are in the same episode. Root
  cause, not guessed: the prompt's examples ("a purchase, a decision...")
  didn't cue the model that an incidental scene-setting remark early in a
  long exchange counts too. Fixed by naming that pattern explicitly and
  telling the model to be exhaustive, not selective, within one episode.
  Re-tested against the identical real episode text: 5/5 real GPT-4.1
  calls now include "The user has just downloaded Ibotta" as its own
  fact. Real end-to-end re-consolidation of the actual Ibotta day
  confirmed the same fact lands in the stored summary.

  This surfaced two further real, previously-invisible problems, both
  found and fixed in the same verification pass (see their own writeups
  below): more facts per busy day pushed an existing grounding-check
  weakness from occasional to near-total failure, and — once grounding
  was fixed and the fact was live in the database — the specific
  LongMemEval question ("how many weeks ago did I start using Ibotta?")
  still came back wrong ("9 weeks ago" vs. gold "3 weeks ago") because
  the answering model got the date arithmetic wrong even with the
  correct fact in front of it. That residual failure is the same
  already-confirmed Gap 4 mechanism 2 (answer-time reasoning, not
  retrieval) documented above for the charity-events case — not a new
  gap, and not chased further here for the same reason that one wasn't.
- **Grounding-check batching: ✅ implemented, real-verified (a fix this
  session's own per-episode verification made necessary, not originally
  scoped).** `groundingCheck` sent every fact for a whole summary in one
  LLM call, asking for an exact 1:1 boolean-per-fact JSON array; on any
  count mismatch it safe-degrades *every* fact in that call to
  ungrounded (a deliberate, pre-existing design — an ungrounded fact is
  still stored, just excluded from retrieval, so this was always meant
  to fail safe rather than destroy information). Real, measured
  discovery: on the Ibotta day specifically (26 sources, 6 clusters,
  worse once per-episode extraction adds more facts on top), 2 fresh,
  independent full real-GPT-4.1 re-consolidation runs both hit this
  mismatch on **every one of 3 scopes sharing that calendar date**,
  every single retry — not a rare fluke, and getting *worse* with more
  facts, exactly the volume per-episode extraction is designed to add.
  One run's final, persisted summary had 81 facts, 0 grounded — the
  Ibotta fact my prompt fix had just successfully recovered was
  completely invisible to retrieval anyway.

  Fixed with `groundingCheckBatchSize = 20`: `groundingCheck` now splits
  any fact list above that size into fixed-size batches, each its own
  grounding call, concatenating results — confining any remaining
  mismatch to the one miscounted batch instead of discarding the whole
  summary's grounding. 5 new unit tests (`grounding_test.go`), including
  one that specifically confirms a mismatch on one batch leaves an
  adjacent, correctly-matched batch's real results untouched. Real
  re-verification: a third fresh full real-GPT-4.1 re-consolidation of
  the same Ibotta day came back with only one small mismatch (one batch
  of 20, off by 1, in a *different* scope) instead of 3/3 scopes fully
  failing — the target scope's summary landed with **85/85 facts
  grounded**, including the Ibotta fact.
- **Phase E (retrieval date-relevance boosting): ✅ implemented,
  real-verified.** Confirmed genuinely needed first, not assumed: a
  deliberately adversarial synthetic scenario (a January "AI conference"
  session about neural networks, a May session about robotics, a
  question asking what was learned "at the AI conference... last month")
  showed via `HUPI_DEBUG_FUSION` that both candidates were retrieved
  (ruling out a candidate-pool gap, Gap 4 mechanism 1) but the
  temporally-wrong January summary's fused score (1.0, pure lexical
  match on "AI conference") beat the correct May one (0.667) purely
  because nothing in ranking was aware of either summary's own period.

  Implemented `internal/store/temporal.go`: `resolveQueryTimeframe`
  detects a small set of relative-time phrases ("last month," "this
  week," etc.) against the caller's own `now`, `parsePeriodRange` parses
  a summary's stored period string (all 4 real formats: daily/weekly/
  monthly/yearly) into a concrete range, `periodsOverlap` checks the two
  against each other. `fusedSearchSummaries` adds a flat
  `temporalRelevanceBoost = 1.0` (a full reciprocal-rank-0 contribution)
  to any candidate whose period overlaps the resolved timeframe. Since
  benchmark/synthetic testing fabricates historical "now" values (see
  `gateway.Handler.Now`'s own doc comment), a real `time.Now()` call
  inside retrieval would have been silently wrong under exactly the
  testing this whole document relies on — `now time.Time` was threaded
  through the `Retriever` interface, `Store.Retrieve`, `retrieve()`, and
  `fusedSearchSummaries` instead, with `h.now()` the one real call site.
  7 new unit tests (`temporal_test.go`), including a self-consistency
  check of `isoWeekStart` against `time.Time.ISOWeek()`'s own reverse
  mapping across a year boundary (`2020-W53`).

  Real re-verification against the adversarial scenario confirmed the
  fused score itself was fixed exactly as designed: May's candidate now
  scores 1.667 (0.667 + the boost) against January's unchanged 1.0.
  But capturing the actual assembled context (via
  `-retrieved-context-out-file`) surfaced a second, real, previously
  invisible bug this fix's own verification exposed: **`mmrSelect`'s
  "pool fits within k" shortcut returned candidates in raw insertion
  order, not sorted by relevance, whenever nothing needed to be
  discarded** — exactly the common case (most queries retrieve fewer
  candidates than the widened `summaryMaxResults`), and exactly the case
  this adversarial test hit. The fused-score fix was real, but the
  context the model actually saw still put January first, because
  nothing after fusion ever re-sorted a "select everything" pool. This
  had been silently undermining Phase D item 2's own retrieval-breadth
  widening the same way, for any query where the widened threshold
  didn't produce a surplus over `maxResults` — an existing unit test
  (`TestMMRSelectIsNoOpWhenPoolFitsWithinK`) had encoded this as
  intentional, using a pool that happened to already be in descending-
  relevance order, so nothing caught it.

  Fixed by sorting the "return everything" branch by relevance
  descending — a "no discard" guarantee, not a "no reorder" one.
  Rewrote the masking unit test with an out-of-order pool so a
  regression back to raw insertion order would be caught. Re-verified
  against the adversarial scenario: the assembled context now correctly
  presents May's (correct) summary first. The final answer is still
  wrong ("Neural networks, deep learning" instead of gold "Robotics,
  actuators, and control systems") — with both the fused ranking and the
  actual context order now fully correct, this is squarely the same
  already-confirmed Gap 4 mechanism 2 (a real model-reasoning limit at
  answer time: the model matched the literal phrase "AI conference" in
  the question to January's content over the "last month" temporal cue,
  even with the right content presented first) — not a retrieval defect,
  and not chased further here for the same reason mechanism 2's other
  instance wasn't.
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

### Status: every phase in this document's original scope is now done

All five phases (A revert, B, C, D, E) plus both of Phase B's own
follow-up options (per-episode fact extraction) are implemented and
real-verified against real GPT-4.1. Two deterministic follow-ups to the
previously-documented answer-time reasoning gap have since been
implemented and real-verified too (below) — one of the three original
reasoning-limit instances is now genuinely fixed, not just
better-understood.

- **Deterministic date-delta labels: ✅ implemented, real-verified —
  fully closes the Ibotta case.** Root cause behind two of the three
  answer-time reasoning failures (Ibotta's "weeks ago" computation, and
  part of the Phase E case): the model had the right fact and the right
  date and still did the subtraction wrong (answered "9 weeks ago"
  against a gold "3 weeks ago" for a 20-day gap). Fix moves the
  arithmetic out of the model entirely: `relativeDateLabel`
  (`internal/store/temporal.go`) computes an already-correct
  "N weeks/days/months/years before now" string from a summary's period
  or an episode's own `ts` against the query's own `now`, rounding to
  the nearest unit (not floor — 20 days rounds to "3 weeks," matching
  how LongMemEval's own gold answers phrase this), and
  `fusedSearchSummaries`/`keywordSearchEpisodes` inject it directly into
  each picked item's context line. 8 new unit tests plus 2 new
  DB-integration tests.

  Real re-verification against the actual Ibotta LongMemEval question,
  under `HUPI_CONTEXT_CHAR_BUDGET=20000` (the already-documented
  recommended setting for cloud-model deployments — the tiny 2000-char
  default truncates this specific low-ranked-but-needed fact's context
  line regardless of this fix, a real, pre-existing, already-documented
  budget limitation, not something this fix caused): the model now
  answers **"3 weeks ago"** — the gold answer, exactly. This is the
  first time in this document's entire investigation that the Ibotta
  case has been fully closed end to end, not just retrieval-complete.
- **Timeframe hard filter for summaries and episodes: ✅ implemented,
  real-verified — partial fix for the Phase E adversarial case, and a
  new, distinct residual gap found in the process.** Root cause behind
  the rest of the Phase E case and the general "disambiguate between a
  temporally-correct and a lexically-closer candidate" shape: Phase E's
  own boost (a soft ranking nudge) correctly reordered candidates, but
  a same-topic distractor sitting right next to the correct answer in
  context was still enough to pull the model's answer toward it. Fix:
  when `resolveQueryTimeframe` confidently resolves a timeframe,
  candidates whose own period/`ts` provably doesn't overlap it are
  *excluded* from context entirely, not merely deprioritized — in both
  `fusedSearchSummaries` (summaries) and `keywordSearchEpisodes` (raw
  episode keyword search, a separate, previously-untouched retrieval
  path this investigation found also needed the same fix). Backs off to
  the unfiltered set if excluding would leave nothing, matching
  `groundingCheck`'s own established safe-degrade direction. 2 new
  DB-integration tests confirm both the exclusion and the backoff.

  Real re-verification against the Phase E adversarial scenario: both
  fixed paths now correctly exclude the January content —
  `HUPI_DEBUG_FUSION` shows the January summary as
  `fused=excluded(timeframe)`, and the January episode no longer appears
  as a raw keyword-matched "related exchange" at all. But the answer
  changed to **"No information available"** rather than the gold
  "Robotics, actuators, and control systems" — an honest abstention
  instead of a confidently wrong answer, but still not correct. Tracing
  the actual assembled context found why: a **third, previously
  untouched retrieval path** — `stage1EntityMatches`, a direct
  substring/name match against the query, unconditionally rendered with
  no date awareness at all — still surfaces an entity literally named
  "AI conference (2024-01-15)" (an artifact of consolidation's own
  entity-naming), asserting a January date for the one thing in context
  labeled "AI conference." The model appears to reasonably (if
  incorrectly, for this benchmark) conclude no May "AI conference"
  exists rather than inferring that the May robotics event *is* what
  the question's phrasing refers to.

  Entities turned out to have a real, general date-scoping signal after
  all — `entities.last_updated` (schema/0001_init.sql), not just a
  one-off naming artifact — closing what first looked like a genuinely
  different, harder problem. **✅ Fixed for two of what turned out to be
  three separate entity-touching retrieval paths**: `stage1EntityMatches`
  (direct substring/name pre-check) and `keywordSearchEntities` (BM25
  over the whole entity corpus) both now apply the same hard filter
  using each entity's own `last_updated`, and inject the same computed
  date label. Both deliberately do **not** back off when excluding would
  leave nothing — unlike the summary/episode paths — because the real
  motivating case for both is exactly one coarsely-matched entity whose
  only date is wrong, and a backoff there would restore precisely the
  match the fix exists to remove; entities are a supplementary signal
  either way, with the real facts and dates living in summaries/episodes
  (where the backoff guarantee still applies). 4 new DB-integration
  tests, including one confirming a match can now correctly be excluded
  down to zero.

  **Real re-verification found this doesn't yet close the adversarial
  case, for two further reasons — one now fixed, one still open:**
  1. **`entities.last_updated` never reflected a simulated historical
     date: ✅ fixed, real-verified.** Direct inspection of the real
     database found every entity in the Phase E adversarial scope — the
     January-dated "AI conference" *and* the May-dated "robotics event"
     alike — had the identical real value `2026-09-30`, the actual
     wall-clock day the benchmark happened to run, not either entity's
     own simulated historical date. Root cause: `upsertEntities`
     (`internal/consolidation/store.go`) set `last_updated = current_date`
     — Postgres's own real clock — never wired to the date actually being
     consolidated, the way a summary's `period` column and an episode's
     `ts` column already were. Fixed with a new `periodAsOfDate(level,
     period)` (`internal/consolidation/period.go`) resolving any
     level/period into the one real calendar date it represents (the
     period's *last* day for weekly/monthly/yearly, matching how "as of"
     naturally reads), threaded into `upsertEntities`'s insert/update
     for both `first_seen` (new entities) and `last_updated` (every
     touch). 5 new unit tests plus a real DB-integration regression test
     using a date years in the past specifically so a real regression
     back to `current_date` can't pass by coincidence.

     Real re-verification: re-ran real consolidation for both the
     January and May dates in the actual Phase E scope; the "AI
     conference" entity's `last_updated` in the real database changed
     from `2026-09-30` to the correct `2024-01-15`. The two entity
     retrieval fixes above are now genuinely load-bearing for backdated
     benchmark/import scenarios, not just day-by-day production use as
     originally scoped.
  2. **`vectorSearchEntities` (the fifth and final entity path): ✅
     fixed, real-verified.** Confirmed by re-running the adversarial case
     after fix #1 above that this embedding-similarity path — the query's
     semantic similarity to "AI conference" matches even without a
     literal substring/keyword hit — was the one actually still
     surfacing the wrong entity, bypassing both already-fixed entity
     paths entirely. Fixed with the same pattern as those two (hard
     filter using `last_updated`, no backoff, same computed date label),
     now genuinely effective because fix #1 made the underlying date
     data correct. 2 new DB-integration tests, deliberately phrasing the
     query without either entity's literal name so only this path's own
     (fake-but-uniform, for test purposes) embedding similarity can find
     them — isolating it from the other two, already-separately-tested
     paths.

     **Real re-verification: the January entity is now completely absent
     from the assembled context** — all five retrieval paths
     (`fusedSearchSummaries`, `keywordSearchEpisodes`,
     `stage1EntityMatches`, `keywordSearchEntities`,
     `vectorSearchEntities`) correctly exclude it, confirmed by
     inspecting the exact context sent to the model. The final answer is
     still wrong ("Not mentioned" vs. gold "Robotics, actuators, and
     control systems") — but this is now the cleanest possible
     demonstration of Gap 4 mechanism 2: with retrieval and consolidation
     both confirmed fully correct and zero distractors present anywhere
     in context, the model still doesn't infer that a "downtown robotics
     event" is what the question's "AI conference... last month" phrase
     refers to. Nothing left in this document's own toolkit
     (consolidation completeness, retrieval ranking) can move this
     further; the answer-time reasoning gap below is the only remaining
     lever.
- **Gap 4 mechanism 2 (answer-time reasoning/arithmetic) — aggregation
  sub-case: ✅ dedicated reasoning pass designed, implemented, and
  real-verified correct; blocked on a separate, newly-identified
  retrieval-completeness gap, not a flaw in the pass itself.** The
  charity-events case (find which two of several mentions are "the
  pair," then compute the gap) doesn't reduce to a single date
  computation or a timeframe exclusion — there's no implied timeframe to
  filter by, and no single date to compute against, since it needs to
  first identify *which* two facts are the relevant pair. Two earlier
  prompt-only fix attempts against it both showed zero measured effect.

  Built as a new, narrow, best-effort pass
  (`internal/gateway/aggregation.go`), gated on the same cheap
  `looksLikeOrderingRequest` check retrieval already uses (surfaced via
  a new `RetrievalResult.NeedsAggregationPass` field — zero cost for any
  other query): one real LLM call extracts every dated fact relevant to
  the question from the already-assembled context, then pure Go code
  (no LLM judgment) pairs/orders the extracted facts and computes the
  actual elapsed time or sequence, reusing the same "shift arithmetic
  into code" approach `relativeDateLabel` already proved. The computed
  result is injected as an additional system message — the existing,
  already-tuned answer-generation call is otherwise untouched, and any
  failure anywhere in the pass degrades to no hint at all, never a
  blocked turn. 19 new unit/wiring tests, including a real regression
  test for a bug real-verification caught: the first version reported
  whatever pair was numerically closest regardless of the actual gap,
  once surfacing a genuine 16-day-apart pair as if it satisfied a
  question that explicitly said "consecutive days" — fixed with a
  plausibility cap (`maxConsecutivePairGapDays`), an honest "no hint" now
  preferred over a confidently-wrong one.

  Real re-verification against the actual charity-events LongMemEval
  question (`b46e15ed`), via `-answer-only` against its real,
  already-consolidated scope: the pass correctly declined to produce a
  hint (`HUPI_DEBUG_AGGREGATION` confirms extraction found real dated
  charity facts, but none within the plausibility cap), and the model
  answered exactly as honestly as before — no regression, no
  misleading hint. Tracing *why* no valid pair surfaced led to the
  actual root cause, investigated and closed in the same pass:

  **Root cause: stale data, not a live gap.** Direct inspection of the
  raw haystack sessions confirmed the genuine consecutive-day pair the
  question refers to really exists in the source conversation — a
  "24-Hour Bike Ride" charity event on 2023-02-14 and a "Ride to Cure
  Cancer" charity bike ride on 2023-02-15 — but neither fact survived
  into any summary this scope's retrieval could reach. The actual log
  from the overnight benchmark run that originally consolidated this
  scope showed **zero** "clustering busy day" events across all 48
  conversations, even though this exact day had 10 source episodes —
  above `clusterEpisodeThreshold` (8) and enough to trigger Phase B's
  topic-clustered consolidation and per-episode extraction, both already
  implemented and merged. The run's own timestamps (started 2026-09-29
  11:44, finished 02:56 the next morning) predate today's Phase B/PR #11
  merge (08:17:58) entirely — this scope was simply never reprocessed
  with the code that already fixes exactly this dilution pattern, not a
  new, undiscovered gap.

  **Fixed by re-consolidating, not by writing new code.** Re-ran
  `hupi-consolidate -date 2023-02-14` (4 real scopes shared this date,
  a small, bounded cost) using the current `main` binary: clustering
  correctly fired (`sources=10 clusters=6`), and the 2023-02-14 summary
  now contains the "24-Hour Bike Ride" fact, grounded. Manually computing
  the intended answer from the real, now-retrievable pair (Feb 15 to the
  question's real 2023-04-18 date is 62 days ≈ 2 months) **exactly
  matches the gold answer ("2")**.

  **Real end-to-end re-verification, 5 runs**: with both facts now
  retrievable, the full pipeline (retrieval → aggregation pass →
  answer) correctly produced **"2 months"**, matching gold exactly, in
  3 of 5 real attempts — the other 2 had the extraction step miss one of
  the two needed facts (real, expected non-determinism in a single
  best-effort LLM call, the same failure mode every other narrow
  extraction pass in this document already has) and safely abstained
  rather than answering wrong. Real-verified that the same exhaustiveness
  wording that fixed per-episode extraction's own identical recall gap
  earlier in this document improves this one too: 1 of 3 runs succeeded
  with the original extraction prompt, 3 of 5 with an added "be
  exhaustive, don't stop at one or two" instruction — kept as a real,
  measured improvement, not a guess.
- **Gap 4 mechanism 2 — semantic-bridging sub-case: ✅ fully resolved,
  and the diagnosis was wrong.** This was previously characterized as a
  "pure inference gap" — the model failing to bridge "AI conference" to
  a semantically-equivalent "downtown robotics event." Re-investigating
  found that characterization didn't hold up: real re-verification
  showed the live pipeline actually succeeded a meaningful fraction of
  the time already (roughly 50-65% across several real batches), not 0%
  — the earlier "fully open" claim was based on too small a sample taken
  earlier in the investigation.

  **Real root cause, found by isolating variables one at a time**:
  testing the exact same context and prompt at `temperature=0` (the
  model's own single most-confident completion, removing sampling
  variance as a factor) showed a stark, clean split — 10/10 correct
  against one real consolidated version of the context, 9/10 *wrong*
  ("not mentioned") against another. The difference: real consolidation
  non-determinism had, on one run, split "the user attended a robotics
  event" and "actuators and control systems were featured" into two
  separate key facts instead of one combined fact. `guaranteedFact`
  (`internal/store/retrieve.go`) — which picks exactly *one* fact to
  protect in the prominent, hard-to-truncate part of context when
  `mostRelevantFactIndex` finds no query-vocabulary overlap to break a
  tie — fell back to `facts[0]`: the bare "attended an event" fragment,
  leaving the one fact that actually carries the answer reachable only
  via the depth section. This was never really about semantic bridging
  at all; it was a content-selection lottery at the context-assembly
  layer, exposed by ordinary non-determinism in how consolidation phrases
  and splits closely-related facts run to run.

  **Fixed**: `guaranteedFactMaxCount` — below this many facts (3),
  `guaranteedFact` now guarantees all of them together instead of picking
  one; the "pick the most relevant" ranking (with its own already-known
  real limitation on larger fact sets) only kicks in above that
  threshold, where `depthText`'s own uncapped facts remain the real
  insurance policy Phase D item 1 built. 4 new/updated unit tests,
  including one pinning down the exact boundary.

  **Real re-verification**: 10 of 10 real end-to-end runs against the
  live Phase E scope now answer correctly ("Actuators, control systems"
  / "Actuators and control systems"), up from roughly 50-65% before —
  confirmed by inspecting the assembled context directly: both facts
  now appear together in the guaranteed line. This closes the last open
  item from this document's original four-gap investigation; every real,
  confirmed case this document set out to address is now resolved.
- **Category 1 (`single-session-preference`) multi-candidate ranking** —
  tracked separately in `docs/LONGMEMEVAL_ACCURACY_PLAN.md`, already
  noted below.
- **Fresh 8-conversation real rescore (post-PR #14): ✅ confirms net
  progress, surfaces one new real gap at scale.** A representative
  8-question LongMemEval sample, replayed end to end against a fresh,
  isolated DB (real GPT-4.1, real official LongMemEval GPT-4o judge):
  **3/8 (37.5%) → 6/8 (75%)**. 3 confirmed real fixes landing cleanly
  (Ibotta, Wells Fargo, backpack-delivery — all exact gold matches); 1
  question initially mis-flagged as a regression and walked back after
  checking the original run's own judge verdict (`f4f1d8a4_abs` — the
  "not mentioned" answer was graded correct both times; not a
  regression); 2 misses matching the already-documented ~40-65%
  per-call extraction-reliability ceiling (`1d4e3b97` preference,
  `b46e15ed` charity events — retrieval itself confirmed fully correct
  this run); and 1 new failure (`gpt4_45189cb4`, sports-order) that did
  not match any previously-documented gap.
- **`gpt4_45189cb4` (sports-order) deep dive: ✅ three distinct, real
  bugs found and fixed, all at the same root cause class as the
  semantic-bridging item above, recurring at a scale that item's own fix
  didn't anticipate.** Root traced to a genuine 41-episode/112-fact busy
  day — 5-6x beyond what `depthText`'s own doc comment anticipated
  ("20+ of them") — that diluted the NFL-playoffs fact this question
  needed out of easy reach three separate ways:
  1. **`writeKeyFacts` had no relevance ordering at all** — facts were
     written in raw cluster-merge insertion order, and
     `mostRelevantFactIndex`'s winner-take-all tie-break (built for
     small fact counts) gives up entirely (`-1`) once enough facts tie
     at the same lexical-overlap score, which happens far more often at
     112 candidates than at the handful it was designed against. Fixed
     by generalizing it into a full ranking, `rankFactsByRelevance`
     (`internal/store/retrieve.go`) — same scoring logic, shared via a
     new `factScores` helper, but every fact gets an order instead of
     an all-or-nothing pick; `writeKeyFacts` now writes the whole list
     in descending-relevance order (stable on ties) so if
     `truncateToBudget`'s backstop has to cut something, it cuts the
     least-relevant tail, not an arbitrary one. No facts dropped, only
     reordered. 4 new unit tests, `go test ./... -count=1` clean.
  2. **The separate aggregation-reasoning pass (Gap 4 mechanism 2, built
     earlier in this document) had its own "order" hint sub-feature,
     and it was a measured net negative once (1) existed.** The
     question's "order of ... in January" phrasing also matched
     `looksLikeOrderingRequest`, triggering this pass's own extraction
     LLM call — subject to the same already-documented ~50-65%
     per-call reliability ceiling — which missed the NFL fact roughly
     half the time and injected an incomplete "chronological order"
     hint. Real-verified the hint was actively harmful, not just
     redundant: the exact same captured context, answered directly with
     no hint, scored 15/15 correct; the real pipeline, with the
     incomplete hint present, scored 5/5 *wrong* — the model deferred
     to the incomplete list over the full, by-then well-ranked context,
     despite the hint's own "use this only if it directly answers the
     question" caveat. Fixed by removing the "order" hint path entirely
     (`resolveOrderHint` deleted, `resolveAggregationHint` now only
     handles the "consecutive pair" shape, `AggregationHint` short-
     circuits before paying for the extraction call on anything else).
     The "pair" shape keeps real value — ranking alone can't identify
     *which two* facts form a specific adjacent pair a "two events in a
     row" question needs — only "order" was a net negative once ranking
     existed to solve that shape on its own. Order-shaped questions
     still get retrieval's own breadth-widening (Phase D item 2,
     `looksLikeOrderingRequest`, unchanged) — only this pass's own extra
     hint was removed.
  3. **`guaranteedFact` — the mechanism this document's own
     semantic-bridging fix (directly above) built to survive truncation
     — still called the old `mostRelevantFactIndex` directly, not the
     new ranking.** This was the actual proximate cause once (1) and
     (2) were both fixed and the sports-order question *still* failed
     5/5: `HUPI_DEBUG_FUSION` showed the busy day's summary was picked
     (`fused=0.3333`, lowest of 3 competing summaries) but its
     `guaranteedFact` line — written first, before any depth, so it
     survives `truncateToBudget` no matter what — resolved to an
     unrelated tire-pressure fact. With 112 facts, several legitimately
     tied at the same nonzero score (sharing one query term each), and
     the old tie-break's "tied == no signal" rule discarded that and
     fell back to `facts[0]`, exactly the same bug class the
     semantic-bridging fix above already diagnosed and fixed — just not
     carried over to this second call site. Fixed:
     `guaranteedFact` now uses `rankFactsByRelevance(...)[0]` instead,
     a strict improvement (identical behavior in the true all-zero
     case, a real relevant pick instead of an arbitrary one on a tie
     among relevant facts). 1 new regression test distinguishing "all
     facts equally relevant" (existing test, unchanged) from "some
     facts tied-but-relevant, facts[0] itself irrelevant" (the real bug).

  **Real end-to-end re-verification after all three fixes**, at the
  real production-default `HUPI_CONTEXT_CHAR_BUDGET` (2000, no
  override): **5/5 correct**, all three events in gold-matching
  chronological order. Confirmed via the actual assembled context (not
  just the final answer) that the busy day's guarantee line now reads
  the real NFL-playoffs fact, not the tire-pressure one — the fix lands
  on the actual mechanism, not a lucky budget side effect. An
  intermediate diagnostic step (temporarily raising the budget to
  20,000 to isolate which of several hypotheses was the real
  bottleneck) confirmed 3/3 correct even with only the `writeKeyFacts`
  ranking fix and the hint removal in place, before the `guaranteedFact`
  fix existed — i.e. a larger budget alone was *also* sufficient, but
  the `guaranteedFact` fix solves the real mechanism at the
  already-shipped default budget, with no budget-size tradeoff.

- **A real, confirmed regression from the above, found and fixed after
  PR #15 merged — cross-summary guarantee-line budget starvation
  (`docs/MEMORY_SCENARIOS.md` scenario D).** Re-running the same real
  8-conversation sample post-merge: `gpt4_e072b769` ("how many weeks ago
  did I start using Ibotta"), previously a confirmed exact-gold-match
  fix, now consistently wrong. Root cause: 9 summaries get picked for
  this generic question; `fusedSearchSummaries`' pass 1 writes every
  picked summary's `guaranteedFact` line, highest-relevance first, with
  no per-summary cap — the Ibotta summary, lowest-fused of the 9, never
  got its guarantee line written before the global budget ran out.
  Confirmed via a budget override that the fact was present and correct
  the whole time, just unreachable in write order. The `guaranteedFact`
  fix itself is what exposed this: picking a genuinely relevant fact on
  a tie (correct) is sometimes longer than the arbitrary `facts[0]`
  fallback it replaced, shifting cumulative guarantee-line cost enough
  to push Ibotta's past the cutoff where it used to just barely fit.

  **Fix**: `guaranteeBudgetPerSummary` caps each picked summary's whole
  guarantee line to a fair, N-aware share of the budget
  (`internal/store/retrieve.go`). Two further real mistakes were found
  and fixed while building and re-verifying this, both the same
  category of error (measuring the wrong thing's cost): the first
  version capped only the fact text, not the `"related memory (summary
  <id>, dated ..., ... match): "` prefix — and this real benchmark's own
  summary IDs run ~130-160 characters, so 9 prefixes alone still
  exceeded the budget regardless of the cap. Fixed by reserving the
  real, measured prefix cost per summary before capping the fact
  (`guaranteeMinFactChars` floors the fact's own share regardless of
  prefix length). The second: `truncateToBudget`'s own
  `"...[truncated to fit context budget]"` marker (37 chars), fine once
  at the final whole-context cut, was being repeated once per picked
  summary — 9 repeats of that overhead alone still blew the budget even
  after the prefix fix. Fixed with a new `hardTruncate` (same cap, no
  repeated marker) used specifically for this per-summary case.

  **Real re-verification**: 4/4 correct on the isolated Ibotta question,
  and stable across 4 full re-runs of the whole 8-conversation sample —
  `gpt4_45189cb4` (sports-order) unaffected (still 4/4 correct),
  confirming the fix doesn't trade one failure for another the way the
  first attempt did. `go build`/`go vet`/`go test ./... -count=1` clean
  across all 20 packages; 3 new unit tests in
  `internal/store/keyfacts_test.go` cover the few-picks (unaffected) and
  many-picks (floor-respecting) cases.

  **A second, separate, still-open issue found during this
  re-verification, not fixed here**: `60bf93ed` ("how many days did my
  backpack take to arrive") has also been consistently wrong since PR
  #15 merged — confirmed present in the very first post-merge rerun,
  before this fix existed, so it's independent of the guarantee-line
  starvation above, not something this fix should have touched. Only one
  summary is picked for this question (no cross-summary competition),
  but the fact needed — "I bought it from Amazon on 1/15" — lives in raw
  episode text appended after all summary content, at position ~18,300
  in an unbudgeted dump, nowhere near the default 2,000-char cutoff.
  Confirmed via the same budget-override technique: correct ("5 days")
  once nothing is truncated. This is the same "context assembly is
  naive tail-truncation, not actually budget-aware" gap Phase B's own
  follow-up item 1 already named — this fix narrowed it for summaries'
  guarantee lines specifically; episodes, entities, and graph content
  still have no per-section budget fairness at all. Tracked as the next
  piece of the same gap, not fixed in this pass.

  **Two other apparent "regressions" checked and ruled out as real model
  variance, not bugs**: `852ce960` (Wells Fargo) and `89527b6b`
  (dinosaur color) were also inconsistent across today's runs — but
  checking every run since the PR #15 merge (12 runs each, including the
  very first, before any further fix) shows both were *already*
  inconsistent from the start (852ce960: 3/12 correct; 89527b6b: 2/12
  correct), not freshly broken by anything built today. 852ce960 in
  particular is a live, real instance of Gap 3's own acknowledged
  non-goal — two genuine, unresolved conflicting pre-approval amounts
  sit side by side in that scope's real data, and the model picks
  between them inconsistently, exactly the class of problem a fully
  general cross-fact contradiction system (explicitly out of scope) would
  address. Confirmed via direct grep across every saved prediction file
  from today, not assumed.

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
