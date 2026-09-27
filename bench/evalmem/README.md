# HUPI adapter for EvalMem

`hupi_adapter.py` implements EvalMem's (github.com/ZeyuuLiu/EvalMem)
adapter contract for HUPI, by shelling out to three real Go tools in
this repo (`cmd/hupi-ingest-turns`, `cmd/hupi-export-memory`,
`cmd/hupi-answer-question`) — no HUPI logic is reimplemented in Python.
See `docs/EVALMEM_INTEGRATION_PLAN.md` for the full design and status.

This file is **not** vendored into an EvalMem checkout. It's loaded
directly from this repo at run time.

## One-time setup

1. Build the three Go tools this adapter drives, into one directory:

   ```bash
   go build -o /path/to/bin/hupi-ingest-turns ./cmd/hupi-ingest-turns
   go build -o /path/to/bin/hupi-export-memory ./cmd/hupi-export-memory
   go build -o /path/to/bin/hupi-answer-question ./cmd/hupi-answer-question
   go build -o /path/to/bin/hupi-consolidate ./cmd/hupi-consolidate
   ```

2. In that same directory, have a real `providers.yaml` (or point
   `HUPI_PROVIDERS_CONFIG` at one) and the usual `HUPI_DATABASE_URL`/
   `HUPI_APP_DATABASE_URL`/`HUPI_KEK` env vars set, exactly as any other
   HUPI CLI tool needs — these tools connect to a real Postgres.

3. Get a real EvalMem checkout (`git clone
   https://github.com/ZeyuuLiu/EvalMem`) and put its `src/` on
   `PYTHONPATH`, alongside this directory (`bench/evalmem`) so
   `hupi_adapter` is importable by module name:

   ```bash
   export PYTHONPATH=/path/to/EvalMem/src:/path/to/hupi/bench/evalmem
   ```

   Passing `--adapter-module bench/evalmem/hupi_adapter.py` (a raw file
   path) to EvalMem's `scripts/run_eval_pipeline.py` does **not** work as
   of EvalMem `0.6.6` — its `_load_adapter()` uses
   `importlib.util.spec_from_file_location` + `exec_module` without
   registering the module in `sys.modules` first, which crashes
   `dataclasses` (`AttributeError: 'NoneType' object has no attribute
   '__dict__'` inside `_is_type`) for any `from __future__ import
   annotations` + `@dataclass` module — which this one is. Passing
   `--adapter-module hupi_adapter` (a plain module name, resolved via
   `importlib.import_module` through the `PYTHONPATH` above) avoids the
   bug entirely and is the supported invocation. No edit to EvalMem's own
   `registry.py` is required either way.

## Running EvalMem's own pipeline against HUPI

```bash
python3 /path/to/EvalMem/scripts/run_eval_pipeline.py \
  --dataset /path/to/locomo10.json \
  --adapter-module hupi_adapter \
  --adapter-class HupiMemoryAdapter \
  --adapter-config-json '{"bin_dir":"/path/to/bin","answer_model":"local-ollama","consolidate_bin":"/path/to/bin/hupi-consolidate","skip_consolidate":false}' \
  --limit 1 \
  --output outputs/hupi_eval.json
```

`--adapter-config-json` fields (all optional, see `HupiMemoryAdapterConfig`):

| field | default | meaning |
|---|---|---|
| `bin_dir` | `.` | directory containing the three built Go tools |
| `answer_model` | `""` (active_chat_provider) | `providers.yaml` profile to answer with |
| `consolidate_bin` | `./hupi-consolidate` | path to the consolidation binary |
| `skip_consolidate` | `false` | skip real consolidation (fast smoke tests only — a real evaluation needs it to have actually run) |

**Real ingestion + consolidation is slow.** A 3-session/58-turn LoCoMo
conversation replayed through the real gateway and consolidated via
local Ollama took **~12 minutes** end to end in this repo's own
mechanics test (`ingest_conversation` alone: 738s) — most of it is the
consolidation LLM calls, one per distinct calendar date touched. Budget
accordingly for anything beyond a `--limit 1`-style smoke test; a real
run against a real cloud answer model would be faster per call but still
pays this cost per conversation, once.

## Verified smoke test (step 6, mechanics only — not a scored run)

Run against a real, ingested HUPI scope, real Postgres, real local
Ollama, with `--no-llm-assist --allow-rule-fallback` (EvalMem's own
rule-based judging only — **zero cloud LLM spend**, per
`docs/EVALMEM_INTEGRATION_PLAN.md`'s step 7 gate on any real judge-model
run):

```bash
python3 .../scripts/run_eval_pipeline.py \
  --dataset /path/to/a/locomo10.json \
  --adapter-module hupi_adapter --adapter-class HupiMemoryAdapter \
  --adapter-config-json '{"bin_dir":"...","answer_model":"local-ollama","consolidate_bin":".../hupi-consolidate"}' \
  --limit 1 --no-llm-assist --allow-rule-fallback \
  --output outputs/smoke.json
```

(`--limit 1` only evaluates the first question, which comes from
whichever conversation is first in the dataset file — trim your own copy
of `locomo10.json` down to one conversation first if you want a fast,
predictable smoke test rather than waiting on whichever conversation
happens to be first.)

Result: pipeline completed with no adapter-contract errors
(`"errors": 0`, `"total": 1`), producing a real, internally-consistent
diagnostic verdict for the one evaluated question ("When did Caroline go
to the LGBTQ support group?", gold answer "7 May 2023"):

- **Encoding: MISS (`EM`)** — the rule-based token-overlap match
  (`find_memory_records`) found no candidate matching the exact,
  time-stamped `f_key` phrase. Expected: HUPI's consolidation
  paraphrases into summaries/key_facts rather than storing verbatim
  quotes, so an exact-phrase match against raw dialogue text misses even
  though the fact *is* encoded (visible directly in the retrieved
  context: "Caroline and Melanie had a supportive conversation...").
- **Retrieval: MISS** — same literal-phrase-match limitation applied to
  `C_original`, despite the real retrieved context including directly
  relevant Caroline/support-group summaries.
- **Generation: FAIL (`GRF`)** — both the online and oracle answers
  reference the wrong date ("May 8" vs. gold "7 May 2023") or hedge
  ("not specified"); the oracle context itself only carried a relative
  date ("yesterday"), not an absolute one — the same relative-date
  benchmark quirk `docs/BENCHMARKS.md`'s own LoCoMo write-up already
  documents and works around with a prompt instruction, not present in
  this adapter's oracle-mode path.

None of this indicates an adapter bug — it's EvalMem's own rule-based
(non-LLM) judge being maximally strict, exactly as expected with
`--no-llm-assist`. The genuinely new information from this test is that
the **pipeline plumbing itself works**: every adapter method got called
with real arguments and returned real, well-formed data, and EvalMem's
attribution logic ran to completion and produced a structurally valid
report.

## Real dependencies still unconfirmed (deferred to step 7)

Deliberately not checked here, since checking them means spending real
money or a real download, neither needed just to prove the pipeline
runs:

- `gpt-5-mini` / `gpt-4o-mini` judge-model reachability for
  `--llm-assist` (used for the *real* LLM-assisted judgement this smoke
  test explicitly disabled).
- `Qwen3-Embedding-0.6B` (EvalMem's own AgenticRAG/MemWiki dependency,
  unrelated to HUPI's own embedding config).
