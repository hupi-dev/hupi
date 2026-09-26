#!/usr/bin/env bash
# Scores cmd/hupi-bench's LongMemEval predictions using LongMemEval's own,
# completely unmodified evaluation scripts (src/evaluation/evaluate_qa.py,
# src/evaluation/print_qa_metrics.py, fetched fresh by bench/fetch-data.sh
# into bench/data/longmemeval, never vendored or reimplemented here).
#
# Unlike LoCoMo's F1/EM scoring, LongMemEval's own methodology is an
# LLM-judge (see evaluate_qa.py's get_anscheck_prompt, quoted verbatim in
# the reviewed plan for transparency) — this requires a real judge model,
# either OPENAI_API_KEY (gpt-4o or gpt-4o-mini, the only two OpenAI
# entries in their own model_zoo) or a local vLLM-style endpoint on
# localhost:8001 (their llama-3.1-70b-instruct entry). There is no
# judge-free path: that's LongMemEval's own real design, not a gap in
# this harness.
#
# Usage:
#   export OPENAI_API_KEY=...          # if using gpt-4o / gpt-4o-mini
#   export OPENAI_ORGANIZATION=...     # optional, only if your key spans multiple orgs
#   ./bench/score_longmemeval.sh <metric_model> <hyp_file> [ref_file]
#
#   metric_model: one of gpt-4o, gpt-4o-mini, llama-3.1-70b-instruct
#                 (evaluate_qa.py's own model_zoo — see that file for
#                 the real, current list, not duplicated here since it
#                 would drift).
#   hyp_file:     cmd/hupi-bench's own -benchmark longmemeval output —
#                 JSONL of {"question_id": ..., "hypothesis": ...}.
#   ref_file:     defaults to bench/data/longmemeval_s_cleaned.json —
#                 must be the same instances hyp_file covers, matched by
#                 question_id. The real ground-truth file, never modified.
#
# Writes <hyp_file>.eval-results-<metric_model> (evaluate_qa.py's own
# naming), then runs print_qa_metrics.py against it for the final
# per-question_type accuracy breakdown — note the real script only takes
# (in_file, ref_file), two positional args: the README's own documented
# usage (`print_qa_metrics.py gpt-4o file.log ref.json`, three args) does
# not match what the actual code checks (`len(sys.argv) != 3`, i.e. two
# args after the script name) — a real discrepancy between their docs and
# their code, found by reading the source directly rather than trusting
# the README, same as bench/FORMAT.md's own LoCoMo findings.
#
# A second real gotcha found the same way: print_qa_metrics.py hardcodes
# assert entry['autoeval_label']['model'] == 'gpt-4o-2024-08-06' — it
# only works when METRIC_MODEL was exactly "gpt-4o" (the one model_zoo
# entry that resolves to that literal string). Using gpt-4o-mini or
# llama-3.1-70b-instruct as the judge produces a valid
# .eval-results-<model> file from evaluate_qa.py, but print_qa_metrics.py
# itself will hard-crash on it — their own per-category breakdown script
# is effectively gpt-4o-only as shipped.
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LONGMEMEVAL_REPO="$SELF_DIR/data/longmemeval"

if [ "$#" -lt 2 ]; then
  echo "usage: $0 <metric_model> <hyp_file> [ref_file]" >&2
  exit 1
fi

METRIC_MODEL="$1"
HYP_FILE="$2"
REF_FILE="${3:-$SELF_DIR/data/longmemeval_s_cleaned.json}"

cd "$LONGMEMEVAL_REPO/src/evaluation"
python3 evaluate_qa.py "$METRIC_MODEL" "$HYP_FILE" "$REF_FILE"

RESULT_FILE="${HYP_FILE}.eval-results-${METRIC_MODEL}"
python3 print_qa_metrics.py "$RESULT_FILE" "$REF_FILE"
