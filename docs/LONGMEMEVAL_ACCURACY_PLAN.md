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
