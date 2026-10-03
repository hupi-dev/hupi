# Plan: fixing the three regressed LongMemEval categories

## Context

The 44/48 LongMemEval re-verification (`docs/BENCHMARKS.md` §7,
`docs/BENCHMARK_IMPROVEMENT_PLAN.md`) showed three categories well below
the others: `single-session-preference` (37.5%), `temporal-reasoning`
(37.5%), `knowledge-update` (12.5%) — against `single-session-user`/
`single-session-assistant` at 100% and `multi-session` at 50%. This plan
is based on directly inspecting every real failure (gold vs. predicted
answer) in these three categories, not on category labels alone — each
one turned out to be a different real problem, sometimes more than one
per category.

## Category 1: `single-session-preference` — cross-session retrieval gap

**Real finding**: every failure is the model correctly attempting a
personalized recommendation (the preference-aware prompt from
`docs/BENCHMARK_IMPROVEMENT_PLAN.md` step 1 works) but retrieval
surfacing nothing — the preference was stated in an earlier,
differently-worded session, and standard similarity search doesn't
bridge the vocabulary gap between "I like language-learning activities"
(stated once, weeks ago) and "recommend cultural events this weekend"
(asked now, no shared vocabulary at all).

**Design**: preference-like facts are exactly what `internal/store`'s
entity layer already exists to hold as standing, structured facts (not
prose) — the gap is that entity retrieval still runs through the same
similarity-ranked/thresholded path as everything else, so a
differently-worded recommendation request doesn't rank a relevant
preference entity highly enough to clear the bar.

Fix: detect a recommendation-seeking question (LongMemEval's own
`question_type == "single-session-preference"` for benchmark
measurement; a real deployment needs a general heuristic — keyword
signal similar to `stage1KeywordSignal`'s existing pattern, e.g.
"recommend", "suggest", "should I", "tips", "advice", "what do you
think I'd like") and, only for that case, widen entity retrieval
specifically: a second, lower-threshold entity pass (or simply
`entityVectorSimilarityThreshold` relaxed for this query shape) so
standing preference facts get a real chance to surface even without
close vocabulary overlap with the current question.

**Real risk to watch**: preference-like facts weren't only seen tagged
`kind=preference` in practice — the Azure VM verification session
(`skill:rust`, "user's favorite programming language") shows they land
under whatever kind consolidation happened to extract, not a single
clean category. The fix should widen entity retrieval generally for
detected preference-seeking queries, not filter by `kind=preference`
specifically — a kind-based filter would under-fire on real data.

**Status: ✅ implemented, verified against real data, widened once —
real, measured, partial improvement, not a clean benchmark win.**
`looksLikeRecommendationRequest` (`internal/store/retrieve.go`) detects
the query shape via a keyword list (`recommendationKeywords`); when
matched, `vectorSearchEntities` runs with
`recommendationEntitySimilarityThreshold` (0.25, vs. the normal 0.50) and
`recommendationEntityMaxResults` (10, vs. the normal 5) instead of the
defaults — both reasoned starting points, not yet measured the way the
file's other thresholds were.

**First real verification** (5 real LongMemEval questions, reused
already-consolidated scopes, `-answer-only`): initial read was "3 of 5
moved from blank to real content, score stayed 0/5." That first pass
turned out to be muddied by a real, separate methodology bug (below) —
see "capture contamination" for why the initial per-question comparisons
weren't fully trustworthy.

**Real methodology bug found and fixed along the way**:
`cmd/hupi-bench`'s QA-answering calls never set `X-Hupi-Capture: off` —
unlike `cmd/hupi-answer-question`, which has this exact lesson already
documented in its own doc comment ("repeated diagnostic calls against
the same run_ctx must not accumulate as real episodes"). Every
`-answer-only` re-check this whole session had been silently writing its
own answers back into the scope as new episodes, meaning a later
re-check on the same scope could retrieve an earlier re-check's own
answer as "memory." Fixed (`cmd/hupi-bench/replay.go`): `sendChatTurn`
now takes an explicit `skipCapture` argument — `true` for QA-answering
turns, `false` for session replay (which should capture normally, since
that's the real conversation history this benchmark measures recall
against).

**Keyword list widened** after the first pass surfaced a real
detector-coverage gap: `recommendationKeywords` broadened
(`"what do you think i"` → `"do you think"`, plus `"could there be a
reason"`, `"why might"`, `"any idea why"`) to catch phrasings like
"Could there be a reason for this?" that the original, narrower list
missed entirely.

**Clean re-verification** (same 5 questions, same scopes, this time with
capture correctly off): **1 of 5 now genuinely, verifiably correct** —
up from 0/5. The cultural-events question now correctly surfaces the
specific "language diversity"/cultural-exchange preference the gold
answer wants, not just *some* real fact. Real, measured progress,
confirmed by the real GPT-4o judge, not assumed.

The other 4 remain wrong, for the same real reasons the first pass
already found: the publications question's detector fires, but a
competing (wrong, topically-related) entity already outranks the
correct one regardless of how wide the net is — a ranking problem, not
a recall problem. The reunion and baking questions now surface *real*,
different preference facts about the same person (reaching out to old
friends before the reunion; a prior cookie-platter bake) rather than
blank answers, but not the one specific fact LongMemEval's own answer
key designated as canonical — the same "multi-candidate ranking, not
recall" gap as the publications case.

**One real follow-up still open, not yet built**: a genuinely different
mechanism for the multi-candidate ranking problem — widening the net
doesn't help once several true facts about the same person are all in
reach; that needs picking the *right* one, not finding *a* one. Not
designed in this pass.

**Update (2026-10-01): closed, for free, as a side effect of a different
fix.** Investigating a separate real failure (Category 2's `gpt4_45189cb4`
below) found that this category's own multi-candidate ranking problem and
that one shared the identical root mechanism: a large `summary_key_facts`
list where the right fact loses a lexical tie to an irrelevant one. Live
re-diagnosis of `1d4e3b97` (the bike-performance question above) found the
premise had shifted since this section was written — there are no
bike-related *entities* in this scope at all; the chain/cassette fact is
one of 135 `summary_key_facts` on a single busy day, and it loses a
12-way lexical tie to an unrelated ALS-research fact sharing the word
"group." [PR #67](https://github.com/hupi-dev/hupi/pull/67) built real
semantic (embedding-based) fact ranking, fused with the existing lexical
score via Reciprocal Rank Fusion, as the fix for `gpt4_45189cb4` — and,
once this scope's summary was reembedded, it promoted the chain/cassette
fact with no additional code needed here. Re-verified against the real
GPT-4o judge: **3/3 correct** (was 0/5 before). The Garmin bike computer
detail (never extracted as a key fact at all) remains a known residual,
not required for this question's gold answer to score correct.

## Category 2: `temporal-reasoning` — originally 3 hypothesized causes, real re-verification found 1

**Real finding**, from inspecting all 5 failures together (initial
surface-level diagnosis, before the deeper per-example re-verification
below corrected it):

1. **Multi-event recall gap** (3 of 5 failures: "which came first,"
   "what's the order of three events," "how many months since two
   events in a row") — these need *every* relevant episode/summary
   recalled, not the single best match. MMR's diversity selection and
   the fixed `maxVectorResults()` cap are tuned for "best answer to one
   question," which can work against exhaustively recalling several
   same-topic events scattered across different days.
2. **Compound-question abstention gap** (1 failure): asked to compare
   two things, found evidence for only one, answered based on that one
   anyway instead of recognizing the *comparison itself* was
   unanswerable. `qaConcisenessPrompt`'s existing "make your best
   specific attempt... only abstain if truly nothing relevant" pushes
   toward answering when *anything* relevant was found — correct for a
   single-fact question, wrong for a comparison where only one side of
   the comparison was found.
3. **Date-arithmetic error** (1 failure): correctly retrieved the right
   source date, then computed the wrong duration from it (a raw
   reasoning error, not a retrieval one).

**Design (original, for causes 2 and 3): superseded — see corrected
findings below.** The original plan was a targeted `qaConcisenessPrompt`
addition for each: a "double-check WHAT was actually asked" instruction
for (2), an explicit show-your-math instruction for (3). Neither was
built, because real re-verification (below) found no genuine example of
either failure mode left to fix.

**Causes (2) and (3) — corrected: not real, independent causes.** Before
writing either prompt addition, each cause's original example question
was re-run from a completely fresh, uncontaminated scope (new scope
owner, full real replay + consolidation, current code) rather than
trusting the original diagnosis at face value — the same discipline
cause (1) below is built on.

- **Cause (2)'s example** ("how many months since two charity events in
  a row, on consecutive days") still answered "no information
  available" from the clean scope. Direct inspection of the real
  haystack confirmed the two consecutive-day charity sessions genuinely
  exist (2023-02-14 and 2023-02-15), but neither date's consolidated
  daily summary mentions charity at all — both were busy multi-session
  days whose summaries cover unrelated topics instead. The model isn't
  failing to recognize an unresolved comparison; it correctly reports
  finding nothing, because consolidation never wrote the fact down. Not
  a prompt bug — the exact same consolidation completeness gap as cause
  (1) below.
- **Cause (3)'s example** ("how many weeks ago did I start using
  Ibotta") turned out to be worse than first thought: the *original*
  scope this diagnosis was based on had a daily summary dated
  2023-05-06 that doesn't correspond to any real haystack session at
  all — a near-certain leftover from the QA-capture-contamination bug
  fixed alongside category 1 (a pre-fix benchmark run's own answer,
  captured as a fake episode at the question's fabricated query time,
  then consolidated into a bogus summary). Re-run from a clean scope,
  the real 2023-04-16 session (where the user says "I've just
  downloaded Ibotta") exists in the haystack, but that day's actual
  consolidated summary is entirely about an unrelated word problem
  ("Jacob has $30...") — the Ibotta content was dropped, not
  misremembered. The clean answer is "No information available," a
  correct abstention given what consolidation actually preserved, not a
  date-arithmetic error. There was never a real arithmetic bug here —
  the original diagnosis was itself an artifact of the contamination
  bug, and the underlying gap is, again, consolidation completeness.

Net effect: this pass found **zero real, distinct examples** of a
compound-question-abstention bug or a date-arithmetic bug in this
sample. Both of the original diagnoses reduce, under clean
re-verification, to the same mechanism as cause (1). Steps 3 and 4 (the
two prompt additions) are **not being implemented** — there's no real
failure they'd fix, and shipping a speculative prompt change with no
verified target repeats exactly the mistake cause (1)'s own
retrieval-widening attempt made before it was reverted. If a genuine
compound-question or date-arithmetic failure surfaces in a future,
larger sample (with consolidation completeness already fixed), revisit
these two prompt additions then — the designs above are still
reasonable, they just don't currently have a real bug to justify
shipping them.

**Cause (1) — corrected root cause, real fix scoped differently.**
The original hypothesis (widen retrieval selection for detected
ordering/counting questions) was implemented, then real-verified against
the 3 actual failing questions — **and had zero measured effect. All 3
answers came back byte-for-byte identical to the pre-fix versions.**
This wasn't a "didn't help much" result; it was a clean signal that the
fix targeted the wrong stage. Traced precisely with a real diagnostic
(`HUPI_DEBUG_FUSION`) and a direct `hupi-export-memory` dump of the real
scope for the clearest case (`gpt4_45189cb4`, "what's the order of the
sports events I watched in January" — missing the NFL playoffs, the
third of three events):

- Every candidate the fusion trace saw had `vectorRank=-1` — vector
  search for summaries returned *nothing* for this query. Only 2
  candidates existed in the entire fused pool (via keyword search),
  both already selected. Widening the final-selection count or MMR's
  diversity penalty can't help when the candidate pool itself is this
  small — there's nothing sitting just outside the cutoff to let in.
- Decrypting and reading all 5 real daily summaries for this scope
  directly confirmed: **none of them mention the NFL playoffs at all.**
  The scope has 47 real episodes but only 5 consolidated daily
  summaries — LongMemEval's `_abs` haystack format crams many separate
  sessions onto a handful of real calendar dates (one date alone had
  16+ sessions), and the missing fact was a passing aside inside an
  unrelated (food-recommendation) message on one of those busy days.
  Whatever wrote that day's summary evidently prioritized other topics
  from that session pile-up and dropped this one — a real
  **consolidation completeness gap**, not a retrieval gap.
- The fact also isn't found via raw episode search, for the same
  underlying reason at a different layer: the episode containing it is
  dominantly *about* ordering food, with the NFL mention buried as an
  aside — the same "buried fact diluted inside longer text" problem
  this file's own similarity-threshold calibration comments already
  document for summaries, recurring at the episode level.

**This connects directly to Category 3's own finding**: both are cases
of "retrieval can't retrieve what consolidation never wrote down (or
wrote down so diluted it can't be found)" — not a retrieval-time
problem at all. The real fix belongs in consolidation (don't let a busy
multi-session day's summary silently drop minor-but-real details), the
same territory as Category 3's Phase 2, not in `internal/store`'s
retrieval-selection code. **Not designed in detail in this pass** —
flagged as a real, corrected follow-up, alongside Category 3 Phase 2,
rather than force-fitting the original (now-disproven) retrieval-side
hypothesis. The retrieval-widening code for this cause has been reverted
(see `internal/store/retrieve.go`'s `fusedSearchSummaries` — back to
always using `maxVectorResults()`/`mmrLambda()` directly, no
per-query-shape override) since it's confirmed to do nothing useful.

**Update (2026-10-01): the consolidation-completeness gap flagged above
is now closed, and closing it surfaced — then fixed — a second, distinct
retrieval-ranking gap underneath.** Fresh live re-diagnosis of
`gpt4_45189cb4` found the NFL-playoffs fact *was* now present (busy-day
dilution at the consolidation layer had since been addressed by other
work), but still wasn't reaching the model: on this scope's 125-fact
summary, the correct fact and an unrelated climate-change fact tied at
the same lexical relevance score (one shared word each — "watched" vs.
"events"), and the old stable-sort tie-break kept the wrong one.
[PR #67](https://github.com/hupi-dev/hupi/pull/67) replaced plain
lexical-overlap fact ranking with Reciprocal Rank Fusion of lexical and
semantic (embedding) rank. Real, measured result: **5/5 correct** on the
real GPT-4o judge (was 0/5). A first version of that fix (semantic
ranking overriding lexical outright) was tried and proven wrong by live
re-verification — see PR #67's own description for the real regression
it caused and why RRF fusion was used instead.

**Update (2026-10-01, round 2)**: a fresh, unbiased 6-conversation
validation (not a re-check of already-tuned cases) found a *different*
temporal-reasoning failure (`gpt4_4edbafa2`, "what date did I attend the
first BBQ event in June") answering "1 June 2023" — the resolved start
date of a monthly period rollup, not any of the three real June dates
actually in the source. Real root cause, confirmed via direct DB
inspection and live reproduction: the grounding-check batch-mismatch bug
described in `docs/CONSOLIDATION_COMPLETENESS_PLAN.md`'s own 2026-10-01
round-2 update had zeroed out the exact 20-fact batch containing the
correct June 3rd fact, making it invisible to retrieval — the monthly
rollup's own start-date was the only day-adjacent signal left for the
model to fall back on. Fixed in the same
[PR #71](https://github.com/hupi-dev/hupi/pull/71). Reconsolidating this
scope and re-answering confirmed **"3 June 2023" (gold: "June 3rd"), 3/3
trials**, no longer the period-start fallback.

## Category 3: `knowledge-update` — no recency precedence across periods

**Real finding, confirmed directly against the real database** (not
guessed): two independent mechanisms, both real, both already documented
in code comments as previously-observed production issues:

1. **Entity attributes merge per-JSON-key, last-write-wins** — if a
   later day's extraction states the same real-world fact under a
   *different* key name than the original (e.g. `pre_approved_amount`
   vs. `preapproval_amount`), the old key is never overwritten. Both
   values sit side-by-side in the same entity's attributes blob with no
   precedence between them.
2. **Summaries only supersede within the same period** — a later
   calendar day's summary is a different period from an earlier day's
   and never supersedes it, even when it directly contradicts it. Both
   stay permanently "current." Confirmed live: the real failing case
   (a Wells Fargo mortgage pre-approval amount) has exactly two real
   summaries in the database, dated months apart, referencing the same
   entity, neither superseding the other.

**Design — two phases, cheap-first**:

- **Phase 1 (cheap, prompt-level)**: reuse the date-labeling
  infrastructure already shipped (`docs/BENCHMARK_IMPROVEMENT_PLAN.md`
  step 5 — every source in context is already labeled with its own
  date). Add an instruction to the answer-time prompt: when multiple
  sources present different values for what looks like the same fact,
  prefer the most recently dated source. This doesn't fix the
  underlying data model, but it's a same-day, low-risk mitigation using
  data that's already present in context.

  **✅ Shipped (2026-10-01), [PR #69](https://github.com/hupi-dev/hupi/pull/69).**
  Also moved this prompt (previously duplicated verbatim between
  `cmd/hupi-bench` and `cmd/hupi-answer-question`) into a new
  `internal/qaprompt` package as part of the same change, closing a real,
  live drift risk discovered along the way. The exact instruction is
  guarded against several real false-positive shapes found investigating
  the Wells Fargo case directly (a value mentioned only in passing, one
  repeated more often or ranked "most relevant," an entity's own "last
  updated" date not dating each individual value within it, genuinely
  different facts on the same topic, and "what was..." past tense alone
  not meaning "give the original").
- **Phase 2 (real fix, consolidation-time)**: extend contradiction
  detection into consolidation itself — when a new day's extraction
  produces a fact that plausibly updates an existing entity attribute or
  summary key_fact, explicitly detect and mark the supersession, the
  same way `Runner.Correct`'s `replace=true` already does for human
  corrections, but automatically, as part of ordinary daily
  consolidation. This is the deeper, more invasive change of everything
  in this plan — needs its own design pass (how "plausibly the same
  fact, different key name" gets detected without false-positiving on
  genuinely different facts) before implementation, not something to
  build in the same step as Phase 1.

  **Update (2026-10-01): this mechanism already existed** (built as
  Phase C sub-problem 2 in `docs/CONSOLIDATION_COMPLETENESS_PLAN.md`,
  before this plan's own Phase 2 was ever started) — the real Wells Fargo
  failure wasn't a missing mechanism, it was two bugs in that mechanism's
  own candidate selection, found and fixed in
  [PR #69](https://github.com/hupi-dev/hupi/pull/69): a hub entity
  (`person:user`, present in nearly every summary) crowded out the one
  summary sharing the actual rare, specific entity
  (`organization:wells-fargo`) out of a bounded top-5 candidate list, and
  a backwards "current summary" filter (`supersedes is null`, should be
  "nothing newer supersedes this one") meant corrected rows were
  permanently excluded from ever being re-compared while their stale,
  superseded predecessors kept being re-checked. Both are independently
  proven fixed via dedicated regression tests. Live end-to-end
  verification against the real Wells Fargo scope remained inconclusive
  on its own — the underlying fact's extraction and grounding reliability
  (see `docs/CONSOLIDATION_COMPLETENESS_PLAN.md`'s own 2026-10-01 update)
  dominates that specific outcome, independent of whether the right
  candidate pair gets compared at all. See
  `docs/CONSOLIDATION_COMPLETENESS_PLAN.md`'s Gap 3 section for the full
  detail on the underlying mechanism this extends.

  **Update (2026-10-01, round 2)**: the same fresh 6-conversation
  validation found a different `knowledge-update` failure (`07741c45`,
  "where do I currently keep my old sneakers?" — location changed from
  "under the bed" to "a shoe rack in my closet" across two sessions)
  answering "not mentioned," recovering *neither* value. Real root cause:
  the same grounding-check batch-mismatch bug as Category 2's update
  above (`docs/CONSOLIDATION_COMPLETENESS_PLAN.md`'s round-2 follow-up)
  had zeroed the batches containing both the old and the updated
  location fact on different days, so this mechanism's own contradiction
  detection never had two real facts to adjudicate between in the first
  place — a recall/grounding gap upstream of Phase C's supersession
  logic, not a defect in it. Fixed in the same
  [PR #71](https://github.com/hupi-dev/hupi/pull/71). Reconsolidating
  both affected days and re-answering confirmed **"Shoe rack in your
  closet" (gold: "in a shoe rack in my closet"), 3/3 trials.**

## Category 4 (added 2026-10-01, round 2): `single-session-assistant` — multi-milestone timeline extraction gap

This category wasn't in this plan's original scope — the 44/48
re-verification above found it at 100%. The fresh, unbiased 6-conversation
validation found a real, new failure in it: `5809eb10` asked what year a
house's construction began, from a single session where the user pasted a
legal-case summary stating 5 different years across 5 different
milestones of one case (construction began 2014, contract signed 2015,
completed/keys received 2016, case decided/cited 2021). The predicted
answer, "2020," matched none of them.

**Real finding**, confirmed by decrypting and reading the actual stored
memory for this scope/day: "construction began in 2014" was never
extracted at all — neither as a key_fact nor in the summary prose — while
the 2015 (signed) and 2016 (keys received) milestones, from the exact
same source paragraph, were. Chunking and grounding were both checked and
ruled out (the real source text fits in one chunk; this day's summary had
0 ungrounded facts). Neither extraction prompt had guidance for "several
dated milestones of one story" — the closest existing instruction covers
attributing a detail to the assistant vs. the user, a different axis
entirely.

**Fix**, [PR #72](https://github.com/hupi-dev/hupi/pull/72): both
`perEpisodeFactPrompt` and `summarySystemPrompt` now explicitly require
each milestone of a multi-milestone timeline to be extracted as its own
fact, naming the specific milestone so one doesn't silently absorb or
replace another.

**Real, measured, partial result, reported honestly**: live replay of the
real source episode through the real model (3/3 trials) confirms "The
construction began in 2014" is now extracted as its own fact. Reconsolidating
the real scope with the fix moved the final predicted answer from a pure
hallucination ("2020", matching nothing in the source) to a grounded
"2015" — the fact now exists, correctly, in storage, but wasn't reaching
the model's context at all (confirmed absent from the real assembled
context via `-retrieved-context-out-file`).

**Follow-up investigation found and fixed one real contributing cause**:
direct measurement of this exact query against this real 62-fact summary
found `factScores` (the lexical half of `rankKeyFacts`'s RRF fusion, see
Category 2's own update above) gave the generic case-description fact a
higher score (7) than the answer fact (3) purely because it repeated more
of the question's own common, widely-shared vocabulary ("Bajimaya",
"Reward Homes Pty Ltd", "case") — the answer fact's genuinely rare,
distinguishing terms ("house", "began") counted no higher than any other
shared word under the old flat overlap count. Fixed in
[PR #75](https://github.com/hupi-dev/hupi/pull/75): `factScores` now uses
BM25 (IDF-weighted, already relied on elsewhere in this file for
summaries/episodes/entities) with length normalization disabled
specifically for key facts (see that PR's own description for why).

**Still not fully resolved, reported honestly**: even with that real
lexical-scoring bug fixed, the final predicted answer for this exact
scope/question is still "2015," not "2014." Direct measurement found two
further, compounding factors, neither touched by PR #75: (1) this
specific fact's real OpenAI `text-embedding-3-small` similarity to this
query is measurably weaker (0.5456) than several longer, more
generically-matching case-description facts (0.60-0.69), dragging down
its fused RRF rank despite now having the strongest lexical score — the
same class of real embedding-model limitation `rankKeyFacts`'s own doc
comment already documents for a different case; and (2) `summaryDepthCap`
(700 chars) still truncates the depth section before reaching this fact's
now-improved-but-not-top-3 rank, because several longer, less-relevant
facts ranked just above it consume the budget first. Both are real,
distinct, deeper tuning questions (RRF fusion weighting, and the
depth-budget/fact-length interaction) that would need their own dedicated
investigation and re-verification against every already-tuned case (the
same discipline that caught PR #75's own length-normalization regression
risk against `gpt4_45189cb4` before it shipped) — recorded here as a
known, deliberately not-further-chased residual, not glossed over.

## Category 5 (added 2026-10-01, round 2): `multi-session` — cross-scenario figure conflation at answer time

Also outside this plan's original scope (44/48 re-verification had this
at 50%, not chosen for that round). The fresh 6-conversation validation's
`multi-session` instance, `09ba9854_abs`, asked a bus fare from Narita
airport to a Shinjuku hotel; the predicted answer, "about ¥4,000 (bus
¥3,200 vs taxi ¥7,000)," paired a genuine, scenario-matched Narita bus
fare (from the session that specifies Narita+Shinjuku) with a genuine but
wrong-airport (Haneda) taxi estimate from an earlier, more generic
session, and did clean arithmetic on the mismatched pair. Gold is an
abstention — no bus fare was ever actually stated for this route.

**Real finding**: not fabrication from nothing — both individual figures
are real, just from two different scenarios never stated together. This
is a distinct failure shape from Category 3's "two competing values of
one fact" (already covered by the existing recency-preference
instruction in `internal/qaprompt`) — here there are two different real
facts about two different scenarios, wrongly combined into one computed
answer.

**Fix**, [PR #73](https://github.com/hupi-dev/hupi/pull/73): `qaprompt.Concise`
now instructs that computing a derived value from two figures requires
both to be stated about the exact same specific scenario named in the
question — give individually-labeled figures instead of inventing a
combined one when they don't match. Deliberately narrow: doesn't touch
the existing "best specific attempt" default or the recency-preference
paragraph.

A broader "hedged/unconfirmed advice isn't a usable fact" rule was
deliberately **not** added, even though it would likely also be needed
for this exact question to reach its gold abstention in every case — this
conversation is pre-booking brainstorming throughout, and distinguishing
that from a confirmed fact is a materially bigger, less-verified change
than the conflation guard, based on one real example. Recorded here as a
known, deliberately deferred residual, matching this plan's own
established discipline against shipping speculative prompt changes from a
single failing case.

**Real, measured result**: live verification against the real,
already-consolidated scope — 3/3 trials pre-fix confidently answered a
hallucinated figure; 3/3 trials post-fix abstain or explicitly label the
bus fare as not mentioned.

## Round 2 validation summary (2026-10-01)

The fresh, unbiased 6-conversation sample that surfaced Categories 4/5
above and the round-2 updates to Categories 2/3 started at **2/6**
correct and ended at **5/6** after PR #71/#72/#73 — `gpt4_4edbafa2` and
`07741c45` now match gold exactly (3/3 trials each); `09ba9854_abs` now
correctly abstains (3/3 trials); `5809eb10` improved from hallucination to
a grounded-but-wrong-milestone answer, with the residual fact-ranking gap
above recorded rather than chased further this round. The other 2
instances (`35a27287`, `0862e8bf_abs`) were already correct and unchanged
— `35a27287`'s verbose, hedging answer style was confirmed, by direct
reproduction against the pre-fix binary, to be pre-existing and
unaffected by any of this round's changes.

## Sequenced steps

1. Category 1 fix: query-shape detection (recommendation-seeking) +
   widened entity retrieval pass. ✅ shipped, keyword list widened after
   real verification (see Category 1's own Status block) — real,
   measured improvement (0/5 → 1/5). **The multi-candidate-ranking
   follow-up this step originally left open is now also closed (2026-10-01,
   PR #67) — see Category 1's own Status block for the full update.**
2. ~~Category 2 fix (1): query-shape detection (ordering/counting) +
   widened/less-diversity-penalized retrieval for that shape.~~
   **Attempted and reverted.** Real verification (`HUPI_DEBUG_FUSION` +
   `hupi-export-memory` trace) proved this had zero effect on the actual
   failing questions — the bottleneck is upstream, at consolidation time,
   not the final-selection stage this step targeted. See Category 2's
   own "Cause (1) — corrected root cause" block. The code
   (`looksLikeOrderingRequest`, the widened `fusedSearchSummaries` params)
   has been fully removed rather than left in place unproven.
3. ~~Category 2 fix (2): compound-question abstention prompt
   addition.~~ **Investigated, not implemented.** Real re-verification
   from a clean scope found no genuine example of this failure mode —
   the one question originally diagnosed this way is the same
   consolidation-completeness gap as step 2/cause (1), not a prompt
   issue. See Category 2's corrected causes (2)/(3) block.
4. ~~Category 2 fix (3): date-arithmetic prompt addition.~~
   **Investigated, not implemented.** The original example turned out
   to be sitting on a scope contaminated by the pre-fix QA-capture bug;
   re-verified clean, it's also the consolidation-completeness gap, not
   a raw arithmetic error. See the same corrected block.
5. Category 3 Phase 1: recency-preference prompt addition (reuses
   existing date labels). **✅ Shipped 2026-10-01, PR #69 — see Category
   3's own Design section for the full update.**
6. Category 3 Phase 2 / Category 2 cause (1): consolidation-time
   contradiction detection **and** consolidation completeness (busy-day
   summary dilution) — flagged as needing its own design pass before
   implementation, not bundled into this same sequence. This is now the
   real, dominant cause behind Category 2 as a whole (causes 1, 2, and 3
   all traced back to it under clean re-verification), not one of three
   equally-weighted causes — raising its priority relative to the
   original sequencing. **✅ Addressed 2026-10-01: consolidation
   completeness (busy-day dilution, light-day skipping) fixed in PR #68;
   the contradiction-detection mechanism's own candidate-selection bugs
   fixed in PR #69 — see both sections' own Design/Status updates above
   for what's closed vs. still a known residual.**

Steps 1 and 5 are real, bounded, independently testable changes to
existing mechanisms (query-shape detection already has a precedent in
`stage1KeywordSignal`; prompt additions follow the exact pattern
`qaConcisenessPrompt`'s own WHO-check already established). Steps 2, 3,
and 4 all turned out, under real re-verification, to target a failure
mode that isn't actually present as a distinct bug — kept here only as
a record of what was tried/investigated and why each was set aside
rather than shipped speculatively. Step 6 is explicitly sequenced last
and separately, matching this session's own established discipline of
not bundling a deep, higher-risk redesign into the same pass as cheaper,
well-understood fixes — and is now the step doing most of the real
remaining work for this category.

## Non-goals for this pass

- Step 6 (consolidation-time contradiction detection and completeness)
  was not designed in detail in this original pass — addressed 2026-10-01
  in PR #68/#69, see the Sequenced steps entries above. (Steps 3 and 4
  were investigated and set aside, not verified-then-shipped — see the
  Sequenced steps entries above.)
- Not attempting a general-purpose "resolve any factual contradiction"
  system — scoped specifically to the same-entity/same-topic,
  different-day case actually observed.
- Not changing `maxVectorResults()`'s global default — widening is
  scoped to detected query shapes (preference-seeking). The
  ordering/counting analog was tried and reverted (step 2 above).

## Verification

- Each step verified the same way this session's other retrieval work
  was: a real unit test for the new query-shape detection function
  (mirroring `mmr_test.go`/`keyfacts_test.go`'s pattern), then a real,
  bounded re-check against the actual failing LongMemEval questions
  already identified in this plan — not the full 48-instance benchmark
  again per step, matching the established "cheap re-verification before
  a full run" discipline.
- A full LongMemEval re-run only once steps 1, 5, and 6 are individually
  verified (3 and 4 no longer need this — they were never shipped), to
  get a real, final combined number.

## Round 3 fast-iteration sample (2026-10-03)

Same 6-question stratified sample as round 2 (one per category, same
question IDs, clean DB via the fast-iteration harness — see the
separately-tracked harness plan), run against `main` at commit 9558e14
(PR #92: based_in/lives_in predicate canonicalization + dashboard
relationship date-range display — unrelated to retrieval/consolidation
accuracy, included only because it was the tip of main when this round
ran).

| Category | question_id | Round 2 (3 trials) | Round 3 (1 trial) |
|---|---|---|---|
| single-session-user | `0862e8bf_abs` | correct | **correct** |
| single-session-assistant | `5809eb10` | grounded but wrong milestone (known residual) | **correct** — matches gold exactly this trial |
| single-session-preference | `35a27287` | correct (verbose/hedging style, confirmed pre-existing) | **wrong** — same verbose-style answer, judge flipped to incorrect this trial |
| temporal-reasoning | `gpt4_4edbafa2` | correct, 3/3 | **correct** |
| knowledge-update | `07741c45` | correct, 3/3 | **wrong** — hypothesis "In a shoe rack" dropped the "in my closet" specificity gold has; judge marked it incorrect |
| multi-session | `09ba9854_abs` | correctly abstains, 3/3 | **correct**, abstains |

Task-averaged accuracy: 4/6 (0.667). Judge: gpt-4o, via
`bench/score_longmemeval.sh`.

Hypothesis tested: none — this was a baseline re-check on `main` as it
stood after round 2's fixes, not a round testing a new change.

Outcome: two instances flipped relative to round 2, in opposite
directions. `5809eb10`'s long-standing "known residual" (wrong milestone)
did not reproduce this trial — it matched gold cleanly. `07741c45`
(previously 3/3 correct) and `35a27287` (previously confirmed-correct
despite being verbose) both scored wrong this trial. Given round 2's own
numbers came from 3 trials each and round 3 is a single trial per
question, the most likely explanation for all three is answer-generation
and LLM-judge variance rather than a real regression or a real fix — not
confirmed either way. **Do not treat `5809eb10` as fixed, or `07741c45`
as regressed, on the strength of this single trial.**

Known limitation this round: the planned direct-DB-decryption sanity
check (reading a stored summary for one scope to cross-check a
prediction, same method prior rounds used) could not be done. The
harness's `bench_round3.env` was regenerating `HUPI_KEK` fresh via
`openssl rand` on every `source`, rather than reusing one key across the
two sub-runs this round was split into (the background run hit this
session's time limit mid-round and was resumed with a separate
`-data-file`/`-out-file` for the 3 remaining questions). The two
ephemeral KEKs from both sub-runs were never saved, so neither scope's
data is decryptable anymore in this clean DB — this didn't affect the
predictions themselves (each sub-run's hypothesis was written to its
output file from in-process state before the key was lost), only the
after-the-fact sanity check. Fixed for future rounds: `bench_round3.env`
now persists the KEK to `bench_round3.kek` on first generation and
reuses it on every subsequent `source`.

Next: run 3 trials each for `5809eb10` and `07741c45` specifically (not
the full 6) to tell real signal from single-trial judge/generation noise
before deciding either needs further work. If `07741c45` is a real
regression, check whether it's consolidation dropping the "closet"
detail or the judge being stricter on a directionally-correct-but-less-
specific answer (the latter wouldn't be a HUPI bug at all).

### Follow-up: 1 additional trial each (2026-10-03)

Re-ran `5809eb10`/`07741c45` only (same clean DB, fresh `resetScope` per
run) for one more independent trial, intending 3 total but stopped after
trial 1 — see results below before spending the other 2.

| question_id | Trial 1 hypothesis | Result | Combined with original round 3 run |
|---|---|---|---|
| `07741c45` | "Taking up space in your closet" | **wrong** (gold: "in a shoe rack in my closet") | 2/2 wrong — both attempts land "in the closet" but drop the specific "shoe rack" container the gold answer names |
| `5809eb10` | "2014" | **correct** | 2/2 correct |

2 data points each, not the originally-planned 3 — still thin to call
either conclusively, but the direction is consistent both times for both
questions: `07741c45` leans toward a real, reproducible gap (not judge
noise — two differently-worded hypotheses both independently omitted
"shoe rack"), and `5809eb10`'s round-2 "known residual" (wrong milestone)
leans toward genuinely resolved rather than a fluke.

Next: if `07741c45` is picked up again, check consolidation's stored
summary/key-facts for this scope directly (decrypt via the persisted
`bench_round3.kek`) to see whether "shoe rack" ever survives
consolidation at all, or only the QA step is dropping it at answer time
— that distinguishes a consolidation-completeness bug from an
answer-prompt specificity issue, same diagnostic split this plan's
earlier categories used throughout.

## Round 4: fresh DB, random sample (2026-10-03)

Previous rounds reused the same 6 fixed question IDs every time, which
can't tell a real fix from a question this specific set of IDs happens
to favor. This round fully tears down and recreates the benchmark
Postgres container (`docker rm -f hupi-bench-pg` + a fresh
`pgvector/pgvector:pg16` + `schema/migrate.sh`, confirmed empty —
`episodes`/`entities`/`summaries` all 0 — before running) and picks one
**random** instance per category from the existing 48-instance stratified
sample (`bench/results/longmemeval_gpt-4.1_2026-09-26/sample_instances.json`,
8 per category), rather than the same round-2 IDs:

| Category | question_id | Result |
|---|---|---|
| single-session-user | `726462e0` | **correct** — "10%" discount |
| multi-session | `2e6d26dc` | **correct** — 5 babies, named correctly |
| single-session-preference | `75f70248` | **correct** |
| temporal-reasoning | `gpt4_93159ced_abs` | **correct** — correctly abstains (hasn't started the Google job yet) |
| knowledge-update | `852ce960` | **wrong** — hypothesis "$350,000", gold "$400,000" (Wells Fargo mortgage pre-approval amount) |
| single-session-assistant | `65240037` | **correct** — tea tree oil dilution ratio |

Task-averaged accuracy: 5/6 (0.833). Judge: gpt-4o.

This is a genuinely clean run — no leftover-scope decryption noise in the
log at all (every date logged `failed=0`), unlike round 3's runs which
had stale scopes from earlier continuations.

**Notable finding**: `knowledge-update` is now 0/2 across two
*different*, randomly-selected instances in two different rounds
(`07741c45`'s sneakers location in round 3, `852ce960`'s mortgage
pre-approval amount here) — both are "a numeric/specific value was
stated, possibly updated, and the wrong one got answered" shapes. Round
2's original "Fixed, 3/3" result was for one specific instance
(`07741c45`, and even that one has since flipped to wrong twice — see
the round 3 and follow-up sections above). Two different instances
failing independently is a stronger signal than the round 3 same-instance
repeats: this now looks like `knowledge-update` as a category still has
a real, unresolved gap, not an artifact of one question's phrasing.

Confirmed by pulling `852ce960`'s actual source sessions from
`longmemeval_s_cleaned.json`: both amounts are real, not a hallucination.
$350,000 appears once, on 2023-08-11; $400,000 appears twice, on
2023-08-30 and 2023-11-30 — i.e. the pre-approval amount was genuinely
updated, and HUPI's answer surfaced the older, superseded value instead
of the current one. This is the same shape of bug Category 3's
recency-preference work (Phase 1, PR #69) already fixed for a different
category — worth checking first whether that mechanism simply doesn't
cover `knowledge-update`'s retrieval path, before designing anything new
specific to this category.

Next: trace `852ce960` through consolidation/retrieval directly (same
`HUPI_DEBUG_FUSION` + `hupi-export-memory` method Category 2 used) to
see whether both the $350k and $400k facts made it into storage at all,
and if so, which one retrieval/fusion is ranking first — that tells
whether this is a storage-completeness gap (one fact never got written)
or a ranking/recency gap (both written, wrong one wins).
