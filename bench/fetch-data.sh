#!/usr/bin/env bash
# Pulls the real LoCoMo and LongMemEval benchmark repos fresh, rather
# than vendoring them into this repo — same reasoning as build.sh's own
# "clone fresh, don't vendor" model for VS Code: both are real, actively
# maintained upstream projects (LoCoMo's own scoring depends on a
# specific Python environment; LongMemEval's is a runnable CLI), so
# pinning a commit and re-cloning is more honest than a stale copy that
# silently drifts from the real thing this harness is supposed to be
# comparable against.
#
# Usage: ./bench/fetch-data.sh
# Populates bench/data/ (gitignored) with both repos at pinned commits.
set -euo pipefail

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATA_DIR="$SELF_DIR/data"
mkdir -p "$DATA_DIR"

# Pinned commits, not just "main" — so a future upstream change to
# either benchmark's data or scoring code can't silently change what a
# past bench/results/ entry actually measured. Bump deliberately, note
# why, same as UPSTREAM_TAG bumps in hupi-code.
LOCOMO_COMMIT="3eb6f2c585f5e1699204e3c3bdf7adc5c28cb376"        # main as of 2026-09-26
LONGMEMEVAL_COMMIT="9e0b455f4ef0e2ab8f2e582289761153549043fc"   # main as of 2026-09-26

echo "==> fetching LoCoMo (snap-research/locomo)"
if [ ! -d "$DATA_DIR/locomo" ]; then
  git clone https://github.com/snap-research/locomo.git "$DATA_DIR/locomo"
fi
git -C "$DATA_DIR/locomo" fetch origin "$LOCOMO_COMMIT"
git -C "$DATA_DIR/locomo" checkout "$LOCOMO_COMMIT"

echo "==> fetching LongMemEval (xiaowu0162/LongMemEval)"
if [ ! -d "$DATA_DIR/longmemeval" ]; then
  git clone https://github.com/xiaowu0162/LongMemEval.git "$DATA_DIR/longmemeval"
fi
git -C "$DATA_DIR/longmemeval" fetch origin "$LONGMEMEVAL_COMMIT"
git -C "$DATA_DIR/longmemeval" checkout "$LONGMEMEVAL_COMMIT"

# The actual dataset isn't in the code repo above — it's hosted
# separately on HuggingFace (see LongMemEval's own README § Dataset
# Format). Starting with longmemeval_s only (the ~115k-token variant,
# ~40 sessions/history) rather than _m (~500 sessions/history, far more
# consolidation+context to replay) or _oracle (evidence-only sessions,
# which trivially inflates recall by removing the actual search
# problem) — _m and _oracle can be added once the harness is proven on
# the smaller variant.
LONGMEMEVAL_DATA_URL="https://huggingface.co/datasets/xiaowu0162/longmemeval-cleaned/resolve/main/longmemeval_s_cleaned.json"
if [ ! -f "$DATA_DIR/longmemeval_s_cleaned.json" ]; then
  echo "==> fetching LongMemEval-S dataset (~115k-token variant)"
  curl -fL "$LONGMEMEVAL_DATA_URL" -o "$DATA_DIR/longmemeval_s_cleaned.json"
fi

echo "OK: bench/data/locomo, bench/data/longmemeval, and bench/data/longmemeval_s_cleaned.json ready"
