# Consolidation architecture review: four findings and a targeted plan for each

## Context

A full read-through of `internal/consolidation` (`runner.go`, `cluster.go`,
`perepisode.go`, `grounding.go`, `contradiction.go`, `store.go`,
`supersession.go`, `types.go`, `prompts.go` — the whole package, not a
sample) was requested after this session's multi-hop counting and
temperature-pin investigations kept surfacing the same underlying
subsystem from different angles. The question behind the review: is
there a bigger architectural change worth making — specifically,
splitting consolidation by the *type of data* it processes (prose vs.
question vs. image) — or are the real gaps something narrower.

**Finding: the real, evidenced gaps are about splitting by *output
responsibility* (prose vs. key-facts vs. entity-attributes vs.
relationships), not by *input modality*.** Every concrete failure this
session actually traced back to a specific cause (attribute dilution,
event-to-day misattribution, vague-paraphrase-instead-of-specific-fact)
was about which extraction task got tangled up with which other task in
one call — never about text vs. image vs. structured-Q&A confusing the
model. Image/file content already has its own narrow, clear attribution
convention in the prompt (the `"[Shared image: ...]"` tag), and nothing
investigated this session implicated that convention as a failure
source. The package has already applied the right instinct once (the
per-episode insurance pass in `perepisode.go` is a narrower, single-
purpose extraction split off from the main call, built after a real
measured dilution failure) — these four findings are places it hasn't
been applied yet, or has a related, adjacent gap.

**Scope constraint for this round, set explicitly by the user**: no
full LoCoMo pipeline re-run to verify any of these four. Each item below
has its own cheap, targeted verification — a unit/integration test, or
at most one real conversation's from-scratch re-ingest — not a 10-
conversation benchmark cycle. If a finding can't be verified that way,
it's flagged as such rather than forced into an expensive re-run.

## Finding 1: entity attributes and key_facts still share one undifferentiated instruction

**Problem, grounded in direct reading**: `summarySystemPrompt`
(`prompts.go:13`) asks for `key_facts`, `entities_touched` (attributes),
and `relationships` in the same breath, with no explicit statement of
which kind of fact belongs in which bucket. `mergeAttributes`
(`store.go:786`) is a flat `map[string]string` overlay with no
accumulation semantics — already confirmed, in this session's own
`docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md`, to produce an entity
attribute (`fourth_video_game_tournament_win_date`) with **zero**
corresponding `key_facts` row anywhere in storage, for a real traced
conversation. `summary_key_facts` already has the right extraction
instruction for repeating milestones (`prompts.go:34`) and is a proven,
working accumulating structure (append-only rows) — the gap is that
nothing tells the model attributes are the *wrong* place for a
repeating/countable event in the first place, so it sometimes invents a
new attribute key for one anyway, alongside (not instead of) the correct
key_fact.

**Proposed fix**: one new paragraph in `summarySystemPrompt`, and the
equivalent in `perEpisodeFactPrompt` where applicable (that prompt
doesn't produce attributes, so only the "is this worth a key_fact"
framing needs the cross-reference): explicitly state that a repeating or
countable type of event (a tournament win, a trip, a purchase) must
always be recorded as its own `key_fact` every time it recurs, and must
**never** get a new attribute key invented to track or count
occurrences of it — attributes are for stable, singular facts about the
entity (a name, a preference, a role), not an accumulating history. This
is a responsibility-boundary clarification, not a schema change,
matching the "explicitly not recommended" conclusion already reached in
`docs/MULTIHOP_COUNT_AGGREGATION_PLAN.md` against a structured-attribute
schema change.

**Risk**: low — purely additive prompt wording, same shape as every
other paragraph already in this prompt. The one real risk precedent in
this exact file (the open-domain inference-guard regression,
`docs/BENCHMARKS.md` §15) was caused by a sentence that *expanded* what
the model is willing to assert; this change narrows what it's willing to
assert into one bucket versus another, the opposite direction of risk.

**Targeted verification (no full pipeline)**:
- Unit test in `prompts_test.go`, matching the existing
  `TestSummarySystemPromptAndPerEpisodeFactPromptCoverMultiMilestoneTimelines`-style
  pattern: assert both prompts contain the new boundary language.
- One real-infra check: fresh from-scratch re-ingest of `conv-42`
  (already deeply characterized this session — the Nate-tournament
  case) in an isolated test database, then a direct decrypted read of
  `person:nate`'s entity attributes (the same `zzdebug`-style one-off
  query pattern used earlier this session) to confirm no new
  per-occurrence attribute key appears, while the tournament wins still
  land as `key_facts`. One conversation, one re-ingest, no benchmark
  scoring involved.

## Finding 2: grounding checks a fact against the whole day, not its own cited source

**Problem, grounded in direct reading**: `groundingCheck`
(`grounding.go:65`) is always given `in.groundingSourceText` — the
*entire* day's joined source text (`storeSummary`, `store.go:59`) — for
every fact in the batch, regardless of which specific episode(s) that
fact's own `source_episode_ids` cites. `storeSummary` separately
validates that a cited episode id is real and genuinely one of this
summary's sources (`store.go:160`), but never checks that the *cited*
episode specifically contains the supporting text — only that the fact
is supported by *something* in the day. A fact could cite the wrong
episode within a correct day and still pass grounding untouched.

**Proposed fix**: keep the existing one-call-per-batch architecture
(don't fragment grounding into one call per fact — that would multiply
real cost for no clear benefit), but tighten the prompt: alongside each
numbered fact, state which source id(s) it claims to cite, and ask the
model to verify the fact is specifically supported by *that* cited
source, not merely present somewhere in the day's combined text.

**Risk — a real tradeoff, not just an upside**: this could increase
false "ungrounded" rejections for a fact that legitimately synthesizes
information across two episodes in the same day (the existing
multi-milestone extraction pattern, `prompts.go:34`, explicitly
encourages exactly this kind of cross-source synthesis for some facts).
Needs real measurement before being trusted, not assumed safe.

**Targeted verification (no full pipeline)**: a hand-crafted
integration test in `grounding_test.go`, mirroring the existing
`TestLiveGroundingCheckReproducesBatchMismatchWithRealData`'s real-API,
fixture-based style (not a replayed conversation): two short synthetic
episodes in one day, a fact that's true of episode B but falsely cites
episode A, confirm the tightened prompt now marks it ungrounded where
today's version would mark it grounded. A second fixture with a fact
that legitimately spans both episodes' content, both cited, confirms
the tightening doesn't regress the legitimate-synthesis case. No
conversation replay, no benchmark scoring.

## Finding 3: rollups don't get the known-entities supersession context

**Problem, grounded in direct reading**: `findKnownEntities`
(`supersession.go:43`) is called once in `RunDaily` (`runner.go:127`)
and threaded into `generateSummary` so the model can recognize a new
attribute as an update to an existing one under a different key name
(`EntityUpdate.SupersedesKeys`). `RunRollup` (`runner.go:686`) calls
`generateSummary` directly with `nil` for `knownEntities` — the code's
own comment says so explicitly ("No knownEntities either — Phase C
sub-problem 1 is scoped to raw daily episode text for now"). A rollup
can therefore reintroduce the exact same key-fragmentation problem
(`preapproval_amount` vs. `pre_approved_amount`) that daily consolidation
now avoids, since `generateSummary` itself doesn't care whether its
sources are raw episodes or lower-level summaries — `findKnownEntities`
takes a plain combined-text string either way.

**Proposed fix**: call `findKnownEntities` in `RunRollup` too, against
the joined text of its own source summaries (`joinSources(sources)`,
already computed), and pass the result into `generateSummary` the same
way `RunDaily` does. This is a small, low-risk change — reusing an
existing, already-tested function against a different text source it
was never structurally prevented from handling.

**Risk**: low. The function itself is generic over its input text; the
only new cost is one more read-only entity scan per rollup, same shape
as the one `RunDaily` already pays.

**Targeted verification (no full pipeline)**: one synthetic conversation
via `cmd/hupi-ingest-turns` (the same tool and pattern the fact-level
redundancy-removal work used earlier this session, specifically chosen
there to get "a clean, unconfounded signal" instead of LoCoMo's own
ambiguous text) — two daily summaries establishing an attribute under
one key, a third day's text restating it under a renamed key, rolled up
into one weekly period. Confirm the rollup's own `generateSummary` call
now receives the renamed-key context and reports the supersession,
rather than both keys surviving side by side into the rollup. One
synthetic conversation, one rollup, no LoCoMo involved at all.

## Finding 4: clustering's cluster-count cap has a known, documented ceiling — not proposing a fix, proposing visibility

**Problem, grounded in direct reading**: `maxClustersPerDay`
(`cluster.go:58`, default 6, overridable via
`HUPI_MAX_CLUSTERS_PER_DAY`) already has a real, measured failure case
on record (`docs/CONSOLIDATION_COMPLETENESS_PLAN.md` Phase D item 3 — a
real day with more genuinely distinct topics than the cap allowed,
confirmed by the surviving key_facts clearly spanning more unrelated
subjects than clusters available). This is already known and already
configurable — not a new finding — but there's currently no live
visibility into how often real production days actually hit this cap,
so raising the default (or leaving it) is still a guess rather than a
measured decision.

**Proposed fix**: not a prompt or algorithm change. Add one counter
metric, matching `internal/metrics`' existing conventions exactly (e.g.
`hupi_consolidation_cluster_merges_total`, incremented in
`clusterSources`, `cluster.go:274`, each time the merge-closest-pair
loop actually fires) — direct, cheap evidence of how often a real
deployment's busy days are hitting the cap, before spending more effort
calibrating it further.

**Risk**: negligible — a counter increment, no behavior change.

**Targeted verification (no full pipeline)**: a unit test in
`cluster_test.go` (which already exercises `clusterSources` directly
with synthetic vectors, no LLM or database needed) asserting the new
metric increments exactly when the merge loop fires, using the same
`testutil`-style metric assertion pattern this session's own reranker/
redundancy metrics work established (`internal/store/rerank_test.go`'s
`histogramSampleCount` helper and siblings). No conversation, no
pipeline, no benchmark.

## Sequencing and overall verification posture

Implement in the order listed — 1 and 3 are the most clearly net-
positive and lowest-risk (pure narrowing / pure context-widening with an
already-proven function); 2 carries a genuine tradeoff and should be
verified on both its target case and its "don't regress legitimate
synthesis" case before being considered safe; 4 is pure instrumentation,
no behavior risk, could be done first or last with no sequencing
constraint.

Across all four: `gofmt -l`, `go build ./...`, `go vet ./...` clean, and
the relevant existing test suite (`go test ./internal/consolidation/...`
against real Postgres) stays green — the same baseline bar as every
other change this session, just without the expensive full-benchmark
step this round explicitly doesn't call for. Record outcomes as a new
dated entry in `docs/BENCHMARKS.md` once implemented, same convention as
every other investigation this session, noting explicitly that
verification was targeted/single-instance, not a full re-run, so a
reader doesn't mistake a narrow confirmation for a benchmark-scale one.
