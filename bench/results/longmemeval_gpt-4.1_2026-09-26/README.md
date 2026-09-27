# LongMemEval — GPT-4.1 (answer) / GPT-4o (judge) — 2026-09-26

See [docs/BENCHMARKS.md](../../../docs/BENCHMARKS.md) §7 for the full
write-up and caveats. This is a bounded 48-instance stratified sample
(8 per question type — scaled up from an initial 18-instance/3-per-type
pilot once mechanics were confirmed sound), not the full 500-instance
`_s` dataset — see BENCHMARKS.md for why.

- `sample_instances.json` — the exact 48 instances sampled from
  `longmemeval_s_cleaned.json` (seed 42, two draws — an initial 18 plus
  30 more with the first 18 excluded), so this run is re-derivable
  without needing the full 264MB dataset.
- `predictions.jsonl` — `cmd/hupi-bench -benchmark longmemeval`'s own
  output: `{"question_id", "hypothesis"}` per line.
- `eval-results-gpt-4o.jsonl` — `evaluate_qa.py`'s own output: each
  prediction plus its `autoeval_label` (GPT-4o judge verdict).

## Reproduce

```
git checkout dd1a78f  # bench/locomo-longmemeval-harness at time of the final 48-instance run
go build -o hupi-bench ./cmd/hupi-bench
go build -o hupi-consolidate ./cmd/hupi-consolidate

# providers.yaml: active_chat_provider/active_consolidation_provider = gpt-4.1 (OpenAI),
# active_embedding_provider = text-embedding-3-small (OpenAI)

export HUPI_CONTEXT_CHAR_BUDGET=20000
./hupi-bench -benchmark longmemeval \
  -data-file sample_instances.json \
  -all-conversations \
  -answer-model <your gpt-4.1 profile name> \
  -consolidate-bin ./hupi-consolidate \
  -out-file predictions.jsonl

export OPENAI_API_KEY=...
bash bench/score_longmemeval.sh gpt-4o predictions.jsonl sample_instances.json
```
