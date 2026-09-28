# EvalMem Integration Plan — operation-level diagnostics on top of the existing harness

Status: **all 7 steps done, including one real evaluation run** (a single
bounded LoCoMo conversation, `conv-26`, real GPT-4.1 + real gpt-4o-mini
judge — see step 7 in §6 for the full result and the two real bugs it
surfaced and fixed). This branch (`feature/evalmem-integration`) started
as planning/scaffolding to work out a concrete adapter design and get it
reviewed before any real judge-model spend, the same discipline the
LoCoMo/LongMemEval benchmark plan itself followed (see
`docs/BENCHMARKS.md`'s own Step 1-7 sequencing) — spend was only
authorized once that design was actually built and smoke-tested.

## 1. What EvalMem actually is, verified against the real repo

[EvalMem](https://github.com/ZeyuuLiu/EvalMem) (MIT-licensed, EMNLP 2026
submission) is a diagnostic framework that decomposes a wrong answer into
*where* it went wrong, rather than reporting a single opaque score. Three
parallel Examiners run per question:

- **Encoding Examiner** — given the question, its annotated key facts
  (`F_key`), and a full export of the memory store (`M`), was the
  information ever actually stored?
- **Retrieval Examiner** — given the same key facts and the system's own
  *native* retrieved context (`C_original`), did retrieval surface usable
  evidence?
- **Generation Examiner** — given *oracle* (perfect, hand-annotated)
  context (`C_oracle`), can the answer model get it right at all?

An Attribution Agent merges the three into a multi-label defect code from
an 11-code taxonomy (`EM`/`EA`/`EW`/`DMP` encoding, `RF`/`LATE`/`NOI`/`NIR`
retrieval, `GH`/`GF`/`GRF` generation), with a "causal shield" that
suppresses downstream codes that are just consequences of an upstream
failure (e.g. don't blame retrieval for missing something that was never
encoded in the first place).

**Why this is worth doing, concretely, not just "more rigor for its own
sake"**: this session's own manual investigation — tracing "Where has
Melanie camped?" through `cmd/hupi-trace`, a throwaway `summary_key_facts`
dump tool, and a direct `Retrieve()` call to find that the fact was
encoded but never surfaced (`docs/BENCHMARKS.md` §3's `2105cc5` fix) — is
exactly one instance of the `RF` (Retrieval Failure) diagnosis EvalMem
would have produced automatically, for every question, not just the ones
we happened to manually chase down. EvalMem formalizes and scales the
exact debugging method that found HUPI's single biggest LoCoMo fix.

## 2. The real adapter interface, verified against real adapter code

Integrating a new memory system means implementing `EvalAdapterProtocol`
(`src/memory_eval/adapters/base.py`'s `BaseMemoryAdapter`, subclassed by
e.g. `o_mem_adapter.py`, `generic_text_adapter.py`) — a plain Python
class registered in `src/memory_eval/adapters/registry.py` under a new
`--memory-system` key (e.g. `hupi_stable_eval`):

| Method | What it must do |
|---|---|
| `ingest_conversation(sample_id, conversation)` | Replay a benchmark conversation's turns into the system under test, return a `run_ctx` handle for later calls |
| `export_full_memory(run_ctx)` | Dump the system's *entire* stored memory for that scope — the `M` the Encoding Examiner inspects |
| `find_memory_records(run_ctx, query, f_key, memory_corpus)` | Locate which exported records support the annotated key facts |
| `retrieve_original(run_ctx, query, top_k)` | The system's real, native retrieval — `C_original` |
| `generate_online_answer(run_ctx, query, top_k)` | The real end-to-end answer, retrieval included |
| `generate_oracle_answer(run_ctx, query, oracle_context)` | Answer using hand-fed oracle context instead of real retrieval — isolates the Generation Examiner from retrieval quality entirely |

## 3. Mapping each method onto code that already exists

This is the encouraging part: nothing here needs inventing from scratch.

- **`ingest_conversation`** → already `cmd/hupi-bench`'s `replayConversation`
  (`cmd/hupi-bench/replay.go`) — a real `gateway.Handler`, backdated
  timestamps, real capture. The adapter's `run_ctx` is just "which scope
  (userID) this sample landed in."
- **`retrieve_original`** → already `internal/store.Retrieve`'s
  `result.ContextMessage` — currently computed and consumed internally by
  `gateway.Handler` per request, but not captured/exported by the harness.
  Needs: a small capture point in `cmd/hupi-bench` (or a direct
  `internal/store` call from a new Go export tool) to save it per
  question instead of discarding it after the answer call.
- **`generate_online_answer`** → already `answerQuestions`
  (`cmd/hupi-bench/replay.go`) — the real end-to-end answer path, no
  changes needed.
- **`generate_oracle_answer`** → structurally identical to the existing
  `-baseline` mode (`runBaselineConversation`, added for the no-memory
  control in `docs/BENCHMARKS.md` §5) — that code already demonstrates
  "stuff exact given text into context, ask the same answer model,
  bypass real retrieval entirely." Feeding it `C_oracle` instead of the
  full raw transcript is the same mechanism, narrower input.
- **`export_full_memory`** → the one genuinely new piece, but a small
  one: this session already built (and discarded, as throwaway
  investigation tooling) exactly this — `dump_facts`/`dump_episodes`-style
  tools that decrypt and dump a scope's entities, summaries,
  `summary_key_facts`, and `entity_relationships`. Formalizing one of
  those into a real, maintained export (likely a new `cmd/hupi-export-memory`
  reusing `internal/store`/`internal/crypto`, or an admin HTTP endpoint)
  is the actual new work item.
- **`find_memory_records`** → per-adapter logic; the reference
  (`generic_text_adapter.py`) does simple token-overlap matching over the
  exported corpus. HUPI's adapter can do the same generically, or reuse
  HUPI's own BM25 scorer (`internal/store/bm25.go`) for a more faithful
  match.

## 4. Annotation requirements — already satisfied by data we already load

EvalMem needs each question annotated with key facts (`F_key`) and oracle
context (`C_oracle`). We don't need to build new annotations, and — a
finding confirmed by actually reading EvalMem's own source
(`src/memory_eval/dataset/locomo_builder.py`), not just the paper's
prose — **we don't need to resolve them ourselves either, for LoCoMo**:

- **LoCoMo**: the `evidence` field (e.g. `["D1:3"]`) is turn IDs pointing
  at oracle source dialogue, exactly as expected. But EvalMem's own
  `build_locomo_eval_samples()` already reads LoCoMo's raw JSON directly
  — its own `_flatten_conversation`/`_build_from_evidence` resolve every
  question's `evidence` IDs into `f_key`/`oracle_context` *before* ever
  calling into our adapter. `find_memory_records`/`generate_oracle_answer`
  receive `f_key`/`oracle_context` as ready-made arguments; there is no
  evidence-resolution code for the HUPI side to write. See §6 step 5 for
  what this means for the sequencing below.
- **LongMemEval**: `answer_session_ids` plus each turn's own `has_answer`
  boolean (confirmed in `bench/FORMAT.md`, Step 1 of the original
  benchmark plan) is the analogous annotation data — but EvalMem's own
  repository ships a LoCoMo sample builder only, no LongMemEval one, so
  there is currently no EvalMem-side consumer for it. Writing one would
  mean building a new `build_longmemeval_eval_samples()`-equivalent
  ourselves, mirroring `locomo_builder.py`'s own resolution logic — a new
  builder, not "small parsing addition." Treated as a real non-goal for
  this branch (§8) unless separately scoped.

EvalMem's own reported LoCoMo POS/NEG split (1,540/446) matches our own
category-5-as-NEG breakdown exactly — real, independent confirmation that
our existing category/`questionType` fields on `qaItem` already line up
with EvalMem's task-type (`τ`) requirement with no remapping needed.

## 5. Real dependencies to verify before any actual run (not yet done)

- **Judge model**: EvalMem's reference config (`configs/keys.local.json`)
  defaults to `gpt-4o-mini`; the paper's own reported numbers use
  `gpt-5-mini`. Either should be reachable via the same OpenAI account
  already used for LoCoMo/LongMemEval's own judge calls — not yet
  confirmed for `gpt-5-mini` specifically.
- **Embedding model**: EvalMem's own AgenticRAG/MemWiki components
  (used *inside* the Encoding Examiner, not a replacement for HUPI's own
  retrieval) expect `Qwen3-Embedding-0.6B`, a local sentence-transformers
  model — a new, separate dependency from HUPI's own
  `text-embedding-3-small`, installed for EvalMem's tooling only.
- **Python environment**: EvalMem is a `pip install -e .` Python package
  (≥3.9), entirely separate from HUPI's own Go toolchain — this
  integration lives alongside `cmd/hupi-bench`, not inside it.

## 6. Sequenced steps

1. ✅ Research (this doc) — real interface confirmed against actual
   adapter source, not just the paper's prose description.
2. ✅ **Formalize a real memory-export tool** — `Store.ExportMemory` +
   `cmd/hupi-export-memory` (`60d6c8f`). Verified against real Postgres
   and a real scope from this session's own LoCoMo benchmark data.
3. ✅ **Add `C_original` capture** — `Handler.OnRetrieve`, a nil-by-default
   seam (`48821b9`), same pattern as the existing `Now`/`NewID` test
   seams. `cmd/hupi-bench` captures it via a new, deliberately separate
   `-retrieved-context-out-file` output. Verified end to end against
   real Postgres + real local Ollama.
4. ✅ **Write the HUPI adapter** — `cmd/hupi-ingest-turns`,
   `cmd/hupi-answer-question`, and `bench/evalmem/hupi_adapter.py`
   (`4b69d52`). Verified end to end against a real ingested scope
   (`user:evalmem-conv30`, real Postgres + real local Ollama): ingest +
   consolidation, oracle-mode answer, and native-mode answer all
   confirmed correct. Along the way, found and fixed a real bug —
   `cmd/hupi-answer-question` skipped `bootstrap.VerifyEmbedding`, so
   query-time embedding used the raw (unpadded) provider dimension while
   consolidation's stored vectors were the verified/zero-padded one,
   causing every native-mode retrieval to fail with a Postgres
   vector-dimension mismatch.
5. ✅ **Resolve LoCoMo/LongMemEval evidence fields into `F_key`/`C_oracle`**
   — re-verified against EvalMem's actual source
   (`src/memory_eval/dataset/locomo_builder.py`): for LoCoMo this step is
   a no-op on the HUPI side. EvalMem's own `build_locomo_eval_samples()`
   resolves every question's raw `evidence` IDs into `f_key`/
   `oracle_context` itself, reading LoCoMo's raw JSON directly — before
   ever calling into our adapter (see §4, updated). There is no evidence-
   resolution code to write here. For LongMemEval, EvalMem ships no
   sample builder at all yet, so there's no consumer to resolve
   `answer_session_ids`/`has_answer` for either — writing one would be a
   new builder (mirroring `locomo_builder.py`), not a small addition;
   moved to §8 as a non-goal for this branch.
6. ✅ **Register the adapter, smoke test.** Registration needs no edit to
   EvalMem's own `registry.py`: its `scripts/run_eval_pipeline.py` loads
   an adapter directly via `--adapter-module hupi_adapter --adapter-class
   HupiMemoryAdapter` (module-name form, not the raw-file-path form —
   see `bench/evalmem/README.md` for a real bug in EvalMem `0.6.6`'s
   file-path loader this sidesteps). Ran the real pipeline
   (`--limit 1 --no-llm-assist --allow-rule-fallback`, zero cloud LLM
   spend — EvalMem's own rule-based judge only) against a real EvalMem
   checkout, real Postgres, real local Ollama, a trimmed 3-session LoCoMo
   conversation: completed with `"errors": 0`, a structurally valid
   diagnostic report (`EM`/`GRF` defect codes, consistent with HUPI's
   paraphrased-summary retrieval missing an exact-phrase rule-based
   match, and the same relative-date benchmark quirk `docs/BENCHMARKS.md`
   already documents for LoCoMo). Full detail in
   `bench/evalmem/README.md`.

   Two real bugs found and fixed along the way, not caught until this
   test actually ran against the real end-to-end pipeline rather than
   the Go tools directly:
   - `ingest_conversation`'s input shape was wrong. The real pipeline
     (`memory_eval.pipeline.runner._conversation_to_turns`, verified
     against actual source) hands adapters flat
     `{turn_index, speaker, text, timestamp}` turns — a different,
     simpler shape than `locomo_builder._flatten_conversation`'s
     `dia_id`/`session_index`/`session_datetime` shape this adapter had
     assumed applied uniformly (it's actually only used internally by
     EvalMem's own F_key/oracle-context resolution, a separate code
     path — see §4). Fixed by adding `HupiMemoryAdapter._to_hupi_turns`,
     which re-derives session boundaries from timestamp changes before
     calling `cmd/hupi-ingest-turns`.
   - The adapter's config dataclass was named `HupiAdapterConfig`, but
     EvalMem's generic adapter-loading convention
     (`scripts/run_eval_pipeline.py`'s `_load_adapter`) requires it be
     named exactly `f"{adapter_class}Config"` — every other adapter in
     the repo follows this (`OMemAdapterConfig` for `OMemAdapter`, etc).
     Renamed to `HupiMemoryAdapterConfig` to match.

   Also added `build_trace_for_query` (unused by the primary pipeline,
   confirmed by reading `FullEvalAdapterProtocol`'s actual definition —
   but every other adapter implements it, for the separate
   `EvalAdapterProtocol` some other tooling may use) and corrected
   `capabilities()`'s `supports_high_recall_candidates` to `False` (this
   adapter doesn't implement `hybrid_retrieve_candidates`).

   Real timing data point for future runs: a 3-session/58-turn
   conversation's real ingest+consolidation (local Ollama) took ~12
   minutes end to end — budget accordingly; this is why the smoke test
   used a trimmed single conversation rather than the full dataset.

   **Follow-up fix (`ebb9b96`)**: while estimating step 7's real cost,
   found that `retrieve_original` and `generate_online_answer` — called
   independently by EvalMem's Retrieval and Generation probes, which run
   concurrently via `ParallelThreeProbeEvaluator`'s own thread pool —
   each independently shelled out to `hupi-answer-question` in native
   mode for the *same* `(run_ctx, query)`, silently doubling real
   HUPI-side retrieval+generation cost per question. Fixed with a
   lock-guarded per-adapter cache (`_native_answer`), keyed on
   scope+query. Verified both in isolation (mocked `_answer`, including a
   two-thread race on the same query — exactly 1 real call either way)
   and against real infra (reusing the already-ingested
   `user:evalmem-conv-26` scope: `retrieve_original` took a real 16.0s,
   the immediately following `generate_online_answer` for the same query
   returned in 0.0s from cache).
7. ✅ **Real evaluation run** — explicitly approved by the user for a
   single bounded sample conversation, with HUPI's own answer/
   consolidation model moved to OpenAI (GPT-4.1, real
   `text-embedding-3-small`), against real Postgres, one full real LoCoMo
   conversation (`conv-26`: 19 sessions, 199 questions), with EvalMem's
   own real LLM-assisted judgement enabled (`gpt-4o-mini`, not the
   rule-only mode steps 1-6 used).

   **First attempt hit a real bug in EvalMem's own strict-mode generation
   probe** (not ours): it makes two independent LLM judge calls to assess
   the oracle answer — one decides PASS/FAIL, the other decides the FAIL
   *substate* (must be `GF`/`GRF`) — and when they disagree (correctness
   judge says wrong, substate judge independently says `NONE`/correct),
   strict mode has no valid substate and hard-raises. This hit ~70% of
   POS questions on `conv-26` (42/60 processed at the time, real,
   observed). Fixed by adding `--allow-rule-fallback` (a real EvalMem
   flag, not a workaround we invented) — re-ran clean: **199/199
   questions, 0 errors**.

   That first clean run's `final_accuracy` (9.5%, `pos: 10.4%`,
   `neg: 6.7%`) was far below the published LoCoMo score (50.2%) — traced
   to a real, separate bug found by re-reading our own tool's code:
   `cmd/hupi-answer-question` sent **no system prompt at all**, unlike
   `cmd/hupi-bench`'s QA loop, which deliberately uses
   `qaConcisenessPrompt` because HUPI's default verbose/hedging answer
   style scores badly against literal graders even when retrieval is
   correct (`replay.go`'s own doc comment). Added the same prompt to
   `cmd/hupi-answer-question` (`724f633`) and re-ran clean on a freshly
   wiped scope: **199/199, 0 errors, final_accuracy 9.5% → 29.1%**
   (`neg_final_accuracy` 6.7% → **82.2%**, closely matching the tuned
   harness's own 75% LongMemEval abstention number; `pos_final_accuracy`
   10.4% → 13.6%).

   Retrieval (`RF`/`NOI`) and encoding (`EM`) defect counts stayed
   essentially flat across both runs (as expected — a system-prompt fix
   doesn't touch retrieval/encoding), confirming the step-6 smoke test's
   own finding still holds at full scale: this adapter's `find_memory_records`
   (plain token-overlap) misses real, correctly-encoded facts that are
   paraphrased rather than quoted verbatim in HUPI's summaries. The
   remaining gap between `pos_final_accuracy` (13.6%) and the official
   50.2% F1 score is very plausibly this matching-methodology artifact,
   not a true HUPI capability regression — not independently verified
   further; would need a better `find_memory_records` (e.g. BM25) to
   isolate cleanly.

## 7. Post-step-7 tuning experiments — real findings, not all wins

After the clean step-7 run (29.1% final_accuracy), the user asked for
pointers on what would move the score. Rather than guess, the actual
retrieval code was read first — this surfaced two real, concrete,
checkable levers already in `internal/store/retrieve.go`:

- `defaultContextCharBudget = 2000` — a small default explicitly
  documented as "safe for a modest local model," overridable via
  `HUPI_CONTEXT_CHAR_BUDGET`, never set for the step-7 run despite using
  GPT-4.1. A `"...[truncated to fit context budget]"` marker was directly
  observed in an earlier context dump for this same scope.
- `maxVectorResults = 5` (hardcoded) — only the top-5 vector-similarity
  summaries/entities/episodes are ever considered, independent of the
  char budget.

Five more full real runs followed (each: `conv-26`, 19 sessions, 199
questions, real GPT-4.1 + real gpt-4o-mini judge, `--allow-rule-fallback`,
a freshly wiped scope). All 199/199, 0 errors, every time.

**Context-budget sweep** (isolating one variable at a time):

| Budget | final_accuracy | pos | neg | correct/199 |
|---|---|---|---|---|
| 2,000 (default) | 29.1% | 13.6% | 82.2% | 58 |
| 8,000 | 30.2% | 20.1% | 64.4% | 60 |
| **20,000** | **36.2%** | **27.9%** | 64.4% | **72** |

No middle sweet spot: the NEG/abstention cost hits its floor already by
8,000 chars (64.4%, identical at 20,000), while POS accuracy keeps
climbing all the way to 20,000 — so within this tested range, higher is
strictly better, not a tradeoff to balance. Likely mechanism: a much
larger budget lets in more tangentially-related content alongside the
genuinely relevant content, which helps POS recall but costs some NEG
precision — a real, understood cost, not unexplained noise.

**`HUPI_MAX_VECTOR_RESULTS` made configurable** (`74cff19`), same
"informed minority override" pattern as the char budget. Tested at 12
(vs. default 5) on top of the 20,000-char budget:

| Config | final_accuracy | pos | neg | correct/199 |
|---|---|---|---|---|
| 20k budget alone | 36.2% | 27.9% | 64.4% | 72 |
| 20k + `mvr=12` alone (no BM25) | 35.7% | 26.0% | 68.9% | 71 |

Essentially neutral — not the win it looked like it might be. Likely
mechanism, inspected directly in a real retrieved_context dump for a
real question: `conv-26` is a two-speaker conversation where nearly
every summary mentions both speakers by name, so raising the candidate
cap mostly pulls in *redundant* near-duplicate summaries rather than
new relevant facts, and the context still hit the truncation limit
regardless.

**BM25-ranked `find_memory_records`** (`6bd17ba`) — replacing
`generic_text_adapter.py`'s plain token-overlap-count matching with a
small, self-contained Okapi BM25 implementation (no new dependency;
`rank_bm25` isn't installed and EvalMem's own agentic-rag extra pulls in
a much heavier stack deliberately not adopted here). Tested combined
with `mvr=12` + the 20k budget:

| Config | final_accuracy | pos | neg | correct/199 |
|---|---|---|---|---|
| `mvr=12` alone (no BM25) | 35.7% | 26.0% | 68.9% | 71 |
| `mvr=12` + BM25 | 32.7% | 20.8% | 73.3% | 65 |

A real regression, isolated cleanly against the `mvr=12`-alone run above
(same budget, same `mvr`, only BM25 added). The BM25 run was also the
*only* one of all six to show new `CORRUPT_WRONG`/`EW` encoding states —
a concrete tell, not just a lower number. Root cause, found by reading
`split_tokens`' actual source: it does **no stopword filtering** at all
(`re.split(r"\W+", normalize_text(text))`), so pure function-word overlap
("her", "the", "was") between a query and a genuinely unrelated record
produced a spurious nonzero BM25 score, handing the Encoding Examiner a
wrong record as if it were a real match. Fixed (`62402e8`) by filtering a
small stopword list out of both the signal and document tokens before
scoring — verified in isolation (a record whose only overlap was "her"
is now correctly excluded) but **not re-verified with a full run** —
would need one more real run to confirm the fix actually recovers to at
or above the `mvr=12`-alone baseline.

**Entity-relationship graph walk, re-verified against real data** — a
still-open item from `docs/BENCHMARKS.md`, never independently checked
before. Added a real observability marker (`", relationship graph"`,
`0435e99`) since a graph-walked entity was previously textually
indistinguishable from one found by direct vector match or by
`stage1EntityMatches`' direct name-mention scan — all three go through
the same `formatEntity` helper. Rebuilt and tested directly (not a full
pipeline run — reused the already-ingested scope) against all 32 of
`conv-26`'s real LoCoMo multi-hop-category questions:

**The graph walk fired 0 times out of 32**, despite 84 real, active
`entity_relationships` rows existing for this scope. Inspected one
example directly (`"Where did Caroline move from 4 years ago?"`):
`person:caroline-s-family`/`person:caroline-s-friends` — both real
graph-connected entities — appear in the retrieved context, but got
there via direct vector match, not the walk. Working hypothesis: in a
two-speaker conversation where nearly every summary mentions both people
by name, almost everything the graph walk could reach hop-by-hop is
already surfaced by direct vector/keyword search first, leaving nothing
new for a hop to add. LoCoMo's own "multi-hop" category tests
cross-*session* narrative connections (a fact mentioned in session 3,
combined with one from session 7), not multi-*edge* graph traversal —
plausibly not the shape of question this feature was actually built to
help with. Not confirmed against a denser, more graph-shaped dataset.

**Net conclusion**: of everything tried, only the context-budget increase
is a confirmed, unambiguous win (29.1% → 36.2%, real GPT-4.1 model, no
other changes). `maxVectorResults` is neutral. BM25 needed a real fix
before it could even match the neutral baseline, and that fix itself is
unverified. Graph walk provably isn't contributing on this dataset's
question shape. None of this changes step 7's underlying scored result
(29.1%/9.5%) — these are follow-up tuning experiments run after and
separate from the reported step-7 numbers, on `feature/evalmem-tuning`,
not `feature/evalmem-integration`.

## 8. Non-goals for this branch, for now

- Not running any real EvalMem evaluation yet (per the explicit
  instruction that started this branch).
- Not adopting MemWiki or AgenticRAG as retrieval improvements to HUPI
  itself — those are EvalMem's own diagnostic-only components, read-only
  probes that never touch the system under test.
- Not building DynaMem-Bench support — LoCoMo and LongMemEval-S are
  already what this repo's own harness targets; DynaMem-Bench is a third,
  separate dataset EvalMem's authors built for their own paper.
- Not writing a LongMemEval sample builder for EvalMem — EvalMem's own
  repository only ships a LoCoMo builder (§4, §6 step 5); adding
  LongMemEval support to EvalMem itself is a new component, not covered
  by this plan's current scope.
