# Plan: improving HUPI's real LoCoMo/LongMemEval scores

## Context

Current, real, published numbers (`docs/BENCHMARKS.md`): **LoCoMo 50.2%**
(all 10 conversations, 1,986 questions), **LongMemEval 52.1%** (48-instance
stratified sample). Both scored with each benchmark's own unmodified code,
both real GPT-4.1 + real embeddings.

This plan was written after a round of EvalMem diagnostic work
(`docs/EVALMEM_INTEGRATION_PLAN.md` §7) surfaced several ideas for
moving these numbers further. **One of those ideas turned out to already
be done**: raising `HUPI_CONTEXT_CHAR_BUDGET` to 20000 looked like an
unclaimed win from the EvalMem side (it moved that diagnostic metric
29.1% → 36.2%), but checking the actual repro commands in
`bench/results/locomo_gpt-4.1_2026-09-26/README.md` and
`bench/results/longmemeval_gpt-4.1_2026-09-26/README.md` confirms
**both published runs already used `HUPI_CONTEXT_CHAR_BUDGET=20000`**
(`docs/BENCHMARKS.md` §4's own "v5: + `key_facts` surfacing + larger
retrieval budget" step). It's already reflected in 50.2%/52.1% — not a
lever left to pull. Recorded here so the mistake isn't repeated.

What's actually still open, combining `docs/BENCHMARKS.md` §8's own
pre-existing list with this session's EvalMem findings:

## Sequenced steps

1. ✅ **A `single-session-preference`-aware answer prompt (LongMemEval).**
   Done, `d7fe2af`. Added `preferenceAnswerPrompt` + `answerPromptFor(qa)`,
   selected by LongMemEval's own `question_type` (falls back to
   `qaConcisenessPrompt` everywhere else, including LoCoMo). Applied to
   both `answerQuestions` and `runBaselineConversation` so the no-memory
   baseline still isolates "does memory help."

   Verified against real infra, not just reasoning: reused the exact 8
   `single-session-preference` scopes already consolidated by the
   original 48-instance run (found via `cmd/hupi-bench`'s own
   deterministic scope naming, split across the two real Postgres
   instances that run used), re-answered with `-answer-only` (no
   re-consolidation needed), scored with LongMemEval's own unmodified
   `evaluate_qa.py` + real GPT-4o judge — same instances, same scoring
   code as the original 0% result.

   **Result: 0% (0/8) → 37.5% (3/8)**, real and judge-verified. Honest
   mixed picture, not a full fix: the 5 remaining failures are a
   different problem now — at least one (the AI/healthcare publications
   question) is a genuine retrieval miss (the answer explicitly says no
   relevant preference was found), not an answer-style issue anymore —
   pointing at steps 3/4 below (diversity-aware assembly, signal fusion)
   rather than further prompt tuning to close the rest of the gap.

2. ✅ **Resolve the graph-walk question.** Option (a) done — a real,
   cheap re-verification at the *current* codepoint (this branch,
   steps 1/3/4/5/6 all applied), not the stale v3 ablation. Genuinely
   cheap: reused the 10 already-consolidated real scopes from the
   original full-scale LoCoMo run (`user:bench-locomo-hupi-conv-*`,
   confirmed still present in the real Postgres instance) via
   `cmd/hupi-bench -answer-only` — zero re-ingestion cost, just
   re-answering. Filtered to the 3 most multi-hop-heavy conversations
   (`conv-42`, `conv-49`, `conv-26` — 106 real category-1 questions
   combined, the most any 3 conversations could offer), run twice
   (`HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK` on vs. off), scored with
   LoCoMo's own real, unmodified scoring code, category 1 specifically
   (not overall — overall would dilute a category-specific effect into
   noise, the original ablation's own real blind spot).

   **Result: 35.3% (on) vs. 35.4% (off), n=106 — a 0.1pp difference,
   well within noise.** This is now the *third* independent measurement
   agreeing the graph walk isn't contributing on these benchmarks (the
   stale v3 ablation, the EvalMem 0/32 firing-rate finding, and now this
   real per-category re-check at the current codepoint). Given three
   independent methods converge on the same answer, this is now a
   settled finding, not an open question — option (b) (invest in an
   explicit cross-session linking mechanism instead, since that's what
   LoCoMo's own "multi-hop" actually tests) is the real next step if this
   is worth pursuing further, not another re-measurement.

3. ✅ **Diversity-aware context assembly (MMR-style de-duplication).**
   Done for `vectorSearchSummaries`, `1bd6a7a`. Scoped to summaries only
   for this pass, not entities/episodes/keyword-search/graph-walk — see
   the commit for why (establishes the refactor point step 4 builds on,
   with a smaller, independently-verifiable change first). Used lexical
   (Jaccard token) overlap between candidate summaries as the diversity
   signal instead of a second embedding round-trip per pair — real
   inspection of the redundancy problem confirmed near-duplicate
   summaries share most content words even when phrased slightly
   differently, so this is a simpler, dependency-free, no-extra-query
   proxy for what a second embedding comparison would measure anyway.
   `vectorSearchSummaries` now overfetches 3x `maxVectorResults()`
   real threshold-clearing candidates, then `mmrSelect` narrows back
   down using standard MMR (`HUPI_MMR_LAMBDA`, default 0.7, same
   override pattern as `contextCharBudget`/`maxVectorResults`).

   Unit-tested in isolation first (`mmr_test.go`): the core hypothesis
   directly (MMR picks a genuinely distinct lower-relevance candidate
   over a near-duplicate higher-relevance one once the top pick is
   already selected), the `lambda=1` boundary (degenerates to plain
   top-K), and the no-real-surplus no-op case.

   Then verified against real infra, not just unit tests: reused the
   already-consolidated `conv-26` scope, real Postgres, real GPT-4.1,
   keyword search disabled to isolate vector search's own contribution.
   At `maxVectorResults=5` (real surplus: 10 threshold-clearing
   candidates for 5 slots), MMR swapped out a weekly-rollup summary that
   was a real near-duplicate of an already-selected daily summary (both
   about "a necklace from Sweden, growing interest in counseling") for a
   genuinely distinct daily summary (adoption agency interviews — a
   topic none of the other 4 selected summaries covered). At
   `maxVectorResults=12` there was no real surplus for this
   scope/query (only 10 candidates cleared threshold), so `mmrSelect`
   correctly no-op'd — confirms it isn't manufacturing a difference
   where there's nothing to select from, not just that it does something
   when there is.

4. ✅ **Fuse vector + BM25 keyword into one ranked candidate list for
   summaries** (`6e8fa66`), building on step 3's refactor point exactly
   as anticipated. Scoped to summaries only for this pass, not
   entities/episodes/graph-walk — episodes/entities stay two
   independently-run searches for now (episodes are a supplementary
   path, not the primary retrieval surface summaries are; see the commit
   for the full reasoning). `fusedSearchSummaries` replaces
   `vectorSearchSummaries`/`keywordSearchSummaries` running as two
   independent searches (one gets first pick of slots, the other only
   adds leftovers) with Reciprocal Rank Fusion — a summary found by
   *both* mechanisms, even at a modest rank in each, now outranks one
   found strongly by only one — with `mmrSelect` (step 3) applying on top
   of the fused score, so redundancy is penalized regardless of which
   mechanism found a candidate.

   Used `k=1` for RRF's smoothing constant, not the literature's usual
   `k=60` — that value was tuned for TREC-scale rankings of hundreds of
   results; at this candidate pool's much smaller scale (a few dozen at
   most) `k=60` would flatten every candidate to nearly the same fused
   score, defeating the point of fusing rankings at all.

   Added a real, deliberate observability label (`", vector+keyword
   match"`, distinct from vector-only/keyword-only), the same reasoning
   as the graph-walk match marker — otherwise dual-signal agreement is
   indistinguishable from a single signal's own confidence.

   Verified thoroughly: unit tests for the fusion math in isolation;
   updated the one existing test whose assertion the new label
   intentionally changed (`TestRetrieve_FusedSearchLabelsSummaryFoundByBothMechanisms`,
   was `TestRetrieve_KeywordSearchSkipsWhatVectorSearchAlreadyFound`);
   ran the **full internal/store test suite against a real Postgres
   instance** (`HUPI_TEST_DATABASE_URL`) — every pre-existing retrieval
   test still passes (graph walk, key facts, corrected-summary, entity
   substring match, keyword-without-phrase), confirming no regression
   elsewhere in the retrieval pipeline; and a real end-to-end check
   against the already-consolidated `conv-26` scope, where a real
   LGBTQ-community query correctly labeled all 5 selected summaries as
   found by both mechanisms.

5. ✅ **Resolve relative dates into absolute ones at consolidation time,
   not just via the query-time prompt** (`a9f1220`). `textSource` gained
   a `date` field ("YYYY-MM-DD", empty for rollup sources);
   `loadDailyEpisodes`/`loadEpisodesByID` now select the episode's own
   real `ts`; `buildSummaryPrompt` labels each source with its date when
   known, and `summarySystemPrompt` instructs resolving a source's
   relative date references against its own labeled date into an
   absolute date before writing it into a summary/key_fact.

   Verification surfaced a real second half of the problem, not just
   confirmed the first: against a real, deliberately targeted synthetic
   conversation (real Postgres, real GPT-4.1 — a session dated 8 May
   2023 with "I lost my job yesterday"), the consolidation model
   correctly resolved the date in the summary prose and key_fact ("lost
   their banking job on 2023-05-07") — but the *separate* grounding-check
   pass (a second, independent LLM call that re-verifies every fact
   against the raw source text) marked that same key_fact ungrounded,
   since the raw text only literally says "yesterday" and the grounding
   checker's own instruction required a fact be "stated," not inferred —
   a correctly resolved date wasn't in its vocabulary as acceptable.
   Extended the identical date-labeling treatment to `joinSources`/
   `groundingSystemPrompt` (the grounding pass's own prompt builder);
   re-verified on the same real scenario — the same key_fact now
   correctly comes back `grounded: true`. Without this second half, the
   resolved date still would have reached the retrievable summary prose
   (ungrounded key_facts aren't shown, but summary prose always is), so
   the fix wasn't broken, but this makes it complete rather than
   partial.

   New unit tests for `buildSummaryPrompt`'s date-label behavior; full
   `internal/consolidation` test suite (`RunDaily`, `RunRollup`,
   `Correct`, relationship writes, entity embedding backfill) verified
   against a real Postgres instance — all pass, confirming the added
   `ts` column select and prompt changes didn't regress anything else.

6. ✅ **Surface `summary_key_facts` more prominently in the assembled
   context** (`8a257ca`), revisited once steps 3-5 gave a fuller picture,
   per the user's own explicit note — not skipped, not treated as lower
   priority. `appendKeyFacts` previously wrote every grounded key_fact in
   plain insertion order, no query-awareness at all. `mostRelevantFactIndex`
   now picks the fact sharing the most query vocabulary and
   `appendKeyFacts` moves it to the front, marked `"(most relevant)"` —
   the same deliberate-emphasis reasoning as the graph-walk and
   vector+keyword match markers. Returns no reordering when there are
   fewer than 2 facts, no query terms, or every fact ties on shared
   vocabulary (including a 0-0 tie) — a fabricated "most relevant" label
   on an arbitrary pick would be worse than none.

   Unit-tested in isolation (clear-winner, no-signal, tie, and both
   trivial no-op cases) and against real infra (the already-consolidated
   `conv-26` scope, real Postgres, real GPT-4.1): a genuine-tie query
   correctly triggered no promotion; a second query against a summary
   with ~15 key facts did trigger promotion — but honestly, it promoted
   a fact about *Melanie's* pets (which happened to literally contain
   the word "pets") over the actually-correct fact about Caroline's
   guinea pig (which doesn't use that literal word) — a real,
   understood limitation of lexical-overlap scoring, the same category
   of limitation the MMR diversity signal (step 3) already has. The
   model still answered correctly despite this, but this specific test
   doesn't demonstrate a clean win, and this is recorded honestly rather
   than oversold. Full `internal/store` test suite verified against real
   Postgres — no regressions.

   **Whether this net helps `GF`/`GRF` at benchmark scale is still
   unconfirmed** — per the plan's own original framing, that needs a
   real benchmark re-run, not this exploratory, bounded check. Listed
   last among steps 3-6 because it remains the least certain to actually
   help, not because it was skipped or deprioritized.

## Combined full-scale re-verification (after all 6 steps)

Full LoCoMo (all 10 conversations, 1,986 questions) and full LongMemEval
(the same 48-instance stratified sample) re-run from scratch — fresh
ingestion, not reused scopes, since step 5's consolidation-time fix only
shows up in newly-consolidated data — against the current codepoint
(steps 1, 3, 4, 5, 6 all applied; step 2 was investigation-only, no code
change). Real GPT-4.1, `HUPI_CONTEXT_CHAR_BUDGET=20000`, scored with each
benchmark's own unmodified code.

**A real bug was caught mid-run, not by code review**: the first attempt
at this re-verification hit the exact same `"sql: expected 4 destination
arguments in Scan, not 5"` error on both benchmarks simultaneously — step
5's `textSource.date` change updated `scanEpisodeSources`'s two known
callers but missed a third, `embedHighImportanceEpisodes`'s own separate
query. Every existing test fixture hardcodes `importance=0.5`, below the
embedding threshold (0.6), so this path was never exercised by any test
in this package, before or during this session. Both runs were already
producing silently-degraded, missing-memory predictions by the time this
was caught; both were killed, the bug fixed (`74d3ba4`, with a real
regression test verified to actually catch it), and both runs restarted
clean from a fresh wipe. See that commit for the full account.

### LoCoMo result: 51.3% (up from 50.2%), but not a clean sweep

| Category | Original (50.2%) | New (51.3%) | Change |
|---|---|---|---|
| 1 — multi-hop | 39.6% | 32.7% | **-6.9pp** |
| 2 — single-hop | 38.0% | 51.4% | **+13.4pp** |
| 3 — temporal | 23.1% | 22.0% | -1.1pp |
| 4 — open-domain | 52.3% | 50.3% | -2.0pp |
| 5 — adversarial (abstention) | 67.7% | 71.1% | +3.4pp |

Net positive overall, but a real, honest mixed picture: single-hop and
abstention improved substantially (consistent with steps 3/4's better-
ranked, less-redundant retrieval), but **multi-hop regressed 6.9pp** and
temporal reasoning didn't improve despite step 5 being specifically
aimed at it (a small category, 96 questions, more noise-prone — not a
clear win, but not clearly explained away by noise alone either).

### Why multi-hop regressed — investigated, not just noted

Compared old vs. new `hupi_prediction` per multi-hop question (matched
by question text, scored with LoCoMo's own real `eval_question_answering`)
directly: 18 real regressions (score dropped ≥0.5) vs. only 6 real
improvements — a genuine, substantial net negative shift, not noise.
Two distinct, evidenced causes, not one:

1. **Answer verbosity increased (13 of 18 regressions).** Real,
   measurable: average category-1 answer length went from 17.9 → 19.8
   words (median 13 → 16, +23%), confirmed by directly counting words
   across every category-1 answer in both runs. `qaConcisenessPrompt` is
   completely unchanged for LoCoMo (step 1's preference-prompt branch
   never triggers here — LoCoMo never sets `questionType`), so this
   isn't a prompt regression. Likely mechanism: steps 3/4/6 genuinely
   improved retrieval quality, and richer, more relevant context gives
   the model more material to elaborate on even under an unchanged
   conciseness instruction. Multi-hop's own official scoring
   (`eval_question_answering`'s `f1()`, which splits an answer into
   sub-answers and scores each independently) is measurably more
   sensitive to this precision dilution than the flatter `f1_score()`
   the other categories use — which is also consistent with why
   single-hop and open-domain, seeing the same verbosity trend, didn't
   regress the same way.
2. **Genuine retrieval misses (5 of 18) — root-caused precisely, not
   just hypothesized.** Investigated one concretely: "Who gave Maria's
   family money during tough times?" (gold: her aunt) came back "No
   information available" in the new run. Exporting the scope's full
   memory shows the fact is correctly extracted and grounded in **three
   separate summaries** — all near-duplicate restatements of the same
   fact ("Maria was inspired to volunteer by her aunt, who helped her
   family when they were struggling"). None made it into the assembled
   context. Initial hypothesis was MMR's redundancy penalty trading away
   all three near-duplicates — **wrong, or at least imprecise**: added a
   real diagnostic (`HUPI_DEBUG_FUSION`, `64c713d`) that prints every
   candidate's vector rank, keyword rank, and fused score, and re-ran
   this exact query. The best "aunt" summary had fused score 0.1667
   (vector rank 5, **no keyword-match support at all**) — well below the
   ~0.25-0.34 cutoff, based purely on RRF-fusion ranking, before MMR's
   diversity logic ever got a chance to matter for it. MMR *did* make
   one real substitution in this same candidate pool (excluding a weekly
   rollup in favor of a lower-fused-score daily summary, correctly
   judging the rollup redundant with an already-picked summary) — but
   that swap is unrelated to the aunt-summary's exclusion.

   The real mechanism is more precise: **RRF's additive fusion
   structurally favors a candidate found by both signals, even weakly in
   each, over one found strongly by only one signal** — `reciprocalRank(11) + reciprocalRank(0) = 0.577`
   comfortably beats `reciprocalRank(4) + 0 = 0.167`, regardless of how
   solid that single vector match actually was. A quick back-of-envelope
   check (substituting a max-based combination instead of pure sum)
   still didn't flip this specific case — the competing candidates' own
   individual signals were also genuinely strong, not just double-
   counted — suggesting this isn't a clean, one-line-fix bug so much as
   an inherent property of any additive rank-fusion scheme: reasonable
   people can disagree on whether "found twice, weakly" should beat
   "found once, solidly," and no formula gets this right for every case.

**Net read**: steps 3/4/6 are real, working improvements — they clearly
helped single-hop and abstention — but they introduced a genuine,
non-hypothetical cost for multi-hop specifically, via an answer-
verbosity side effect (clearly fixable, see below) and a structural
property of additive RRF fusion that sometimes ranks a solid
single-signal match below several weaker dual-signal ones (a real
tradeoff of the fusion design, not a bug, and not obviously fixable
without its own new tradeoffs). Not
silently accepted as a win; see "Is this fixable?" below.

### Is this fixable?

**The verbosity cause: yes, fixed and verified (`fda0a3f`).** Added one
targeted paragraph to `qaConcisenessPrompt` (both copies, per this
repo's own "small tools duplicate rather than cross-import"
convention): for a list or yes/no answer, give only the items or the
verdict, not supporting context for each one, even when that detail is
available. Deliberately additive — doesn't touch the existing "include
every specific detail the question asks for" instruction, which guards
against a real, separate, already-fixed truncation problem from earlier
in this benchmarking effort's own history.

Verified against real infra, not just reasoning: reused the already-
consolidated `conv-26`/`conv-30`/`conv-41` scopes from the full
re-verification (`-answer-only`, real GPT-4.1, real LoCoMo scoring),
re-answered the same 74 multi-hop questions that showed the regression.

| | Accuracy (these 74 questions) |
|---|---|
| Original baseline (before steps 1/3-6) | 38.5% |
| After steps 1/3-6, before this fix (the regression) | 31.7% |
| **With this fix** | **45.9%** |

Not just a recovery — this beats the original baseline by a real
margin, confirming steps 3/4/6's retrieval improvements were genuinely
valuable all along; the verbosity side effect was masking them, not
canceling them out. Since `qaConcisenessPrompt` is shared across every
category (not multi-hop-specific), this plausibly helps single-hop,
temporal, and open-domain too, not just multi-hop.

**Confirmed at full scale.** Re-ran all 10 conversations / 1,986
questions with the fix (`-answer-only` against the already-consolidated
scopes — the fix only touches answering, not consolidation), scored
with LoCoMo's own real `eval_question_answering`: 0 errors.

| Category | Original (50.2%) | Post steps 1/3-6, pre-fix (51.3%) | Post steps 1/3-6, with fix |
|---|---|---|---|
| 1 — multi-hop | 39.6% | 32.7% (-6.9pp) | **46.4%** (+6.8pp vs original) |
| 2 — single-hop | 38.0% | 51.4% | **61.1%** |
| 3 — temporal | 23.1% | 22.0% | **35.3%** |
| 4 — open-domain | 52.3% | 50.3% | **56.4%** |
| 5 — adversarial (abstention) | 67.7% | 71.1% | 68.2% |
| **Overall** | **50.2%** | **51.3%** | **57.3%** |

The verbosity fix didn't just recover the multi-hop regression — every
category improved over the original baseline, confirming the 74-question
subset result held at full scale. Category 5 (abstention) dipped
slightly vs. the pre-fix run (71.1% → 68.2%) but stayed above the
original 67.7% — a reasonable tradeoff: a little of the "say less"
abstention benefit traded for more complete, correct answers everywhere
else. Net: **+7.1pp overall vs. the original published baseline**, with
no category left worse off than where this pass started.

**The RRF dual-signal-bias cause: investigated, not cleanly fixable.**
A max-based fusion formula (`max(vectorRRF, keywordRRF)` with a bonus
for dual-signal agreement, instead of a plain sum) was worked through
by hand against the real numbers from the `HUPI_DEBUG_FUSION` trace: it
still wouldn't have surfaced the aunt-summary in this specific case,
because the candidates that beat it don't just win *by* being
dual-signal — their individual best signal (e.g. a genuine top-1
keyword match) is independently strong enough to win either way. This
isn't a bug to patch so much as an inherent property of any rank-fusion
scheme: reasonable systems can disagree on whether "found twice,
weakly" should outrank "found once, solidly," and different real
queries will want different answers to that question. Not pursued
further this pass — flagged as a real, open design question for
`fusedSearchSummaries` if it recurs as a measurable cost at full
benchmark scale, not something to speculatively "fix" against one
hand-traced example.

### LongMemEval result: 71.67% task-averaged on a 30/48 partial run (up from 52.1%), full run not completed

The original pre-fix run hit a real OpenAI credit-exhaustion outage
partway through (documented below), which also exposed a real gap in
`cmd/hupi-bench` itself: a single late failure discarded the entire
run's output, since predictions were only ever written once, at the very
end. Fixed properly (`ebafe29`, see "Harness resilience fix" below) with
per-conversation error isolation, incremental output, and a resume mode
that skips already-complete conversations on restart — verified against
real Postgres by killing an in-progress run and confirming the restart
correctly skipped the 6 already-done conversations and cleanly reset the
one that was mid-consolidation.

The resumed run (with the v7 fixes: MMR, RRF fusion, date resolution,
key-fact promotion, conciseness fix) was intentionally stopped after 30
of 48 instances to bound cost, rather than run to completion. Scored with
LongMemEval's own real, unmodified GPT-4o-judge scoring:

| | Task-averaged | Overall | Abstention |
|---|---|---|---|
| Original (48 instances) | 52.1% | — | 75% |
| v7 fixes (30 instances, partial) | **71.67%** | 73.33% | 66.67% |

Directionally consistent with LoCoMo's own improvement (50.2% → 57.3%),
but **not promoted to a new published headline** — see
`docs/BENCHMARKS.md` §7 for why a 30-instance partial run shouldn't
replace a completed 48-instance one. A full-scale LongMemEval
re-verification remains open (`docs/BENCHMARKS.md` §8).

### Harness resilience fix (`ebafe29`)

The credit-exhaustion incident above surfaced a real gap unrelated to any
retrieval fix: `cmd/hupi-bench` aborted its entire process on the first
unhandled per-question error (a 429), discarding ~31 already-consolidated
conversations' worth of real, paid-for work, because predictions were
only ever written once, at the very end. Fixed with three changes to
`cmd/hupi-bench` (`main.go`, `replay.go`): per-question and
per-conversation error isolation (log and continue, rather than abort),
incremental output (rewritten after every conversation, not just once at
the end), and a resume mode — restarting with the same `-out-file` skips
conversations that are already fully answered and cleanly resets
(`resetScope`) anything else, since replay has no dedup guard and would
otherwise double-write episodes into a partially-consolidated scope.
Verified: 3 new unit tests on the resume-completeness logic, full build
and `go vet` clean, and a live test against real Postgres — killing an
in-flight run and confirming the restart skipped the done conversations
and correctly reset the interrupted one.

## Non-goals for this pass

- Not re-running the full LoCoMo/LongMemEval benchmarks from scratch
  speculatively "to see if anything moved" — each real fix should be
  verified on a small/bounded sample first (matching the discipline
  `docs/EVALMEM_INTEGRATION_PLAN.md` and the original benchmark
  effort both already established), with a full re-run only once a
  specific, understood change has bounded-sample evidence behind it.
- Not scaling the LongMemEval sample past 48 instances (`docs/BENCHMARKS.md`
  §8's own "not urgent" item) — orthogonal to the ideas above, and not
  a source of real score improvement on its own, just narrower
  confidence intervals.
- Not pursuing the real-Claude/Anthropic-model pass (`docs/BENCHMARKS.md`
  §8) as part of this plan — a model-diversity question, not a retrieval/
  generation-quality one, and out of scope here.

## Verification

Each step gets a real, bounded-sample test before being considered
"done," following the same discipline as steps 1-7 of the EvalMem plan:
run on a small slice of real data first (a handful of LongMemEval
instances for step 1; the existing full LoCoMo dataset re-run only for
step 2's graph-walk ablation, since that's what the original ablation
itself used; a bounded conversation or two for steps 3-6), record the
real before/after numbers, and only fold a change into a full official
re-run once it has real, understood evidence behind it — not before.
