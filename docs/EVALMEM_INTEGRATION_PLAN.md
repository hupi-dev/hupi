# EvalMem Integration Plan — operation-level diagnostics on top of the existing harness

Status: **planning/scaffolding only — no evaluation runs yet.** This
branch (`feature/evalmem-integration`) exists to work out a concrete
adapter design and get it reviewed before any real judge-model spend, the
same discipline the LoCoMo/LongMemEval benchmark plan itself followed
(see `docs/BENCHMARKS.md`'s own Step 1-7 sequencing).

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
  this branch (§7) unless separately scoped.

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
   moved to §7 as a non-goal for this branch.
6. **Register the adapter**, confirm `gpt-5-mini`/embedding-model
   dependencies, do a tiny (single-conversation, `--limit 1`-style) smoke
   test — still not a real scored run, just confirming the pipeline
   executes end to end without error. `bench/evalmem/hupi_adapter.py`
   itself has not yet been smoke-tested against a real EvalMem Python
   install/import — next.
7. **Real evaluation run** — explicitly out of scope until this plan is
   reviewed and steps 2-6 are actually built and smoke-tested. No cloud
   judge spend happens before this step is separately approved.

## 7. Non-goals for this branch, for now

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
