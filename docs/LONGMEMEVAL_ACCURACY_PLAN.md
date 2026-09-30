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

## Category 2: `temporal-reasoning` — three distinct causes, not one

**Real finding**, from inspecting all 5 failures together:

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

**Design**:
- For (1): same query-shape detection as Category 1's fix, but for
  ordering/counting/sequence questions (keywords: "order", "sequence",
  "first...then", "how many times/events", "in a row") — when detected,
  widen retrieval the same way (lower threshold and/or reduced MMR
  diversity penalty specifically for this query shape, since exhaustive
  recall of same-topic items is exactly what's wanted here, not a
  diverse sample).
- For (2): a targeted addition to `qaConcisenessPrompt`, mirroring its
  existing "double-check WHO the information is about" instruction with
  a new "double-check WHAT was actually asked" one: if the question
  requires comparing, counting, or ordering multiple distinct things and
  evidence was only found for some of them, say so explicitly rather
  than answering as if the comparison were fully resolved.
- For (3): a smaller prompt addition asking the model to show its date
  arithmetic explicitly (state both dates, then compute the
  difference) rather than estimating — a cheap intervention, lower
  confidence than (1)/(2) since this is partly a raw-model-arithmetic
  reliability limit no prompt fully closes.

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

## Sequenced steps

1. Category 1 fix: query-shape detection (recommendation-seeking) +
   widened entity retrieval pass.
2. Category 2 fix (1): query-shape detection (ordering/counting) +
   widened/less-diversity-penalized retrieval for that shape.
3. Category 2 fix (2): compound-question abstention prompt addition.
4. Category 2 fix (3): date-arithmetic prompt addition.
5. Category 3 Phase 1: recency-preference prompt addition (reuses
   existing date labels).
6. Category 3 Phase 2: consolidation-time contradiction detection —
   flagged as needing its own design pass before implementation, not
   bundled into this same sequence.

Steps 1-5 are all real, bounded, independently testable changes to
existing mechanisms (query-shape detection already has a precedent in
`stage1KeywordSignal`; prompt additions follow the exact pattern
`qaConcisenessPrompt`'s own WHO-check already established). Step 6 is
explicitly sequenced last and separately, matching this session's own
established discipline of not bundling a deep, higher-risk redesign into
the same pass as cheaper, well-understood fixes.

## Non-goals for this pass

- Step 6 (consolidation-time contradiction detection) is not being
  designed in detail yet — flagged for its own follow-up plan once
  steps 1-5 are verified, not scoped further here.
- Not attempting a general-purpose "resolve any factual contradiction"
  system — scoped specifically to the same-entity/same-topic,
  different-day case actually observed.
- Not changing `maxVectorResults()`'s global default — widening is
  scoped to detected query shapes (preference-seeking,
  ordering/counting), not a blanket increase for every query.

## Verification

- Each step verified the same way this session's other retrieval work
  was: a real unit test for the new query-shape detection function
  (mirroring `mmr_test.go`/`keyfacts_test.go`'s pattern), then a real,
  bounded re-check against the actual failing LongMemEval questions
  already identified in this plan — not the full 48-instance benchmark
  again per step, matching the established "cheap re-verification before
  a full run" discipline.
- A full LongMemEval re-run only once all of steps 1-5 are individually
  verified, to get a real, final combined number.
