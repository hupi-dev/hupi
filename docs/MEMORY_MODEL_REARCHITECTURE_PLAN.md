# Plan: unifying the memory model (five features, one architecture)

## Context

Triggered by competitive research into Supermemory's open-source schema
(`packages/validation/schemas.ts` in `supermemoryai/supermemory`), not
by a benchmark gap — explicitly scoped for **product functionality**,
not LoCoMo/LongMemEval score. Five candidate features came out of that
research: `isInference`, a fact-to-fact relation graph
(`updates`/`extends`/`derives`), `forgetAfter`/`forgetReason`
expiration, `sourceCount` reinforcement tracking, and a unified
`isStatic` memory model replacing the current entities/key_facts split.

Each was assessed independently first (see conversation history this
plan was born from). This document exists because they aren't
independent in practice: several share a foundation, one is a
prerequisite the others keep almost-needing, and at least two touch the
citation system (`docs/ANSWER_CITATIONS_PLAN.md`, both phases already
shipped) in ways that change its own scope. Sequencing them separately,
in isolation, would mean paying the integration cost of the unified
model piecemeal, four separate times, instead of once.

## Guiding principles

- **Additive before destructive.** Every phase below is a new column,
  new table, or new optional field — nothing in Phase 1-4 deletes or
  restructures existing data until Phase 0 itself, which is the one
  genuine migration and is scoped and risk-flagged accordingly.
- **Citations must stay trustworthy.** A citation is HUPI's specific
  promise that a user can verify what fed an answer. Any new fact
  category (inferred, superseded, expired) that can't honestly make
  that promise needs its own, explicit handling in the citation path —
  not silent inclusion under the existing snippet logic.
- **Real-infra verification, not just unit tests, for anything
  touching an LLM prompt.** This project has a documented history (this
  same consolidation subsystem, multiple sessions) of a prompt change
  looking correct, passing unit tests, and still not doing what it
  claims until verified against a real model and real re-ingested data.
  Phases 4 and 5 below carry real regression risk for exactly this
  reason and are scoped to be verified the same way.
- **This is explicitly not a benchmark-chasing plan.** Where a phase's
  expected effect on LoCoMo/LongMemEval is near-zero (Phase 0, Phase 2),
  that's stated plainly rather than inflated — the justification is
  product functionality, data minimization, or long-term maintainability
  instead.

## Current state (what this replaces)

Two separate, differently-shaped storage mechanisms for "a fact about
something":

- `entities.attributes` (`schema/0001_init.sql`) — one encrypted JSON
  blob per entity, updated in place via `mergeAttributes`'s flat
  per-key overlay (`internal/consolidation/store.go:786`). No
  versioning, no history, no per-attribute supersession beyond the
  `SupersedesKeys` escape hatch.
- `summary_key_facts` — append-only rows, one per fact, each
  independently `grounded` and citing `source_episode_ids`
  (`internal/consolidation/store.go`'s insert at line ~187). This is
  the structure that actually works for repeating/countable facts
  (confirmed this session, the multi-hop counting work).

The boundary between the two is a real, confirmed source of bugs — the
session's own Finding 1 (`docs/CONSOLIDATION_ARCHITECTURE_REVIEW_PLAN.md`)
traced a live failure to exactly this seam: an attribute recording a
specific tournament win survived while the key_fact that should have
backed it got silently merged away by an unrelated contradiction-check
bug, leaving an attribute with zero fact-level evidence behind it.

Citations (`internal/gateway/handler.go`'s `Citation`/`Ref`,
`internal/store/retrieve.go`'s `summaryCitationSnippet`) currently cite
at **summary granularity**: `Ref.Kind` is `Summary | Entity | Episode`,
never an individual key_fact. `summaryCitationSnippet` picks exactly
one "most relevant" fact out of a summary's full fact list to fold into
one citation — the rest of that summary's facts, even today, aren't
independently traceable. This matters for every phase below: none of
the new per-fact metadata (inference flag, relation, source count,
expiry) is visible through a citation by default, because citations
don't yet expose individual facts at all.

## Target architecture

One unified table (working name: `memories`, replacing both
`entities.attributes` and `summary_key_facts`'s role) with:

```
memories
  id                  text primary key
  scope_kind/owner    (as today)
  entity_id           text references entities(id)   -- null for a
                                                       -- pure event
                                                       -- fact with no
                                                       -- single owning
                                                       -- entity
  summary_id          text references summaries(id)  -- provenance,
                                                       -- same role
                                                       -- source_episode_ids
                                                       -- plays today
  content             bytea        -- encrypted, replaces both
                                   -- entities.attributes values and
                                   -- summary_key_facts.fact
  is_static           boolean      -- today's "attribute" vs "event
                                   -- fact" distinction, as a flag
                                   -- instead of a different table
  is_inference        boolean default false
  source_count        int default 1
  expires_at           date, nullable
  expire_reason        text, nullable
  grounded            boolean      -- unchanged semantics
  source_episode_ids  text[]       -- unchanged semantics
  created_at/updated_at

memory_relations
  id                  text primary key
  from_memory_id       text references memories(id)
  to_memory_id          text references memories(id)
  relation_type        text check (relation_type in
                                   ('updates','extends','derives'))
  scope_kind/owner
  created_at
```

And an extended citation model — one citation per fact, not per
summary (see "Decided: citations move to fact granularity" below for
the reasoning; this is the concrete shape it resolves to):

```
Ref.Kind gains RefKindMemory — the fact-level citation type, replacing
RefKindEntity entirely (attributes are memories now) and replacing
RefKindSummary's old fact-bundling role specifically. RefKindSummary
itself doesn't go away: it keeps citing a summary's *prose* paragraph,
now a separate citation from any individual fact within it, not a
replacement for one.

Citation gains:
  ParentSummaryID string        `json:"parent_summary_id,omitempty"`
                                 -- memories.summary_id, carried onto
                                 -- the citation so a caller/UI can
                                 -- group fact-level citations under
                                 -- their parent summary without a
                                 -- second lookup
  IsInference     *bool         `json:"is_inference,omitempty"`
  Relations       []RelationRef `json:"relations,omitempty"`
                                 -- populated only when the cited
                                 -- memory has an updates/extends/
                                 -- derives link worth surfacing
```

**Scope note, so this isn't mistaken for a context-budget change**:
this is a change to the *citation reporting layer* —
`RetrievalResult.Citations`, the transparency/audit trail returned
alongside an answer — not to `RetrievalResult.ContextMessage`, the
actual text injected into the model's prompt. `appendKeyFacts` already
writes a summary's full fact list into context separately from however
citations get built from the same `picks`/`facts` loop; fact-level
citations don't require injecting more tokens into the answer call
itself, only reporting more granularly on what was available.

### Decided: citations move to fact granularity (not left open)

Originally posed as an open question; resolved during planning review.
**Citations move to per-fact granularity in Phase 0.** Reasoning:

- Phase 4's `is_inference` marking requires it, not just benefits from
  it. `summaryCitationSnippet` today folds one "most relevant" fact
  into one citation per summary — if an answer relies on two facts
  from the same summary, one literal and one inferred, summary-level
  citations have no way to mark only one of them as an inference. The
  citation would misrepresent an inferred claim as a stated one by
  construction, not by a fixable oversight.
- Phase 3's relation graph has no summary-level equivalent at all.
  "This fact updates that one" is a statement about two specific
  facts — summaries don't update other summaries in the unified model,
  facts do. A citation meant to show a relation chain has nothing to
  point at without fact granularity; this isn't an enhancement for
  Phase 3, it's a prerequisite.
- It improves `attribution.go`'s already-shipped Phase 2 check for
  free, in precision — that judge currently evaluates a whole bundled
  prose+fact unit as one snippet; fact-granular citations give it finer
  resolution with no changes to `attribution.go`'s own logic.

**Cost consequence this does introduce, not free**: `buildAttributionPrompt`
lists every citation's snippet into one judge call — a summary that
produces 1 bundled citation today could produce 5 fact-level citations
after this change, directly inflating that prompt's size on every
`X-Hupi-Explain: deep` request. This needs the same batching discipline
`groundingCheckBatchSize` already applies to consolidation-time
grounding checks for the identical reason (a large fact count overwhelming
one LLM call) — scope a `attributionCheckBatchSize` (or equivalent cap)
into Phase 0 alongside the citation changes themselves, not as a
separate later fix.

**Presentation nuance, not an architecture blocker**: a summary's
*prose* (the narrative paragraph) isn't a "fact" in the unified model
and keeps its own, separate citation role for answers that draw on
general narrative rather than one specific fact. The shape is "add
fact-level citations alongside prose-level ones," not a strict
replacement — and a UI can still group fact-level citations under
their parent summary (via the new `ParentSummaryID` field above) for
display even though the underlying data is fact-grained, addressing
the volume concern without reverting the granularity decision.

This changes Phase 0's scope estimate upward (every citation
construction site needs to emit per-fact, not just repoint its `Ref`,
and the attribution batching cap above is new scope, not implied by
anything written before this review) but removes what would otherwise
become blocking rework inside Phase 3.

## Phased delivery

Six phases (Phase 5 added after Phase 3 shipped `updates` only and
`extends` turned out to need its own dedicated prompt-design effort —
see Phase 5's own doc comment), each its own PR (matching this repo's
one-concern-per-PR convention), in dependency order — **not** the order
the five original features were presented in.

### Phase 0 — Unify the storage model + migrate citations together

**Why first, and why bundled with citations**: items 2-4 below all add
fields to "a fact" — building each on top of two parallel storage
mechanisms means building it twice, or building it once and having it
silently not apply to half of what HUPI stores (exactly Finding 1's
failure shape, generalized). And the migration can't stop at the
schema: `Ref.Kind`'s `Entity`/`Summary` split directly mirrors the
*current* two-table model — migrating the storage without migrating
`Ref`/`Citation` alongside it would leave the citation system
describing an architecture that no longer exists underneath it.

**Scope**:
- New `memories`/`memory_relations` tables (migration script).
- One-time backfill: every `entities.attributes` key becomes a
  `memories` row with `is_static=true`; every `summary_key_facts` row
  becomes a `memories` row with `is_static=false`. Existing `grounded`,
  `source_episode_ids` values carry over unchanged.
- Rewrite `mergeAttributes`/`upsertEntities`
  (`internal/consolidation/store.go`) to operate against the unified
  table using `summary_key_facts`'s existing append+supersede pattern,
  rather than the flat per-key overlay — attributes gain real history
  for the first time.
- Rewrite every direct `entities.attributes` reader: confirmed bounded
  to three files (`internal/consolidation/store.go`,
  `internal/consolidation/supersession.go`, `internal/store/retrieve.go`)
  — not an unbounded blast radius, but a real rewrite in each.
- Citation model: add `RefKindMemory`, update all nine citation
  construction call sites in `retrieve.go` to emit one citation per
  injected fact, not one per summary — see "Decided: citations move to
  fact granularity" above (under "Target architecture"). Prose
  citations stay as a separate, additional citation type, not replaced.
  Add the attribution batching cap that same section calls for.

**Complexity: Very High.** The one phase in this plan that's a genuine
breaking migration, not an additive change. Budget this as its own
multi-week effort, not a quick win — see "Non-goals" for why this is
still worth doing anyway.

**Verification**: schema migration tested against a copy of real
production-shaped data (same `zzdebug`-style decrypted-read discipline
this session used throughout), full consolidation test suite, a fresh
from-scratch LoCoMo re-ingest confirming retrieval/citation output is
unchanged in content (even though the underlying storage changed) for
a fixed set of real traced questions from this session's own history
(the Nate tournament case, the Joanna allergy case — both already have
known-correct expected behavior to regression-test against).

**Risk**: real data-loss risk if the backfill is wrong — this is the
one phase that needs a rollback plan (keep old tables read-only
alongside the new ones for one full release before dropping them, not
a single irreversible cutover).

### Phase 1 — `forgetAfter`/`forgetReason` (native expiration)

**Why second**: cheapest and safest of the remaining four, and proves
the new unified table works in a real, user-visible path (retrieval
filtering) before building anything riskier on top of it.

**Scope**: `expires_at`/`expire_reason` columns (already in the Phase 0
schema — this phase is mostly prompt + filter work, not more schema).
One narrow extraction instruction in `summarySystemPrompt`
(`internal/consolidation/prompts.go`): "if a fact is explicitly
time-bound, set when it naturally goes stale." `loadKeyFacts`
equivalent gains `WHERE expires_at IS NULL OR expires_at > :as_of`,
reusing the `now`/query-time plumbing `Retriever.Retrieve` already
threads through (`internal/gateway/handler.go`'s own doc comment on
why `now` is a parameter, not `time.Now()`).

**Citation impact**: none. An expired fact is filtered before
citation-construction code ever sees it — the same mechanism
`grounded=false` already uses today.

**Complexity: Low-Medium.**

**Verification**: unit tests for the filter; one targeted live-prompt
check (does the model actually set sensible expiry dates for a few
real traced "I have X tomorrow"-shaped cases) rather than a full
re-ingest, matching this session's own "one or two targeted instances"
discipline for low-risk additive changes.

### Phase 2 — `sourceCount` (reinforcement signal)

**Why third**: reuses infrastructure that already exists and was just
hardened this session — the `redundancyDedupEnabled()`-gated
"redundant" classification in `internal/consolidation/contradiction.go`
already distinguishes "this restates an existing fact" from "this is a
new occurrence of a repeating event," which is the one genuinely hard
sub-problem this feature needs. Today that classification *deletes*
the restatement; this phase repoints it to increment `source_count`
and keep one row instead.

**Scope**: `source_count` column (already in Phase 0 schema).
`contradiction.go`'s redundant-handling branch changes from delete to
increment. `rankKeyFacts` (`internal/store/retrieve.go`) gains
`source_count` as a ranking input — likely a secondary RRF signal or
tie-breaker, not a primary one, to avoid letting a frequently-repeated
but low-relevance fact crowd out a rare, highly-relevant one.

**Citation impact**: low, optional upside — `source_count` could be
surfaced in a citation snippet ("mentioned 5 times") as a confidence
indicator; purely additive, not required for the feature to work.

**Complexity: Low-Medium.**

**Verification**: the redundant-vs-new-occurrence classifier is
already verified (PR #120's own real-data trace); this phase mainly
needs a ranking-quality check — direct comparison of `rankKeyFacts`
output before/after on a few real multi-mention cases, not a full
re-ingest.

### Phase 3 — `updates`/`extends`/`derives` relation graph

**Why fourth**: the real differentiator for long-lived usage (coherent
fact history, foundation for a future user-facing memory browser), but
the highest-complexity item after Phase 0, and benefits from Phases 1-2
having already proven the unified model in production first.

**Scope**: `memory_relations` table (already in Phase 0 schema).
`contradiction.go`'s response schema gains a third classification
beyond contradiction/redundant — `extends` (explicit metadata for what
today is already the implicit "not a contradiction, just sits
alongside" outcome) and `derives` (the link Phase 4's inference feature
needs to stay safe against staleness). Retrieval gains logic to
optionally suppress an `updates`-superseded fact from a crowded result
set even though it stays `grounded=true` (today, suppression only
happens via literal replacement/deletion). Citations gain the
`Relations []RelationRef` field and rendering logic to show a fact's

**What actually shipped vs. this original scope**: `updates` was wired
by reusing `contradiction.go`'s *existing* cross-period correction flow
(`Runner.Correct`) exactly as-is — a corrected fact is still written via
a brand-new summary version that supersedes the whole old one, the same
full-supersession mechanism this codebase already had. `recordUpdateRelations`
only adds an explicit `memory_relations` edge alongside that, for
traceability/citation display — it changes nothing about which facts
retrieval can see. This means the "suppress an `updates`-superseded fact
from a crowded result set even though it stays `grounded=true`" scope
item above was never reachable: the old fact's *entire summary* is
already superseded and invisible to retrieval the moment `Correct`
commits (the existing `not exists (select 1 from summaries newer where
newer.supersedes = s.id)` filter), so there's no scenario where an
`updates`-superseded fact is still `grounded=true` *and* still
retrievable, competing for space in a crowded result set. That scenario
only arises if a future, lighter-weight "updates" path is added that
links two facts *without* superseding either one's containing summary
(e.g. a same-period update within one summary's own fact list, which
nothing in this codebase detects or classifies today, cross-period or
otherwise) — deferred until such a path exists, not implemented
speculatively against a case this session never built. "extends" was
drafted and live-tested but not shipped — see Phase 5 below.
update/derivation chain.

**Complexity: High as originally scoped** (a richer 3-way classifier
plus new retrieval-time filtering); **what actually shipped (`updates`
only, reusing the existing correction flow) was Low** — the complexity
budget above was almost entirely `extends`'s, which moved to Phase 5.

**Risk**: a looser classifier has more surface area to misfire than
today's narrow binary contradiction check — PR #120 earlier this
session spent real effort specifically hardening that binary check
against conflating two distinct occurrences of a repeating event. This
risk applies to `extends` (Phase 5), not to `updates`: shipped `updates`
reuses the *same* contradiction classification PR #120 already
hardened, unchanged, so it carries none of this risk itself.

**Verification**: `updates` — a new test confirming the `memory_relations`
row and `Citation.Relations` rendering, on top of the existing
contradiction-correction test's own already-established assertions (no
new LLM behavior to verify). `extends` — see Phase 5's own verification
section; this is where the real-data-trace / live-A/B discipline
PR #120 established actually applies.

### Phase 4 — `isInference` (consolidation-time inference extraction)

**Why last**: the highest upside of the five for the open-domain/
inference weak spot, but depends on Phase 3's `derives` relation to stay
safe (an inference needs to know what it was inferred *from*, so it can
be revisited when those source facts change — without that link, an
inferred fact silently goes stale the moment its basis changes), and
carries the most direct regression risk of any item here: this session
already ran this exact experiment once, at the QA-prompt level, and hit
a real (later noise-floor-explained) regression before it shipped.

**Scope**: `is_inference` column (already in Phase 0 schema). New,
narrowly-scoped paragraph in `summarySystemPrompt` — anchored to real
traced cases the way every other paragraph in that prompt already is,
not an open-ended "infer more" license. Every new inferred fact should
be written with a `derives` relation (Phase 3) back to its source
facts. Citation gains `IsInference *bool`; the citation snippet
rendering must mark an inferred fact distinctly from a literally-stated
one — this is not optional polish, it's the difference between a
citation that's still trustworthy and one that silently overstates its
own certainty. `attribution.go`'s Phase 2 "did the answer rely on this"
judge prompt needs an explicit instruction for how to treat an inferred
snippet, since today's prompt assumes every snippet is a stated fact.

**Complexity: Medium-High**, concentrated in prompt engineering and
citation-rendering, not schema (the schema piece is the cheapest part
of this phase).

**Risk**: real, demonstrated regression risk (see above) — mitigate by
building incrementally from the one already-verified real case (the
Joanna/asthma symptom-to-condition inference, F1 0→1 this session),
not a broad new instruction, and by measuring against the noise floor
(two identical re-ingests, same lesson this session's own temperature-
pin work established) before attributing any category-level movement to
the change.

**Verification**: live prompt A/B on the specific real traced case
first; full re-ingest only after that passes; noise-floor-aware
category-level comparison, never a single run's number taken at face
value (this session's own established, hard-won discipline).

**Update — live-tested, not shipped, same root cause as Phase 5's
`extends`**: the "live prompt A/B on the specific real traced case"
verification step above was run before writing any production code, the
same discipline Phase 5 used. Two attempts against real GPT-4.1,
temperature 0, the exact Joanna/asthma case:

- First attempt: a new paragraph appended to `summarySystemPrompt`
  describing the exception (KNOWN ENTITIES attributes implying an
  unstated condition) with the allergy/asthma example worked through.
  Result: not fired — the model extracted only the literal hiking fact,
  no inference.
- Second attempt: instead of appending a separate paragraph, the
  exception was inserted as a carve-out directly adjacent to the
  prompt's own foundational rule it needed to override ("Do not include
  anything you are inferring, generalizing, or guessing beyond what the
  source text states" — the very first key_facts instruction in the
  whole prompt). Result: still not fired, identical output.
- Isolated capability check (same technique that settled Phase 5's
  question): the identical allergy pattern, asked directly with no
  surrounding task framing at all ("what underlying condition does this
  imply? One word") — gpt-4.1 answered "Asthma" immediately, temperature
  0. This rules out a model-capability limitation; the suppression is
  specific to the extraction prompt's own framing.

**Conclusion**: this is the same failure mode as Phase 5's `extends`,
not a coincidence — `summarySystemPrompt`'s entire design is "extract
only what's literally stated," repeated and reinforced throughout
(this is its very first instruction). A single exception, however
precisely placed or worded, is fighting the prompt's own dominant,
repeated framing, and loses. A reliable consolidation-time inference
extractor needs its own separate, dedicated call — one whose system
prompt doesn't carry the "never infer" framing to begin with — not an
addition to `summarySystemPrompt`.

**Shipped as a separate dedicated call — `extractInferences`
(`inference.go`)**: a wholly standalone system prompt (no
`summarySystemPrompt` framing at all — "You are looking at a list of
known entities and their existing recorded attributes... your only job:
check whether any entity's existing attributes... directly and
specifically imply an unstated medical or general condition"), run
after `generateDailySummary` using the same `knownEntities` already
loaded for the main extraction. Live-verified, real GPT-4.1, before
being wired into production code: fired correctly on the Joanna/asthma
case (right entity id, right attribute keys named); correctly reported
nothing for an entity with unrelated attributes (favorite_color,
hometown); correctly reported nothing with no known entities at all;
correctly declined a real near-miss (a marathon runner's high-protein
diet — thematically health-adjacent but not an actual diagnostic
pattern). Behind `HUPI_ENABLE_INFERENCE_EXTRACTION` (off by default,
same posture as `redundancyDedupEnabled`/`queryExpansionEnabled` —
mutates stored data, not just ranking).

An inferred fact is never sent through `groundingCheck` (it checks a
fact against raw conversation text, which an attribute-derived
inference was never going to match) — marked `grounded=true`
unconditionally instead, trusting the extraction prompt's own
live-verified precision bar the same way a literal fact trusts
`groundingCheck`'s. Each inferred fact gets a `derives`
`memory_relations` edge to every attribute row named in
`inferred_from_attribute_keys` (resolved via `entityattrs.CurrentRowIDs`,
the same lookup `internal/store/retrieve.go` already uses) — Phase 4's
own stated safety mechanism, now live. `Citation.IsInference` is a
pointer to the real, known `memories.is_inference` value for every
key-fact citation (never nil — that column always has a definite
answer once this ships). `attribution.go`'s judge prompt marks an
inferred snippet with a `[inferred]` prefix and an explicit instruction
that this doesn't change how reliance is judged.

**Not yet done**: the plan's own final verification step — a full
re-ingest, noise-floor-aware category-level comparison — has not been
run. What shipped is verified at the unit/integration level (real
Postgres, the exact traced case against a real LLM) but not at
benchmark scale. Flagging this plainly rather than claiming a category-
level improvement that hasn't been measured, same discipline as Phase
1's own "not yet done" live-prompt-check note before credentials became
available.

**Real bug found and fixed, later (while auditing every `cmd/` binary
for compatibility with this whole plan)**: `Runner.CurrentContent` —
what `hupi-correct -dump-template` reads, and what any `Correct` call
resubmits — selected `fact, source_episode_ids, expires_at, expire_reason,
source_count` but not `is_inference`. Confirmed live with a real test:
correcting *any* unrelated part of a summary containing an inferred fact
silently flattened that fact to `IsInference: false`, which routed it
through the real grounding check it was always meant to skip — and an
inferred fact, never extracted from source text, reliably comes back
ungrounded there, silently vanishing from retrieval. Fixed by adding
`is_inference` to `CurrentContent`'s select (`runner.go`), with a
regression test (`TestCurrentContent_DumpTemplateThenCorrectPreservesInferredFact`)
reproducing the exact dump → edit-something-unrelated → resubmit
sequence `hupi-correct` performs. `InferredFromEntityID`/
`InferredFromAttributeKeys` still don't round-trip — unlike
`is_inference`, they were never persisted as their own
`summary_key_facts` columns (only used transiently at write time to
record the `derives` edge), so a fact corrected this way keeps its
inference status and grounding exemption, but a fresh correction can't
re-derive which specific attributes it came from, and no new `derives`
edge gets written for it going forward. Smaller, separate, still-open
gap — would need either new persisted columns or reconstructing the
provenance from the fact's existing `memory_relations` rows.

### Phase 5 — `extends` classification (deferred from Phase 3, shipped
via dedicated prompt engineering)

**Why split out, not shipped with Phase 3**: Phase 3 shipped `updates`
only — recording the *already-verified* contradiction/replacement
classification as an explicit `memory_relations` edge, zero new LLM
risk. `extends` (a NEW fact that adds detail/a follow-up development to
an EXISTING fact, without contradicting or restating it) was drafted
and live-tested twice against real GPT-4.1 as an addition to
`contradictionCheckPrompt`, and real-verified NOT to work:

- First attempt: explicit instruction + a narrow "only when genuinely a
  continuation of the same specific thread" guardrail. Result: 0/2 test
  cases fired (a clean hobby-extension case, a clean ongoing-goal-plus-
  specific-instance case) — the model reported nothing for either.
- Second attempt: loosened wording, added two fully-worked positive
  examples matching the test cases almost verbatim (marathon training ->
  a specific training run; learning guitar -> a specific practice
  session). Result: identical — 0/2, including the case the new example
  was written to match exactly.
- Both attempts correctly handled every *other* case in the same
  battery (a genuine value-change classified as `contradiction`;
  unrelated facts reported as nothing; the PR #120 false-positive shape
  — two distinct occurrences of a repeating event — correctly reported
  as nothing, not misclassified as either `contradiction` or `extends`).

**Conclusion**: this isn't a wording nit fixable with one more
iteration. `contradictionCheckPrompt`'s entire framing — extensive
guardrails and worked examples, almost all of them about *rejecting* a
classification — gives the model a strong prior toward "report nothing"
that a few added sentences in the same system prompt don't overcome,
even with directly-matching worked examples. A reliable `extends`
classifier needs its own dedicated, separately-tuned prompt (and likely
its own LLM call, not appended to the skeptical contradiction-check
call) — real scope, not a quick addition.

**Shipped as a from-scratch standalone call — `checkExtends`
(`extends.go`)**: a wholly separate system prompt
(`extendsCheckPrompt`), with no `contradictionCheckPrompt` framing at
all — "Your only job: decide whether the NEW fact is a specific,
concrete follow-up development of one of the EXISTING facts... without
changing or contradicting what the EXISTING fact itself states," two
worked positive examples (marathon training -> a specific training run;
learning guitar -> a specific practice session), and explicit negative
guidance covering the three already-known false-positive shapes
(value-change contradictions, repeating distinct event occurrences,
and merely-same-topic facts). Live-verified, real GPT-4.1, before being
wired into production code, against a 7-case battery: both worked
positive examples fired correctly; a genuine value-change, an unrelated
fact pair, and the PR #120 repeating-tournament shape all correctly
reported nothing; one EXISTING fact with two different NEW facts both
developing it correctly produced two separate pairs. The first
iteration of this prompt had one real false positive, found by a
deliberately adversarial case not in the original battery (a vague "I
love running and find it relaxing" NEW fact against the marathon
EXISTING fact) — the prompt's own "concrete step" language wasn't yet
explicit that a vague restatement of general interest in the same topic
doesn't qualify; adding one more explicit negative worked example fixed
it on the next pass with no regression to the positives, confirmed by
re-running the full battery, not just the failing case.

Runs as an independent second opinion alongside `checkOneRelatedSummary`
in `checkCrossPeriodContradictions`'s existing per-related-summary loop
— same `newFacts`/`oldFacts` already loaded for the contradiction
check, reused rather than re-queried, gated by its own
`HUPI_ENABLE_EXTENDS_DETECTION` toggle (off by default, same posture as
`inferenceExtractionEnabled`: this writes new `memory_relations` rows,
even though unlike `updates`/inference it never mutates a fact's own
text). Unlike `updates`, there is no `Correct` call at all — both facts
already exist exactly as stored, so a genuine pair is just a graph edge,
written by `recordExtendsRelations` the same text-join way
`recordUpdateRelations` already does for `updates` (`from` = the NEW
fact's own `memories` row, `to` = the EXISTING fact it develops).
`loadMemoryRelations`'s existing query needed no changes to surface
this through `Citation.Relations` — it already selects any
`relation_type`, exactly as anticipated below.

**Complexity: Medium**, concentrated entirely in prompt design and live
verification, confirmed in practice — the plumbing (`memory_relations`
table, citation rendering, the text-join relation-recording pattern)
already existed from Phase 3's `updates` work and needed no changes to
carry `extends` once a prompt actually produced one.

**Risk**: same shape Phase 3's own section already flagged — a looser
classifier has more surface area to misfire than a narrow binary one,
and the from-scratch prompt did produce exactly one real false positive
during verification (see above), caught by deliberately testing beyond
the cases the prompt was written to pass, not just by trusting a clean
first run.

**Not yet done**: same caveat as Phase 4's own — this is verified at the
unit/integration level (real Postgres, an expanded real-LLM battery) but
a full re-ingest, noise-floor-aware category-level comparison has not
been run.

## Dependency graph (why this order, restated plainly)

```
Phase 0 (unify + citations)
   │
   ├──> Phase 1 (expiration)        — independent of 2/3/4/5, sequenced
   │                                   early for its low risk
   ├──> Phase 2 (sourceCount)       — independent of 1/3/4/5, sequenced
   │                                   early for its low risk
   │
   └──> Phase 3 (relations: updates only) ──> Phase 4 (inference)
          │                                      (derives relation is
          │                                       Phase 4's safety
          │                                       mechanism)
          └──> Phase 5 (relations: extends)
                 (independent of Phase 4 — a standalone prompt-design
                  effort deferred out of Phase 3, not a dependency of
                  the inference work)
```

Phases 1 and 2 could run in either order, or in parallel, once Phase 0
lands. Phase 4 should not start before Phase 3 ships. Phase 5 can run
any time after Phase 3 (including in parallel with Phase 4) — it only
needs Phase 3's `memory_relations` table/citation plumbing, not
anything Phase 4 adds.

## Non-goals / explicitly deferred

- **Not chasing LoCoMo/LongMemEval score with this plan.** Phase 0, 1,
  and 2 are expected to have near-zero direct benchmark effect; this
  plan's justification is product functionality, data minimization, and
  reducing a confirmed class of bugs (Finding 1), not a score.
- **Not building a user-facing memory browser/graph UI in this plan.**
  Phase 3's relation graph is the *foundation* such a feature would
  need, but building the UI itself is separate, later scope.
- **Not re-litigating whether `isStatic` should be a flag vs. staying
  two tables with a shared interface** — this plan takes the "one table,
  one flag" design as settled based on the earlier discussion; worth
  challenging explicitly if there's disagreement before Phase 0 starts,
  since reversing it after Phase 0 ships is expensive.

## Open questions (need a decision before Phase 0 starts)

1. Phase 0's rollback window — how long do the old `entities`/
   `summary_key_facts` tables stay readable-but-unwritten after cutover
   before being dropped for good?
2. Does `memory_relations` need to support more than one relation
   between the same two facts (e.g., a fact that both `extends` one
   fact and `derives` from another), or is one relation per ordered
   pair sufficient for the real cases seen so far?
3. Citation volume cap: does the UI grouping (facts nested under their
   parent summary) happen client-side from the full fact-level list
   HUPI returns, or does HUPI itself cap/group before returning
   citations — i.e., is grouping a presentation concern or an API
   contract concern?
