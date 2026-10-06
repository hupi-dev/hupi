# Memory behavior scenarios: what actually happens, question type by question type

This is a product-level reference, not a design doc: for each real shape
of question a user can ask, it walks through what happens at each stage
— **consolidation** (nightly, turns raw episodes into summaries/entities),
**retrieval** (per-question, assembles context from those summaries/
entities), and **answer generation** (the model's final response) — and
states plainly whether that scenario currently works, has a known gap, or
has an open regression. Every claim here is backed by a real, cited
finding from the last two days' investigation (`docs/CONSOLIDATION_COMPLETENESS_PLAN.md`,
`docs/LONGMEMEVAL_ACCURACY_PLAN.md`, `docs/BENCHMARK_IMPROVEMENT_PLAN.md`)
or this session's own real-verified testing — nothing here is
hypothetical or aspirational.

**How to read the status column**: ✅ fixed and real-verified · ⚠️ known,
open gap (not yet fixed) · 🔴 confirmed regression (a recent fix made this
worse, not yet resolved).

## The basic lifecycle, in one paragraph

A conversation turn is captured as an **episode** (raw text, encrypted,
scoped). Overnight, `cmd/hupi-consolidate` walks every active scope and,
per calendar day with new episodes, calls `generateSummary` — one LLM
call that reads every episode for that day and writes a daily **summary**
(prose) plus a list of **key_facts** (short, individually grounded
statements) and updates **entities** (standing facts like preferences).
Weekly/monthly/yearly **rollups** later combine several days' summaries
the same way. When a question comes in, `internal/store.Retrieve` runs
vector + keyword + graph-walk search across summaries/episodes/entities,
fuses and ranks the candidates, assembles their text into one context
message under a fixed character budget, and hands that to the answering
model alongside the question.

---

## A. Single-fact recall on a quiet day — the happy path

**Example**: "What's my dentist's phone number?" (stated once, on an
otherwise uneventful day).

- **Consolidation**: one summary, a handful of key_facts, the phone
  number extracted cleanly. Nothing to dilute it.
- **Retrieval**: the summary is an easy top match on both vector and
  keyword signals; its `guaranteedFact` line (written before any
  truncation can happen) carries the one fact that matters.
- **Answer**: correct, every time.
- **Status**: ✅ this is the case the whole architecture is built around,
  and it works. Every failure mode below is about what happens once a day
  stops being quiet.

## B. Busy-day fact omission (dilution at consolidation time)

**Example**: `gpt4_e072b769` — "How many weeks ago did I start using the
cashback app Ibotta?" The real mention ("I've just downloaded Ibotta")
sits inside a day with many other sessions.

- **Consolidation**: `generateSummary` sees every episode for the day
  (confirmed: no `MaxTokens` cap, no `LIMIT` on episode load — the model
  has everything) but isn't told that a busy day's worth of separate,
  unrelated topics should *all* survive. It picks whatever reads as most
  prominent and silently drops the rest. The real Ibotta case: that day's
  summary was entirely about an unrelated word problem — Ibotta never
  appears anywhere in the stored summary.
- **Retrieval**: can't retrieve what was never written down — this is not
  a retrieval bug, confirmed by direct `hupi-export-memory` reads of the
  stored summary.
- **Answer**: "No information available" — a correct abstention given
  what's actually stored, not a model failure.
- **Status**: ⚠️ **partially mitigated, not closed.** Topic-clustered
  batch consolidation (Phase B — cluster a busy day by similarity,
  summarize each cluster separately, merge mechanically) recovers most
  real cases (3 of 4 known examples fully fixed this way). A real ceiling
  remains: `maxClustersPerDay` (default 6) — a day with more genuinely
  distinct topics than the cap allows still merges some into an
  under-cap, diluted cluster. Confirmed: raising the cap to 10 for the
  Ibotta case took effect (10 real clusters) but *still* didn't recover
  the fact — per-episode fact extraction (treat every episode as its own
  grounding unit, independent of day-level clustering) is the real next
  step for this specific residual case, not further cap tuning. See
  `CONSOLIDATION_COMPLETENESS_PLAN.md` Gap 1 / Phase B / Phase D item 3.

## C. Extreme busy-day fact dilution, *after* clustering (within-summary)

**Example**: `gpt4_45189cb4` — "What's the order of the sports events I
watched in January?" A single day had 41 episodes, correctly triggering
clustering — but still produced **112 key facts on one summary**, 5-6x
beyond what the system was ever designed around.

- **Consolidation**: works as intended (clustering fired, the NFL fact
  *is* extracted and grounded this time — unlike scenario B).
- **Retrieval, first bug**: `writeKeyFacts` wrote all 112 facts in raw
  insertion order, no relevance ranking at all. The old "pick one most
  relevant fact" tie-break gave up entirely once enough facts tied at the
  same score (much more likely at 112 candidates) and fell back to
  arbitrary order.
- **Retrieval, second bug**: the *one* line specifically engineered to
  survive truncation (`guaranteedFact`, written before any depth section)
  used that same broken tie-break — on the real busy day, it guaranteed
  an unrelated tire-pressure fact instead of the NFL-playoffs fact the
  question needed, which was left in the lower-priority section a tight
  budget then cut away entirely.
- **Answer-time, third bug**: the question's "order of ... in January"
  phrasing also triggered a separate aggregation-reasoning pass (see
  scenario F), whose own extraction step missed the NFL fact roughly half
  the time and injected an incomplete "chronological order" hint the
  model deferred to over the full context.
- **Status**: ✅ **all three fixed and real-verified this session** —
  `rankFactsByRelevance` (a full, stable ranking instead of a
  winner-take-all pick), `guaranteedFact` switched to use it, and the
  aggregation pass's "order" hint removed entirely (kept only for the
  "consecutive pair" shape, which still needs it — ranking can't tell
  *which two* facts form a specific pair). 5/5 correct end to end at the
  real production-default context budget, confirmed via the actual
  assembled context. See `CONSOLIDATION_COMPLETENESS_PLAN.md`'s newest
  status entry and PR #15.

## D. Cross-summary budget starvation in guarantee-line assembly ✅ fixed

**Example**: re-running the same 8-conversation sample after PR #15
merged — `gpt4_e072b769` (Ibotta), previously correct, now consistently
wrong across every run.

- **What's actually happening**: retrieval picks *multiple* summaries for
  a generic question (9 summaries matched "weeks ago... Ibotta... app"
  via weak keyword overlap). Every picked summary's `guaranteedFact` line
  is written, one per summary, **before any summary's depth section** —
  in relevance order, highest-scored first. There is no fair budget split
  across these guarantee lines: if there are enough of them, the ones
  written first can consume the *entire* context budget before reaching
  a lower-scored (but still correctly matched) summary at all. Confirmed:
  with the budget raised to 20,000 chars, the Ibotta fact comes back
  correct — it was never missing, just unreachable under the default
  2,000-char budget once several summaries' guarantee lines ran ahead of
  it.
- **Why scenario C's fix exposed this**: `guaranteedFact` now picks a
  genuinely relevant fact on a tie instead of an arbitrary one (correct,
  and necessary for scenario C) — but a genuinely relevant fact is
  sometimes longer text than the arbitrary fallback it replaced, which
  shifts how many characters each summary's guarantee line costs. For
  several summaries ranked ahead of Ibotta's in this question's
  candidate list, that shift was enough to push Ibotta's own guarantee
  line past the budget cutoff where it used to just barely fit.
- **The fix, and a real second mistake found while building it**: cap
  each picked summary's guarantee line to a fair, computed-from-N share
  of the budget (`guaranteeBudgetPerSummary`). The first version of this
  capped only the fact text, not the `"related memory (summary <id>,
  dated ..., ... match): "` prefix wrapped around it — and this real
  benchmark's own summary IDs run ~130-160 characters, so 9 prefixes
  alone still exceeded the budget before the fix changed anything.
  Fixed by reserving the prefix's real, measured cost per summary before
  capping the fact text, and guaranteeing a real minimum
  (`guaranteeMinFactChars`) for the fact regardless of prefix length. A
  *third* mistake surfaced right after that: `truncateToBudget`'s own
  `"...[truncated to fit context budget]"` marker (37 chars) was being
  added once per picked summary — fine when it fires once, at the final
  whole-context cut, but repeating it 9 times was itself enough overhead
  to still blow the budget. Fixed with a separate `hardTruncate` (same
  per-summary cap, no repeated marker).
- **Status**: ✅ fixed and real-verified — 4/4 correct across repeated
  runs, confirmed via the actual assembled context that all 9 guarantee
  lines, including Ibotta's, now fit. `internal/store/keyfacts_test.go`
  has unit tests pinning down the few-picks (unaffected) and many-picks
  (floor-respecting) cases.
- **A related, separate issue found while re-verifying — ✅ also now
  fixed, three further layers deep.** `60bf93ed` ("how many days did my
  backpack take to arrive") was *also* wrong in every run since PR #15
  merged, confirmed present before the guarantee-line fix existed — a
  different mechanism, same architectural gap class ("naive
  tail-truncation, not actually budget-aware across every section of
  context"). Three real sub-causes found and fixed in sequence, each
  only visible once the previous one was addressed:
  1. **One summary's own depth section, uncapped, consumed almost the
     whole budget by itself.** This single picked summary legitimately
     had **120 key facts** — a LongMemEval `_abs` haystack artifact
     cramming many sessions' worth of unrelated content onto one
     calendar day — and `depthText`'s facts were deliberately uncapped
     ("20+ of them, each individually cheap," true at 20, false at 120).
     Fixed: `summaryDepthCap` bounds the whole depth block (facts +
     prose together) at the write site — safe to reintroduce now (an
     earlier, similar cap was tried and reverted for a different reason
     back when facts weren't yet relevance-ranked) since `writeKeyFacts`
     already orders facts by relevance, so a tail cut only drops the
     least-useful ones.
  2. **Freeing that budget just let the next-biggest uncapped thing fill
     it instead.** Episode-level "related exchange" entries
     (`vectorSearchEpisodes`/`keywordSearchEpisodes`) write each
     matched episode's full USER/ASSISTANT text with no cap at all; the
     first-ranked one consumed the newly-freed room before any other
     episode got a chance. Fixed: `episodeExchangeCap` bounds each
     episode entry too.
  3. **A plain head-truncate on each episode still wasn't enough** — a
     real "episode" here turned out to be an entire multi-turn session,
     not a single exchange, with the actual needed detail ("I bought it
     from Amazon on 1/15") mentioned partway through a later turn,
     thousands of characters into that one episode's own text — far
     beyond what any reasonable head-truncate could reach. Fixed:
     `centeredExcerpt` keeps a window centered on the *densest cluster*
     of matched query terms instead of always keeping the text's start.
     A first version centered on the *first* matched term and still
     missed the detail — it landed on an earlier, sparser, less relevant
     mention of "backpack"; scoring every candidate position by how many
     other query-term occurrences cluster near it correctly favored the
     denser, actually-relevant passage instead (which also contained the
     distinguishing term "bought").

  **Real re-verification**: 5/5 correct, stable across 4 full re-runs of
  the whole 8-conversation sample, with every other previously-fixed
  case (`gpt4_45189cb4`, `gpt4_e072b769`, and `b46e15ed`, now also
  reliably correct) unaffected.

  Episodes, entities, and graph content *still* have no fully unified,
  cross-section budget fairness — this fixed the two concrete mechanisms
  a real case exposed (one summary's depth, one episode's exchange text),
  not a general rewrite of context assembly. Tracked as a smaller
  residual than before, not closed as a category.

## E. Cross-day aggregation ("two events in a row", "since my last X and Y")

**Example**: `b46e15ed` — "How many months since I participated in two
charity events in a row, on consecutive days?" Needs Feb 14's fact *and*
Feb 15's fact *and* the realization they're consecutive.

- **Consolidation**: each day is consolidated independently — nothing
  about `RunDaily` processing one date at a time can ever notice a
  cross-day pattern on its own. (A weekly rollup *could*, in principle,
  but inherits the same single-pass compression risk, and — unlike daily
  summaries — never gets a second chance once written: `RunRollup`
  early-returns if a summary for that period already exists.)
- **Retrieval**: once both days' facts individually survive scenario B's
  fix, a budget-aware, two-pass context assembly (guarantee every picked
  summary's best fact first, depth second) reliably gets both dated facts
  into context together — confirmed via direct string search on the real
  assembled context.
- **Answer-time**: getting both facts into context isn't sufficient by
  itself — see scenario F.
- **Status**: ✅ retrieval side fixed (Phase D item 1). The remaining gap
  for this exact example lives at answer time, scenario F below.

## F. Raw multi-fact aggregation reasoning at answer time

**Continuing example E**: both charity-event facts are confirmed present
in the real assembled context (`HUPI_DEBUG_FUSION` and direct string
search both confirm this) — and the model *still* answers "no information
available."

- **What's happening**: this isn't retrieval or consolidation anymore —
  it's the model failing to (a) recognize which two of several
  candidate-event mentions are "the pair on consecutive days" and (b)
  compute the requested duration from that pair, in one single-shot
  answer call. Two escalating prompt-only fixes were tried (scan every
  occurrence before answering; explicitly check each pair for one
  calendar day apart) and both measured **zero effect** — reverted rather
  than left in place unproven.
- **The real fix**: a dedicated aggregation-reasoning pass — a separate,
  deterministic-where-possible step that extracts dated facts via one
  focused LLM call, then resolves "closest consecutive pair, gap to now"
  in pure code (`resolveConsecutivePairHint`), handing the answering model
  a precomputed hint instead of asking it to do extraction + pairing +
  arithmetic in one pass.
- **A hint shape that was tried and had to be removed**: the same pass
  also had an "order" hint (list every event chronologically) for
  ordering-shaped questions — removed this session after real
  verification showed it was a **net negative** once scenario C's ranking
  fix existed: its own extraction step missed facts about as often as any
  other single-shot extraction call, and the model deferred to the
  incomplete hint over the now-well-ranked full context (15/15 correct
  with no hint vs. 5/5 wrong with the incomplete hint, identical
  underlying context).
- **Status**: ✅ "consecutive pair" shape fixed and kept (real, irreplaceable
  value — ranking alone can't identify *which two* facts form the pair).
  ⚠️ open, acknowledged model-capability ceiling for aggregation shapes
  this pass doesn't cover — not a retrieval or consolidation problem, and
  not chasing further prompt iterations without a new, different
  mechanism design.

## G. Contradicting facts across time (no supersession)

**Example** (real production data, not a benchmark artifact): a Wells
Fargo mortgage pre-approval amount has two real summaries, dated months
apart, referencing the same entity — neither superseding the other.

- **Two distinct, both-confirmed root causes**:
  1. **Entity attribute merge is last-write-wins per-JSON-key.** If a
     later extraction states the same real fact under a *differently
     named* key than before, the old key is never touched — both sit
     side by side in the entity's attributes forever.
  2. **Summary supersession only checks the exact same (level, period).**
     A later day's summary that directly contradicts an earlier day's
     has no mechanism to say so — they're different periods by
     construction, so the check never considers the earlier one.
- **Status**: ✅ both fixed and real-verified. `EntityUpdate.SupersedesKeys`
  lets the model explicitly declare "this key replaces that one,"
  confirmed to populate correctly on a synthetic re-run of the real Wells
  Fargo scenario. Cross-period contradiction detection
  (`checkCrossPeriodContradictions`) runs as a best-effort step after
  daily consolidation, reuses `Runner.Correct`'s existing supersession
  machinery, and was confirmed (after two follow-up fixes: grounding
  against the triggering period's own source text, and rewriting stale
  prose, not just the facts) to produce a summary whose corrected
  `key_facts` are genuinely `grounded: true` and whose prose has no trace
  of the old, wrong value.

## H. Temporal relevance ("last month," "since my trip")

**Example (synthetic, adversarial test)**: a textually-similar but
temporally-wrong summary outranked the correct one (fused score 1.0 vs.
0.667) purely because ranking had no signal for whether a candidate's
own dated period actually overlapped the timeframe the question implied.

- **What's happening**: ranking was pure embedding/keyword similarity to
  the question's words — nothing boosted a candidate for being from the
  right *time*, only for sharing vocabulary.
- **Status**: ✅ fixed (`internal/store/temporal.go`) — candidates whose
  period plausibly matches an implied timeframe get a ranking boost.
  Verification of this fix also exposed and fixed a second, unrelated
  real bug: `mmrSelect` could silently discard relevance order even when
  nothing actually needed to be dropped for diversity.

## I. Cross-session preference / recommendation requests

**Example**: "Recommend some cultural events this weekend" — the user
stated "I like language-learning activities" once, weeks ago, in
completely different wording, and the model correctly attempts a
personalized recommendation but retrieval surfaces nothing relevant.

- **What's happening**: preference-like facts live in the entity layer as
  standing facts, but entity retrieval ran through the same
  similarity-ranked/thresholded path as everything else — a
  differently-worded recommendation request shares no close vocabulary
  with how the preference was originally phrased, so it never clears the
  similarity bar.
- **Status**: ✅ partially fixed, ⚠️ known remaining gap. Detecting a
  recommendation-seeking question shape and widening entity retrieval
  specifically for it (lower threshold, more results) is shipped and
  real-verified — 1 of 5 real test questions went from blank to fully
  correct. The other 4 now surface *real*, relevant facts about the right
  person (not blank answers anymore) but not always the one specific fact
  the gold answer designates as canonical, when several true preference
  facts about the same person are all in reach — a ranking problem among
  multiple true candidates, not a recall problem, and not yet designed.

## J. Small-fact-set "guarantee lottery" (below the ranking threshold)

**Example**: a real consolidation run split "the user attended a
downtown robotics event" and "actuators and control systems were
featured" into two separate key facts instead of one, on an otherwise
ordinary day. With `guaranteedFact` only below `guaranteedFactMaxCount`
(3) guaranteeing *all* facts together, and above it picking just one, a
2-fact summary used to always go through the "pick one" path's tie-break.

- **What's happening**: at temperature 0, two real consolidation outputs
  of the same underlying conversation produced measurably different
  downstream answers (10/10 correct vs. 9/10 wrong) purely based on which
  of the two facts got guaranteed — the one that actually carried the
  answer, or the content-free "attended an event" fragment.
- **Status**: ✅ fixed — below the threshold, every fact is guaranteed
  together instead of picking one; the "most relevant" ranking only
  kicks in once there are enough facts that guaranteeing all of them
  would be expensive. 10/10 real end-to-end runs correct after the fix,
  up from roughly 50-65% before.

## K. Multi-hop questions (LoCoMo category 1) — a real, structural tradeoff

**Example**: "Who gave Maria's family money during tough times?" (gold:
her aunt) — the fact is correctly extracted and grounded in **three**
separate near-duplicate summaries, and none made it into the assembled
context.

- **What's happening, precisely diagnosed, not guessed**: this is a
  structural property of additive rank fusion (RRF), not a bug in any one
  component — a candidate found weakly by *both* vector and keyword
  signals can outscore one found strongly by only one signal
  (`reciprocalRank(11) + reciprocalRank(0) = 0.577` beats
  `reciprocalRank(4) + 0 = 0.167`), even when the single-signal match was
  genuinely solid. A separate, unrelated verbosity regression (richer
  retrieval giving the model more to elaborate on, diluting precision
  under multi-hop's stricter sub-answer scoring) compounded this in the
  same batch and was fixed; the fusion-structure effect was not.
- **Status**: ⚠️ open, acknowledged tradeoff. Multi-hop regressed 6.9
  points even as single-hop and abstention improved substantially from
  the same retrieval-quality changes — not obviously fixable without its
  own new tradeoffs, and not silently accepted as a win.

## L. Human-driven correction (not automatic supersession)

**Example**: an operator runs `hupi-correct` with a corrected summary
after noticing a consolidation mistake.

- **What's happening**: `Runner.Correct` reloads the real original source
  material (not the possibly-wrong old summary) so the grounding check
  runs against ground truth, then writes a new version with `supersedes`
  set — recorded as a new version, never an edit, audited as a `correct`
  event rather than a `capture`.
- **Status**: ✅ works as designed — this is the same machinery scenario G's
  automatic cross-period contradiction detection reuses, just
  human-triggered instead of automatic.

## M. Correct abstention ("No information available")

**Example**: `f4f1d8a4_abs` — gold answer is itself "You did not mention
this information," and the model correctly says nothing is available.

- **Status**: ✅ working as intended, and worth stating explicitly because
  it's easy to mistake for a failure during review: an abstention is not
  automatically a bug. This exact case was initially (and wrongly)
  flagged as a regression during this investigation — re-checking the
  *original* run's own judge verdict showed the identical answer was
  graded correct both times. The lesson generalized: always check what
  the gold answer and the real judge actually say before treating an
  abstention as a miss.

## N. Fact relations and inference (updates/extends/derives)

**Example**: a later message describes a concrete training run; an
earlier, still-current fact says the same person is training for a
marathon. Separately, a person's recorded allergy attributes (`allergic_to:
most reptiles and animals with fur`, `allergic_to_cockroaches: yes`)
directly imply an unstated condition (asthma) nothing in the conversation
ever said outright.

- **What's happening**: scenario G's automatic contradiction correction
  now also writes an explicit `updates` graph edge
  (`memory_relations`) linking the new fact back to the one it replaced.
  Two further, independent, opt-in passes (both off by default) were
  added alongside it, each its own standalone LLM call rather than a
  bolt-on to an existing prompt: `extends` detection records a graph edge
  when a new fact is a concrete follow-up development of an existing one
  without contradicting it (the training-run example above); inference
  extraction checks whether a known entity's combined attributes directly
  imply an unstated condition and, if so, stores it as a new fact flagged
  `is_inference` with a `derives` edge back to the attributes it came
  from (the asthma example above).
- **Why two separate bolt-on attempts failed first**: both `extends` and
  inference extraction were tried, twice each, as additions to the
  existing contradiction-check and consolidation prompts respectively —
  both attempts failed to fire live against real GPT-4.1, even with
  directly-matching worked examples. Root cause, confirmed by an isolated
  capability check (the model could make the identical judgment
  correctly when asked in isolation, with no surrounding task framing):
  the host prompt's own dominant framing (skeptical "report nothing" for
  contradiction-check, "extract only what's literally stated" for
  consolidation) suppresses any single bolt-on exception. A wholly
  separate, standalone prompt with no competing framing fired reliably on
  the first attempt for inference extraction, and after one prompt
  tightening (a vague topical restatement was briefly a false positive)
  for `extends`.
- **Status**: ✅ real-verified at the unit/integration level — real
  Postgres, real GPT-4.1 batteries (4/4 for inference, 7/7 for `extends`
  after the one fix above) — but **not yet verified at benchmark scale**.
  Both passes are off by default
  (`HUPI_ENABLE_EXTENDS_DETECTION`/`HUPI_ENABLE_INFERENCE_EXTRACTION`); a
  full re-ingest, category-level before/after comparison hasn't been run.
  See [MEMORY_MODEL_REARCHITECTURE_PLAN.md](MEMORY_MODEL_REARCHITECTURE_PLAN.md)
  Phases 3-5 for the complete verification record.

---

## Open items, in priority order

1. ⚠️ **Scenario D's residual — entities and graph content still have no
   per-section budget fairness.** Summary depth and episode exchanges are
   now both capped (`summaryDepthCap`, `episodeExchangeCap` +
   `centeredExcerpt`); a hypothetical case where an entity's attributes
   or graph-walk content alone consumed the budget hasn't been observed
   in a real failure yet, but the same class of fix would apply if one
   ever is.
2. ⚠️ **Scenario B's residual case** — per-episode fact extraction for
   days whose topic diversity exceeds even a raised `maxClustersPerDay`.
3. ⚠️ **Scenario I's remaining gap** — ranking among multiple true
   preference facts about the same person, once recall itself is no
   longer the bottleneck.
4. ⚠️ **Scenario K** — multi-hop vs. single-hop/abstention tradeoff from
   additive RRF fusion; not obviously fixable without its own cost.
5. ⚠️ **Scenario G's Phase 2 generalization** — current cross-period
   contradiction detection is real and shipped; a fully general
   "detect any plausible update to any prior fact" system remains an
   explicit non-goal, scoped only to the same-entity/same-topic case
   actually observed in production. A real live instance of the gap this
   deliberately doesn't cover: `852ce960` (Wells Fargo) has two genuine,
   unresolved conflicting pre-approval amounts in its own test scope,
   and the model picks the wrong one in roughly 3 of 4 real runs — known
   variance on an acknowledged non-goal, not a new bug.
6. ⚠️ **Scenario F's aggregation ceiling** — shapes beyond "consecutive
   pair" still rely on the model's own single-shot multi-step reasoning,
   with no measured prompt fix; a genuinely different mechanism would be
   needed if this needs closing further.
