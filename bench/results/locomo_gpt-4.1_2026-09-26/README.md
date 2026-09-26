# LoCoMo — GPT-4.1 — 2026-09-26

See [docs/BENCHMARKS.md](../../../docs/BENCHMARKS.md) for the full
write-up, caveats, and score progression. This directory holds the raw
artifacts for the final (v6) run only.

- `predictions.json` — `cmd/hupi-bench`'s own output: all 10
  conversations, 1,986 questions, each original `locomo10.json` QA entry
  plus one added `hupi_prediction` field.
- `stats.json` — `bench/score_locomo.py`'s output: per-category counts
  and accuracy, written by LoCoMo's own unmodified
  `task_eval.evaluation_stats.analyze_aggr_acc`.

## Reproduce

```
git checkout e8aaa01  # bench/locomo-longmemeval-harness at time of this run
go build -o hupi-bench ./cmd/hupi-bench
go build -o hupi-consolidate ./cmd/hupi-consolidate

# providers.yaml: active_chat_provider/active_consolidation_provider = gpt-4.1 (OpenAI),
# active_embedding_provider = text-embedding-3-small (OpenAI)

export HUPI_CONTEXT_CHAR_BUDGET=20000
./hupi-bench -benchmark locomo \
  -data-file locomo10.json \
  -all-conversations \
  -answer-model <your gpt-4.1 profile name> \
  -consolidate-bin ./hupi-consolidate \
  -out-file predictions.json

python3 bench/score_locomo.py predictions.json stats.json locomo10.json
```

`locomo10.json` is `snap-research/locomo`, pinned commit
`3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376` (see `bench/fetch-data.sh`).
