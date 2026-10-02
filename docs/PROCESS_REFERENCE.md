# Process Reference — Every Mechanism, Every Prompt, When It Runs

This is the exhaustive "what actually happens, mechanism by mechanism"
reference — every real/LLM call HUPI makes, the exact prompt text behind
each one, every tunable constant and the real measurement behind its
value, and precisely when each piece runs. It complements, and
deliberately doesn't repeat, [HOW_IT_WORKS.md](HOW_IT_WORKS.md) (the
narrative *why*), [ARCHITECTURE.md](../ARCHITECTURE.md) (design intent),
[CODE_GUIDE.md](CODE_GUIDE.md) (the structural *where*), and
[API_REFERENCE.md](API_REFERENCE.md) (the HTTP call chain). Read this one
when the question is "what exact logic decides X" or "what does the model
actually get told to do."

Everything here is verified against the real code as it exists today, not
summarized from design docs — every prompt is the literal constant,
every threshold cites the real measurement behind it where one exists.

---

## 1. Two timelines, one shared vocabulary

Everything below falls into one of two timelines:

- **Live request path** (§2) — runs synchronously inside one
  `POST /v1/chat/completions` call, `cmd/hupi`. A slow or failed step
  here delays an answer the user is actively waiting on.
- **Nightly consolidation** (§3) — runs once per scope, once per day,
  via cron (`cmd/hupi-consolidate`), never inside a live request. A slow
  or failed step here delays memory getting distilled, never a live
  answer.

A third, smaller category — **benchmark-only prompts** (§6) — never runs
in a real deployment at all; flagged separately so it's never confused
with live behavior.

---

## 2. Live request path (`internal/gateway/handler.go`)

In order, for every `POST /v1/chat/completions`:

### 2.1 Scope resolution
No LLM call. See [API_REFERENCE.md](API_REFERENCE.md) for the exact
auth/scope logic.

### 2.2 Retrieval (`internal/store/retrieve.go`'s `Store.Retrieve`)

Unless `X-Hupi-Memory: off`. Query text is always
`lastUserMessage(messages)` — the literal last user-role message,
**before** any attachment is merged in (§2.4 runs later specifically so
a document's full text never inflates this query).

**Anchor** (always returned, regardless of gate): the caller's own
`self_model` entity (personal voice, anchored to `actingUser` even
inside a team workspace — [TIER3_PLAN.md](TIER3_PLAN.md) D3) plus a
one-line pointer to the workspace's latest daily summary. No LLM call.

**Stage 1 — cheap, no-LLM-call pre-check.** Decides whether to search
*at all*:
- `stage1EntityMatches` — exact substring match of every known entity's
  name or id-slug against the query text (case-insensitive).
- `stage1KeywordSignal` — a small fixed phrase list: `remember`,
  `recall`, `decided`, `decide`, `prefer`, `we discussed`, `last time`,
  `again`, `what did`, `earlier`, `before`.
- `stage1QuestionSignal` — a trailing `?`, or the message starts with a
  common interrogative/auxiliary word (`what`/`whats`/`when`/`is`/
  `are`/`can`/...). Added after live testing found real recall
  questions with none of the fixed phrases above (`"What is my
  project's codename?"`).

If stage 1 finds an entity match with a resolvable timeframe (see §2.2.5)
that doesn't overlap, it's **excluded outright**, not just
deprioritized — the one path where a stage-1 match renders with zero
date awareness otherwise. If neither an entity match nor a keyword/
question signal fires: **`Gate: skipped`**, anchor only, nothing else
runs.

**Stage 2 — something looked worth searching for.** The query is
embedded exactly **once** (one real network call) and reused by every
mechanism below:

| Mechanism | Similarity threshold | Notes |
|---|---|---|
| Fused summary search | 0.40 (0.25 if ordering-shaped, see below) | RRF-fused with BM25 keyword rank, MMR-selected |
| Episode vector search | 0.55 | Deliberately stricter — raw episode text was never grounding-checked |
| Entity vector search | 0.50 (0.25 if recommendation-shaped) | |
| Keyword (BM25) search | score > 0 | Episodes + entities; summaries handled by the fused search above |
| Graph walk | — | 2 hops, 10 entities max, seeded from every entity found above |

Every one of these thresholds except BM25's was calibrated against real
measured query/document similarity pairs — see §2.2.1-§2.2.4 for the
actual numbers. **Re-measure any of them if the embedding model
changes.**

Two query-shape detectors widen the pool for one call, not forever:
- `looksLikeRecommendationRequest` (`recommend`, `suggest`, `should i`,
  `any tips`, ...) — widens **entity** search (threshold 0.25, cap 10)
  because a recommendation-seeking question often shares no vocabulary
  at all with the preference it needs (`"what should I bake"` vs. `"I
  want to practice my Spanish"`).
- `looksLikeOrderingRequest` — widens **summary** search (threshold
  0.25, cap 15) for a multi-event ordering/counting question. This is
  the *second* attempt at this fix — the first widened only the
  final-selection count and measured zero effect, since every candidate
  had `vectorRank=-1`: nothing was even in the pool to select from. This
  version widens the fetch itself.

Also sets `NeedsAggregationPass` if ordering-shaped, triggering §2.3.

#### 2.2.1 Summary vector threshold (0.40) — real measurement

Original design value was 0.75; an initial single measurement came back
0.50 for an exact semantic match. A broader measurement against a real
daily summary:

```
0.0716  true negative  — "what's the weather like today?"
0.1217  true negative  — "write me a haiku about the ocean"
0.1792  true positive  — a real fact, buried in a long unrelated summary
0.1830  true negative  — "how do I set up a Kubernetes ingress?"
0.2866  near-miss      — "how does memory retrieval work?"
0.3023  true positive  — a dashboard fact, similarly buried
0.3288  near-miss      — generic Postgres tuning question
0.5226  true positive  — the summary's actual central topic, paraphrased
0.5729  true positive  — same, a different central fact
0.6301  true positive  — same, asked directly
```

True positives and true negatives **interleave** below ~0.33 — a buried
real fact can score lower than a wholly unrelated question, because a
long multi-topic summary embeds as the average of everything it
mentions. 0.40 is the one clean gap (between the highest near-miss 0.33
and the lowest strong true positive 0.52); going lower would also admit
the near-misses sitting right next to the buried facts. The actual fix
for a buried fact is giving it its own embedding — see §2.2.8
(per-fact semantic ranking) — not lowering this threshold further.

#### 2.2.2 Entity vector threshold (0.50) — real measurement

```
"what programming language do I prefer?"      -> 0.6811 (true positive)
"what's my favorite programming language?"     -> 0.6666 (true positive)
"do I like Rust?" vs the matching entity       -> 0.5343 (weakest true positive)
"do I like Rust?" vs a related-but-wrong entity -> 0.6240 (also legitimate)
"prefer"/"favorite" vs a related entity        -> 0.41-0.44 (correctly excluded)
"what's the weather today?" vs any of the above -> 0.07-0.10 (unrelated)
```

#### 2.2.3 Episode vector threshold (0.55) — real measurement

Deliberately stricter than summaries' 0.40 — raw episode text has never
passed a grounding check, so a false positive here hands the model
unverified content and calls it memory, worse than a missed broad
paraphrase:

```
"how do I set up a Kubernetes ingress controller?"  -> 0.1325 (TN)
"a reasonable concurrency limit for a task queue?" (generic) -> 0.3911 (near-miss)
"tell me about my job scheduler's core architecture" -> 0.4477 (true positive, broad)
"Postgres or Redis for a job queue in general?" (generic) -> 0.5110 (near-miss)
"what database does Meridian use instead of Redis, and why?" -> 0.6478 (true positive)
"what async runtime does Meridian's scheduler use?"  -> 0.6544 (true positive)
"how many concurrent jobs can Meridian handle per worker node?" -> 0.6851 (true positive)
```

The clean gap sits between the 0.51 near-miss and 0.65 lowest direct
true positive — at the cost of the one broader paraphrase (0.4477) no
longer clearing it, a deliberate trade given raw episodes' lack of
grounding.

#### 2.2.4 BM25 keyword score (`> 0`) — real measurement

Measured against an 18-document corpus mixing several distinct topics:

```
11.10  true positive  — exact rare-term match, direct question
 6.91  true positive  — exact rare-term match, different phrasing
 5.29  true positive  — exact rare-term match, different topic
 5.19  near-miss       — same rare term, wrong specific document
 2.55  near-miss       — a shared moderately-common word, wrong topic
 1.29  near-miss       — "async runtime" vs "tokio" — BM25 can't bridge
                          synonyms, one shared rare name saves it
 0.00  every true negative, zero exceptions
```

The one clean gap: `0` vs. any positive score. RRF fusion (not the
score itself) is what separates a true positive from a same-topic
near-miss above that cutoff.

Tuning: `bm25K1 = 1.5`, `bm25B = 0.75` — standard Okapi defaults, not yet
calibrated against real traffic. Tokenizer: lowercase, split on
non-alphanumeric, drop a small English stopword list, **drop
single-character tokens** (apostrophe-splitting otherwise turns every
possessive into a spurious `"s"` token that scored a true negative
identically to a real near-miss). No stemming — exact-token overlap is
the whole point of running this alongside vector search.

**Per-scope governance.** BM25 has no database index — every stored field
is encrypted at rest, so it decrypts and scores the entire in-scope
corpus on every call, unlike vector search's HNSW-indexed lookups. Beyond
the existing global `HUPI_ENABLE_KEYWORD_SEARCH` switch, each retrieval
call also picks one of three tiers for the scope it's running in, based
on that scope's own real corpus size (`scope_corpus_size`, refreshed once
per day by consolidation for any scope with new episodes that day):
**full** (today's unrestricted scan, below `keywordSearchNarrowThreshold`),
**narrowed** (above that threshold: summary keyword search is narrowed to
summaries whose `entities_touched` overlaps a stage-1-matched entity —
falling back to a full scan when no entity matched, so a rare term with
no entity anchor is never silently dropped), or **disabled** (above
`keywordSearchDisableThreshold`: keyword search skipped entirely for that
scope). `hupi_keyword_search_tier_total` (labeled `full`/`narrowed`/
`disabled`, no scope-owner label) gives a real, deployment-wide
distribution to calibrate both thresholds against.

#### 2.2.5 Timeframe hard filter + temporal relevance boost

`resolveQueryTimeframe` recognizes a small fixed set of relative-time
phrases (`yesterday`, `today`, `last week`, `this week`, `last month`,
`this month`, `last year`, `this year`) and resolves them to a concrete
`[start, end)` range. Two separate, real-verified fixes layer on this:

1. **Hard filter**: a candidate whose period provably doesn't overlap
   the resolved timeframe is **excluded from the pool entirely**, not
   just deprioritized — found necessary after the boost alone still let
   a same-topic, wrong-period distractor win on raw lexical match
   (`"AI conference"` literally in the wrong summary's text) even after
   ranking correctly promoted the right one. Only applied when excluding
   wouldn't leave the pool empty; backs off entirely otherwise (never
   destroys information on an unclear signal).
2. **Temporal relevance boost**: `reciprocalRank(0)` (= `1/(rrfK+1)` =
   0.5 at the real `rrfK=1.0`) added to a candidate's fused score when
   its period genuinely overlaps the resolved timeframe — a full
   best-rank-equivalent contribution, strong but not an unconditional
   override.

`relativeDateLabel` additionally converts a resolved date into
already-computed English (`"3 weeks before now"`) rather than handing
the model a raw date to do arithmetic on — real-verified the model gets
date arithmetic wrong (answered `"9 weeks ago"` against a gold `"3 weeks
ago"`) even with the correct date present in context.

#### 2.2.6 Reciprocal Rank Fusion (RRF) — the actual formula

```go
const rrfK = 1.0
func reciprocalRank(rank int) float64 { return 1.0 / (rrfK + float64(rank+1)) }
```

`rrfK=1.0`, not the RRF literature's usual `k=60` — that value was
tuned for TREC-scale rankings (hundreds of results); at HUPI's scale (a
few dozen candidates at most) `k=60` would flatten a #1 candidate and a
#20 candidate to nearly the same score, defeating the point of fusing
rankings at all. `k=1` preserves meaningful separation at this smaller
scale.

RRF is used in **two distinct places**, same formula, same constant:
- **Summary retrieval** (`fusedSearchSummaries`): a summary's vector
  rank and keyword rank are separately computed, then
  `reciprocalRank(vectorRank) + reciprocalRank(keywordRank)` — a summary
  found by *both* mechanisms outranks one found strongly by only one.
- **Per-fact ranking inside a selected summary** (`rankKeyFacts`, §2.2.8)
  — lexical rank and semantic (embedding) rank fused the same way,
  instead of letting either one unilaterally win.

#### 2.2.7 MMR (Maximal Marginal Relevance) diversity selection

```
score = lambda * relevance - (1 - lambda) * maxSimilarityToAlreadySelected
```

`lambda = 0.7` by default (`HUPI_MMR_LAMBDA` override) — favors
relevance over diversity, while still penalizing a near-duplicate of
something already picked. Diversity signal is **Jaccard token overlap**
(not a second embedding round-trip) — real inspection of actual
near-duplicate summaries found they share most content words even when
phrased slightly differently run to run, so lexical overlap is a cheap,
accurate-enough proxy. Candidate pool is overfetched 3x
(`summaryOverfetchFactor`) beyond the final pick count — MMR has nothing
to select *from* otherwise (measured: raising the final count alone,
with no overfetch, was neutral, since a two-speaker conversation mostly
repeats the same well-covered facts).

#### 2.2.8 Per-fact semantic ranking + the two-pass "guarantee" write (`rankKeyFacts`/`writeKeyFacts`)

Every picked summary's `summary_key_facts` get ranked and written — this
is a **second, independent** semantic-ranking layer, one level below
summary selection:

- **Lexical rank** (`factScores`/`rankFactsByRelevance`): word-overlap
  against query terms, length-normalization **disabled** (`b=0`) —
  unlike summary/episode BM25. Key facts are already short, single-topic
  atomic statements; length differences are incidental phrasing, not a
  dilution signal to normalize away. Real regression without this: three
  facts sharing exactly one query term tied, and standard length
  normalization broke the tie on sentence length alone, the wrong way.
- **Semantic rank**: cosine similarity to the query, only used when
  *every* fact in the summary has a valid per-fact embedding
  (`schema/0021_summary_key_facts_embeddings.sql`) under the active
  model — a mixed state is only ever transient.
- **Fused via RRF**, not semantic-overrides-lexical — a first version
  that let semantic ranking win outright measured a real regression on a
  different real question (an embedding model false positive, `0.293` vs
  `0.218`, discarded a clean, unambiguous lexical-top fact). RRF rescues
  a fact with no lexical signal via a strong semantic rank, without
  letting one noisy embedding comparison override a clean lexical
  signal.
- Falls back to lexical-only whenever semantic ranking is unavailable or
  disabled (`HUPI_ENABLE_SEMANTIC_FACT_RANKING=false`).

**The two-pass write** (`fusedSearchSummaries`'s render step): every
picked summary's single **"guaranteed"** fact (or, below
`guaranteedFactMaxCount=3` facts, **all** of them together — picking just
one at that scale was measured a real lottery: 9/10 wrong answers with
one fact guaranteed, 10/10 correct with both) is written for **every
pick first**, before **any** pick's deeper fact list. Only after every
guarantee is written does pass 2 write each pick's full depth content.
This way the final context-budget truncation can only ever cut into
depth content, never into a guarantee that hasn't been written yet. Each
pick's guarantee line has its own fair budget share
(`guaranteeBudgetPerSummary`, computed from how many were actually
picked) — a flat shared cap let several high-ranked picks' guarantees
alone exhaust the budget before a lower-ranked pick's guarantee was ever
reached.

### 2.3 Aggregation reasoning pass (`internal/gateway/aggregation.go`)

Only runs when retrieval set `NeedsAggregationPass` (an
ordering/counting-shaped question). One real LLM call, deliberately
narrow — **extraction only**, no pairing or arithmetic (two earlier
prompt-only attempts that asked one call to do perception + pairing +
arithmetic together measured zero effect):

> **System prompt** (`aggregationExtractionSystemPrompt`): *"You will be
> given a question that requires comparing, ordering, or counting
> multiple dated events, and a block of retrieved context that may
> contain the relevant facts. Extract every fact in the context that
> states a specific date and is relevant to answering the question...
> Be exhaustive, not selective... Only include a fact if the context
> actually states a specific calendar date for it — do not infer, guess,
> or resolve a relative date yourself."*

The rest — finding a consecutive pair, computing a count — is
deterministic Go code (`resolveAggregationHint`), not a second LLM call.
Degrades silently to "no hint" on any failure (never blocks the turn).

### 2.4 Attachment merge (`internal/gateway/attachments.go`)

Only when the request includes `attachments`. Runs **after** retrieval
(§2.2), **before** the provider call — see §2.2's own note on why.
Validates first (count ≤ 5, size ≤ 8 MiB post-base64-decode, image MIME
allowlist) as a hard 400; then processes each attachment concurrently:

- **Document** (`.txt`/`.docx`/`.pdf`, sniffed from magic bytes) →
  `internal/ingest.Extract` — pure local parsing, no LLM/network call,
  10s timeout + panic recovery.
- **Image** → one real vision-model call:

  > **Instruction** (`imageDescribeInstruction`): *"Describe this image
  > factually and specifically: what it shows, any visible text,
  > notable objects, people, or scenes. Be concise but concrete enough
  > that someone who can't see the image would understand what's in
  > it."*

  Resolved via `Registry.Vision()` (falls back to `Registry.Chat()` if
  `active_vision_provider` is unset), using that profile's `vision_model`
  (falls back to the profile's own chat `model` if unset).

Either way, the result is appended to the **last user message's**
content with a provenance marker — `[Attached file: resume.pdf]...[End
of attached file: resume.pdf]` or `[Shared image: vacation.jpg]...` — so
every downstream step (capture, consolidation) sees it as "just more
characters," no further code changes needed anywhere. A per-attachment
extraction/captioning failure degrades to a placeholder marker plus a
response warning, never fails the whole request.

### 2.5 Provider call
The actual chat completion — external network call to whichever vendor
`active_chat_provider` (or a named profile) resolves to.

### 2.6 Capture (`internal/store/capture.go`)
Synchronous, one Postgres insert, no network call: encrypts and stores
the (attachment-merged) turn as a new episode. A capture failure is
logged, never surfaced to the client (except `/v1/feedback`, where the
opposite tradeoff applies).

### 2.7 Citations (`X-Hupi-Explain: on|deep`)

- **`on`** (free): `hupi_citations` is exactly what retrieval (§2.2)
  already computed — no extra call.
- **`deep`** (one extra real LLM call, `internal/gateway/attribution.go`):

  > **System prompt** (`attributionSystemPrompt`): *"You will be given a
  > generated answer and a numbered list of candidate memory snippets
  > that were available when it was written. For each snippet, in
  > order, decide whether the answer's content actually, specifically
  > relies on it — not just topically related, but relied on to produce
  > a detail in the answer. Respond with exactly one JSON object,
  > nothing else: {"used": [true, false, ...]}"*

  Mirrors grounding's own pattern (numbered items + one JSON verdict
  array), but **not** index-tagged the way grounding is (§3.4) — a bare
  positional array, since the candidate count here is always small
  (whatever retrieval picked for one turn, never a 20-fact batch). A
  malformed or wrong-length response is a hard error (leaves `Used` as
  `nil`, meaning "not checked"), never silently defaults every citation
  to "unused" — a real, previously-fixed bug (`hupi_citations`'
  `nil`-means-"not checked" contract would otherwise be violated).

---

## 3. Nightly consolidation (`cmd/hupi-consolidate`, `internal/consolidation`)

Runs once per active scope (`loadActiveScopes` — every row in `users` +
`teams`), once per day, via cron. Never touches a live request.

### 3.1 Clustering decision (`generateDailySummary`)

- `≤ 8` episodes that day (`clusterEpisodeThreshold`): one single
  consolidation call over everything (§3.2), plus the per-episode
  insurance pass (§3.3) regardless.
- `> 8` episodes: sources are embedded (one real call) and grouped via
  union-find on pairwise cosine similarity, **threshold 0.60**
  (deliberately high — merging two genuinely distinct topics back
  together reproduces the exact dilution problem clustering exists to
  fix; two loosely-related sources staying separate costs nothing).
  Capped at **6 clusters** (`HUPI_MAX_CLUSTERS_PER_DAY` override) — above
  the cap, the two most-similar clusters (by centroid) merge until at or
  under it. Each cluster gets its **own independent** consolidation call
  (§3.2), mechanically concatenated afterward — never a further LLM
  compression pass, which would reintroduce the exact dilution clustering
  exists to fix.
- If embedding fails, or clustering collapses to ≤ 1 cluster: falls back
  to the single-call path.

### 3.2 Consolidation prompt (`summarySystemPrompt`, `internal/consolidation/prompts.go`)

The main LLM call — one per cluster (or one for the whole day, below the
threshold). Full system prompt (abridged for length; see the file for
the complete text):

> *"You are HUPI's consolidation engine. You will be given a set of
> labeled source texts and must respond with exactly one JSON object...
> {summary, key_facts: [{fact, source_episode_ids}], entities_touched:
> [{id, kind, name, attributes, supersedes_keys}], relationships:
> [{subject_kind, subject_name, predicate, object_kind, object_name,
> valid_from, valid_until}]}. Only include a key_fact if it is directly
> and specifically supported by the source texts... Cite the exact
> source ids it came from."*

Real, load-bearing rules inside that prompt, each added for a specific
confirmed failure:
- **Assistant-authored content** gets extracted too, explicitly
  attributed ("In the bedtime story the assistant wrote... the dragon is
  named Ember," never a bare claim) — the same discipline extended to
  **attachments**: a `[Attached file: resume.pdf]`/`[Shared image:
  ...]` block is attributed by filename, never confused with the user's
  own typed words.
- **Multi-milestone dates**: each distinct dated milestone in one
  underlying story (began / signed / completed / decided) becomes its
  own fact, naming the specific milestone — a passage's single most
  memorable date must not stand in for all of them.
- **Relative dates resolved to absolute**, against that source's own
  labeled date, not today's.
- **No false "previously mentioned" qualifiers** — a user stating a fact
  by asking the assistant to recall it ("remember when I got
  pre-approved for $400,000?") is reported as a plain, direct, present
  statement of that fact, not hedged with "previously mentioned" unless
  the source itself states when it was discussed before.
- **`supersedes_keys`**: only named when a `KNOWN ENTITIES` block (see
  below) shows the same entity with an existing attribute key a source
  explicitly updates under a new name — otherwise omitted, so an
  uncertain case leaves the stale key sitting alongside the new one
  rather than risk discarding a still-true fact.

**Context the user message carries, when applicable**:
- **`ALREADY-ESTABLISHED RECORD`** — the day's current draft before a
  re-consolidation (more episodes arrived, or a manual re-run). Prevents
  a real regression: without it, a re-run could see the user's original
  raw number and a later, already human-corrected one as two conflicting
  claims and walk the correction back, since nothing in raw episodes
  reveals a correction was deliberate.
- **`KNOWN ENTITIES`** — existing entities whose name appears in this
  day's sources, with current attributes, specifically so the model can
  recognize an update under a new key name instead of leaving a second,
  differently-named key sitting there forever.

A malformed (non-JSON) response is retried up to `consolidationParseRetries=2` times.

### 3.3 Per-episode insurance pass (`perEpisodeFactPrompt`, `internal/consolidation/perepisode.go`)

Runs on **every day, regardless of episode count** (originally gated to
busy days only; extended after two real failures on *light* days — a
single message can bury a real detail as a trailing aside just as easily
as a busy day's crowding: `"...by the way, remember when I got
pre-approved for $400,000 from Wells Fargo?"` tacked onto a message about
cable providers). One independent call per episode (chunked at 4,000
characters for an unusually long one — measured 5/5 correct vs. 3/5 on
the same content sent whole).

> **System prompt** (abridged): *"You are looking at a single
> conversation exchange for anything worth remembering as a standalone,
> checkable fact... Be exhaustive, not selective... Pay particular
> attention to incidental scene-setting remarks early in the exchange...
> Respond with {"facts": [...]}. If there's nothing worth keeping,
> respond with {"facts": []}."*

Same assistant-content/attachment attribution and multi-milestone-date
rules as §3.2's prompt (kept in sync by design, not by shared code —
each prompt is self-contained). Facts from this pass are **appended**,
not deduplicated — redundancy here is cheaper than a missed fact.

### 3.4 Grounding check (`groundingSystemPrompt`, `internal/consolidation/grounding.go`)

A **second, independent** LLM call — re-verifies every extracted fact
against the raw source text, **never shown the model's own self-cited
source id**, so a plausible-looking false citation can't talk its way
past the check:

> *"You are a fact-checker. You will be given source text and a numbered
> list of claimed facts. For each fact, in order, decide whether the
> source text actually, specifically supports it — not just plausible,
> not just related, but stated. Respond with exactly one JSON object:
> {"grounded": [{"i": 1, "ok": true}, ...]}"*

**Index-tagged**, not a bare positional array (unlike attribution's
check, §2.7) — a real, reproduced failure mode: a live call against 20
real facts came back with 21 verdicts. Batched at **20 facts per call**
(`groundingCheckBatchSize`) specifically to confine any mismatch to one
batch instead of the whole summary. On a count mismatch: retried once,
then **salvaged by index** — every fact whose tagged index resolves
cleanly keeps its real verdict; only a genuinely unindexed fact is forced
to `false`. An ungrounded fact is **stored, not deleted** — just excluded
from retrieval until reviewed.

### 3.5 Storage (`storeSummary`, `internal/consolidation/store.go`)

Encrypts and writes the summary, its key facts (with `grounded`
true/false), and upserts touched entities (decrypt-merge-encrypt,
applying `supersedes_keys`). After commit: embeds the summary prose, each
grounded key fact (batched), and each touched entity — three separate
provider calls, each in its own short transaction (never inside the
write transaction, per the "no network call inside an open transaction"
rule).

### 3.6 Cross-period contradiction detection (`contradictionCheckPrompt`, `internal/consolidation/contradiction.go`)

Best-effort, **after** the day's summary is durably stored — a failure
here never fails the day's consolidation. Finds other *current*
summaries (any level/period) sharing a touched entity, ranked by entity
**specificity** (sum of `1/frequency` across the scope's own summaries,
not raw recency — a near-universal entity like `person:user` would
otherwise crowd out a genuinely rare, specific one like
`organization:wells-fargo` from the bounded candidate list), capped at 5
related summaries checked:

> *"You are checking whether any of a set of NEW facts contradicts any
> of a set of EXISTING facts about the same entities. A contradiction
> means a NEW fact states a different, incompatible value for the exact
> same specific real-world attribute an EXISTING fact already states...
> It is NOT a contradiction if a NEW fact is simply a different,
> additional fact about the same entity... When in doubt, do not report
> it — a missed contradiction is far less costly than incorrectly
> discarding a fact that was actually still true."*

A real contradiction is applied via the same `Runner.Correct` path a
human correction uses (new version, `supersedes` set, re-grounded against
the *triggering* period's own sources, not the old summary's) — never an
in-place edit.

### 3.7 Rollup staleness refresh
`refreshRollupsCovering`, always called (not just on re-consolidation) —
a day backfilled out of order after its week's rollup already ran would
otherwise never get revisited by the normal cron cadence.

### 3.8 Embedding backfill
`embedHighImportanceEpisodes` + `entitiesMissingEmbeddings`/`embedEntities`
— best-effort, logged on failure, never fails the day's consolidation
(a real, previously-fixed bug: these used to propagate errors all the
way to the scope loop, registering a transient provider hiccup as a
failed consolidation run even though the summary itself was already
committed).

---

## 4. Rollups (`RunRollup`) and corrections (`Runner.Correct`)

Weekly/monthly/yearly rollups reuse the exact same `generateSummary`
call (§3.2's prompt) with lower-level summaries as sources instead of
episodes — no separate prompt. A correction (`cmd/hupi-correct`, or
§3.6's automatic contradiction fix) always writes a **new version**
(`supersedes` + `correction_reason` set), re-running the same grounding
check (§3.4) against the original source material — a human correction
can be wrong too.

---

## 5. Quick-reference: every prompt

| Prompt constant | File | Live or cron? | One-line purpose |
|---|---|---|---|
| `summarySystemPrompt` | `internal/consolidation/prompts.go` | Cron | Distill a day/period's episodes into summary + key facts + entities + relationships |
| `perEpisodeFactPrompt` | `internal/consolidation/perepisode.go` | Cron | Narrow, per-episode "is there a standalone fact here" insurance pass |
| `groundingSystemPrompt` | `internal/consolidation/grounding.go` | Cron | Independently fact-check every extracted claim against raw source text |
| `contradictionCheckPrompt` | `internal/consolidation/contradiction.go` | Cron | Does a new fact contradict another current period's fact about the same entity |
| `aggregationExtractionSystemPrompt` | `internal/gateway/aggregation.go` | Live | Extract every dated fact relevant to an ordering/counting question |
| `imageDescribeInstruction` | `internal/gateway/attachments.go` | Live | Caption a shared image factually, for memory |
| `attributionSystemPrompt` | `internal/gateway/attribution.go` | Live (opt-in, `X-Hupi-Explain: deep`) | Which retrieved snippets did this specific answer actually rely on |
| `qaprompt.Concise` | `internal/qaprompt/qaprompt.go` | **Never live** — benchmark/EvalMem only | Short, specific, literal-scoring-friendly answer style |

## 6. Benchmark-only prompts — never live

`qaprompt.Concise` is injected **only** by `cmd/hupi-bench`'s QA phase and
`cmd/hupi-answer-question` — both evaluation tools, never
`internal/gateway.Handler`. It exists because LoCoMo/LongMemEval score
literal word-overlap or an external judge against short gold phrases
(`"7 May 2023"`), and HUPI's default hedging, multi-sentence answer style
scores near-zero against that even when retrieval was correct. Some of
its rules (the Category 5 exact-abstention-phrase tuning, specifically)
are **benchmark-vocabulary-aware tuning**, named here honestly rather
than presented as a general product capability — see the prompt's own
doc comment in source for the full, dated reasoning per rule.

---

## 7. Quick-reference: every tunable constant

| Constant | Default | Override | Calibration status |
|---|---|---|---|
| `vectorSimilarityThreshold` (summaries) | 0.40 | — | Measured (§2.2.1) |
| `entityVectorSimilarityThreshold` | 0.50 | — | Measured (§2.2.2) |
| `episodeVectorSimilarityThreshold` | 0.55 | — | Measured (§2.2.3) |
| `recommendationEntitySimilarityThreshold` | 0.25 | — | Reasoned, not yet measured |
| `orderingSummarySimilarityThreshold` | 0.25 | — | Reasoned, not yet measured |
| BM25 cutoff | `score > 0` | — | Measured (§2.2.4) |
| `bm25K1` / `bm25B` | 1.5 / 0.75 | — | Standard defaults, not yet calibrated |
| `rrfK` | 1.0 | — | Reasoned for this pool size, not yet measured |
| `mmrLambda` | 0.7 | `HUPI_MMR_LAMBDA` | Reasoned, not yet measured |
| `summaryOverfetchFactor` | 3 | — | Measured (raising final count alone was neutral) |
| `temporalRelevanceBoost` | `reciprocalRank(0)` = 0.5 | — | Real-verified against an adversarial case |
| `clusterSimilarityThreshold` | 0.60 | — | Reasoned, not yet measured |
| `clusterEpisodeThreshold` | 8 | — | Reasoned |
| `defaultMaxClustersPerDay` | 6 | `HUPI_MAX_CLUSTERS_PER_DAY` | Measured against a real 6-7-topic day |
| `perEpisodeChunkCharLimit` | 4,000 | — | Measured (5/5 vs. 3/5 unchunked) |
| `groundingCheckBatchSize` | 20 | — | Measured (reproduced count-mismatch above this) |
| `guaranteedFactMaxCount` | 3 | — | Measured (9/10 wrong vs. 10/10 correct) |
| `maxRelatedSummariesForContradictionCheck` | 5 | — | Reasoned cost bound |
| `defaultContextCharBudget` | 2,000 | `HUPI_CONTEXT_CHAR_BUDGET` | Reasoned (20,000 recommended for cloud models) |
| `defaultMaxVectorResults` | 5 | `HUPI_MAX_VECTOR_RESULTS` | Reasoned |
| `graphWalkMaxHops` / `-MaxResults` | 2 / 10 | `HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK` (off by default) | Measured: no benefit on either public benchmark |
| `keywordSearchEnabled` | on | `HUPI_ENABLE_KEYWORD_SEARCH=false` | — |
| `keywordSearchNarrowThreshold` | 500 (combined episode+summary count) | `HUPI_KEYWORD_SEARCH_NARROW_THRESHOLD` | Reasoned, not yet measured |
| `keywordSearchDisableThreshold` | 3,000 (combined episode+summary count) | `HUPI_KEYWORD_SEARCH_DISABLE_THRESHOLD` | Reasoned, not yet measured |
| `semanticFactRankingEnabled` | on | `HUPI_ENABLE_SEMANTIC_FACT_RANKING=false` | — |

Every "measured" row's real numbers live inline in the relevant source
file's own doc comment — this table points at them, it doesn't replace
them. Re-measure any similarity threshold if the embedding model
changes; none of these numbers are portable across models.
