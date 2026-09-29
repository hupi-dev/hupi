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

2. **Resolve the graph-walk question — fix or deliberately stop
   relying on it.** Two independent measurements now agree it isn't
   contributing on these benchmarks: the original ablation at the v3
   codepoint (`HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK=false`) showed no
   measurable difference (34.2% vs. 34.8%, within noise,
   `docs/BENCHMARKS.md` §8), and this session's own EvalMem work
   (`docs/EVALMEM_INTEGRATION_PLAN.md` §7) found the walk firing 0/32
   times on real LoCoMo multi-hop questions despite 84 real relationships
   existing for that scope. Working hypothesis from that investigation:
   LoCoMo's own "multi-hop" tests cross-*session* narrative linking, not
   multi-*edge* graph traversal — this feature may be solving a
   different problem than these two benchmarks pose. Two honest options,
   not a foregone conclusion: (a) re-run the ablation at the current
   (v6-equivalent, `main`) codepoint to get one more real, current data
   point before deciding, since the last one is stale relative to
   today's code, or (b) accept the walk isn't the right lever for these
   benchmarks and invest in an explicit cross-session linking mechanism
   instead. Do (a) first — cheap, re-uses the existing full-scale
   dataset, no design work — then decide (b) only if it still shows
   nothing.

3. **Diversity-aware context assembly (MMR-style de-duplication).**
   New finding, not previously flagged: raising `HUPI_MAX_VECTOR_RESULTS`
   from 5 to 12 (`docs/EVALMEM_INTEGRATION_PLAN.md` §7) was neutral, not
   a win, because LoCoMo's two-speaker conversations mean most extra
   vector-search candidates are near-duplicate restatements of
   already-well-covered facts, not new ones — raising K just means more
   competition for the same fixed char budget without more *distinct*
   coverage. A relevance-*and*-diversity re-ranker (Maximal Marginal
   Relevance is the standard, well-understood technique — score by
   `λ · similarity(candidate, query) - (1-λ) · max(similarity(candidate, already_selected))`,
   greedily) over `internal/store/retrieve.go`'s already-collected
   candidate pool, before truncating to the char budget, should let a
   fixed budget cover more distinct facts. Real, medium-sized change:
   touches `vectorSearchSummaries`/`vectorSearchEntities`/
   `vectorSearchEpisodes`'s result assembly, needs a real embedding
   available for the similarity-to-already-selected computation (already
   have summary/entity/episode embeddings — no new embedding calls
   needed, just a similarity computation over vectors already fetched).

4. **Fuse vector + BM25 keyword + graph-walk into one ranked candidate
   list, instead of three independently-run searches concatenated.**
   New finding: each mechanism currently contributes its own candidates
   somewhat independently (see `retrieve.go`'s `vectorSearchSummaries`
   vs. `keywordSearchSummaries` vs. `graphWalkRelationships`, each
   writing directly to the same `strings.Builder`). A single fused rank
   (e.g. reciprocal rank fusion across the three signals, or a weighted
   linear combination) before truncating to the char budget should
   produce a higher-precision top-N within the same budget — directly
   targeting the retrieval-defect dominance (`RF`/`NOI`) seen in every
   EvalMem run this session. Bigger, more architectural change than #3 —
   sequenced after it since #3's de-duplication logic and #4's fusion
   logic will likely share the same "candidate pool before truncation"
   refactor point in `retrieve.go`; doing #3 first establishes that
   refactor with a smaller, easier-to-verify change.

5. **Resolve relative dates into absolute ones at consolidation time,
   not just via the query-time prompt.** New finding: the existing
   `qaConcisenessPrompt` fix (`docs/BENCHMARKS.md` §3) patches the
   *query* side — it tells the model to always answer with an absolute
   date. But if a summary itself still stores "yesterday" or "last week"
   (LoCoMo's own raw dialogue phrasing) instead of resolving it against
   that session's real recorded date at consolidation time, the
   ambiguity is baked into the stored memory itself, not just into
   answer phrasing — and no query-time instruction can fully recover
   information that was never resolved to begin with. LoCoMo's
   temporal-reasoning category is one of its weakest (23.1%,
   `docs/BENCHMARKS.md` §1) — a plausible, targeted fix for specifically
   that category. Scoped to `internal/consolidation`'s summary-writing
   prompt/logic: instruct the consolidation model to resolve relative
   time references against the episode's own known timestamp before
   writing them into a summary.

6. 🔖 **Flagged by the user for deeper investigation before scoping**
   (2026-09-28) — do not skip this one; revisit once steps 3-5 are done
   so there's a fuller, better-understood picture to design against, not
   because it's lower-priority. **Surface `summary_key_facts` more
   prominently in the assembled context, not just as trailing bullet
   points under each summary.** Most speculative item here. Generation
   defects (`GF`/`GRF`) stayed
   high across every EvalMem run this session even after the answer-style
   prompt fix and even with the larger budget — suggesting that
   sometimes the right fact genuinely is present in the assembled
   context, but the model still doesn't extract it correctly from prose
   it has to parse itself. Idea: give the single most relevant extracted
   key fact its own leading position/emphasis in the context, rather
   than presenting it as one bullet among several under a summary
   paragraph. Not scoped further yet — needs a smaller, targeted
   experiment (e.g. on the same bounded EvalMem sample from step 7) to
   check whether this actually reduces `GF`/`GRF` before considering it
   for a real benchmark re-run; listed last because it's the least
   understood of the six.

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
