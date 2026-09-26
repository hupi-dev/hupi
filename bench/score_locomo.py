#!/usr/bin/env python3
"""Scores cmd/hupi-bench's LoCoMo predictions using LoCoMo's own,
completely unmodified scoring code (task_eval.evaluation and
task_eval.evaluation_stats, imported directly from bench/data/locomo --
fetched fresh by bench/fetch-data.sh, never vendored or reimplemented
here). Requires bert-score/nltk/regex/numpy/tqdm installed (bert-score
pulls in torch/transformers transitively purely as a top-level import
in their own evaluation.py -- unused by the functions this script
actually calls, but still required to import the module at all, since
that's a real, unmodified dependency of their real, unmodified code).

Usage:
    python3 bench/score_locomo.py <predictions.json> <output_stats.json> [ann_file]

    predictions.json: cmd/hupi-bench's own output -- a list of
                       {"sample_id": ..., "qa": [...]}, each qa entry
                       being the ORIGINAL locomo10.json entry plus one
                       added "hupi_prediction" field. This is the same
                       shape LoCoMo's own evaluate_qa.py writes, on
                       purpose -- see main.go's own doc comment.
    output_stats.json: where analyze_aggr_acc writes per-category stats
                       (merges into this file under the "hupi" key if it
                       already holds other models' stats, matching their
                       own multi-model-comparison convention).
    ann_file:          defaults to bench/data/locomo/data/locomo10.json
                       -- must be the same conversations predictions.json
                       covers, matched by sample_id. This is the real
                       ground-truth file, never modified.
"""
import sys
import os
import json

SELF_DIR = os.path.dirname(os.path.abspath(__file__))
LOCOMO_REPO = os.path.join(SELF_DIR, "data", "locomo")
sys.path.insert(0, LOCOMO_REPO)

from task_eval.evaluation import eval_question_answering  # noqa: E402
from task_eval.evaluation_stats import analyze_aggr_acc  # noqa: E402

MODEL_KEY = "hupi"
PREDICTION_KEY = "hupi_prediction"


def main():
    if len(sys.argv) < 3:
        print(f"usage: {sys.argv[0]} <predictions.json> <output_stats.json> [ann_file]")
        sys.exit(1)
    pred_file = sys.argv[1]
    stats_out = sys.argv[2]
    ann_file = sys.argv[3] if len(sys.argv) > 3 else os.path.join(LOCOMO_REPO, "data", "locomo10.json")

    predictions = json.load(open(pred_file))

    # A real, previously-undocumented quirk in LoCoMo's own official
    # data, found by actually running their code against it rather than
    # assumed: category-5 (adversarial) questions are keyed
    # "adversarial_answer" in locomo10.json, not "answer" — and there is
    # zero handling of that anywhere in task_eval/*.py. Their own
    # eval_question_answering does `line['answer']` unconditionally, so
    # it would KeyError on these same entries if anyone called it
    # directly too; this isn't hypothetical, it's what happened here
    # before this normalization was added. Fixed in our own glue code,
    # not by editing their file: eval_question_answering itself stays
    # byte-for-byte unmodified, this just hands it well-formed input the
    # way any real caller of their library needs to.
    for conv in predictions:
        for qa in conv["qa"]:
            if "answer" not in qa and "adversarial_answer" in qa:
                qa["answer"] = qa["adversarial_answer"]

    # eval_question_answering is LoCoMo's own real per-question scoring
    # logic -- plain F1 for single-hop/temporal/open-domain, a different
    # partial-F1 for multi-hop, substring-match-on-abstention for
    # adversarial (see that function's own category branches) -- called
    # exactly the way their own evaluate_qa.py calls it, writing the
    # score back onto each qa entry under "<model>_f1", their own
    # convention.
    for conv in predictions:
        exact_matches, _lengths, _recall = eval_question_answering(conv["qa"], PREDICTION_KEY)
        for i in range(len(conv["qa"])):
            conv["qa"][i][MODEL_KEY + "_f1"] = round(exact_matches[i], 3)

    scored_file = pred_file + ".scored.json"
    with open(scored_file, "w") as f:
        json.dump(predictions, f, indent=2)

    # analyze_aggr_acc is LoCoMo's own per-category aggregation --
    # reads ann_file for conversation lengths (encoder=None: falls back
    # to character-length, no tokenizer dependency needed) and
    # scored_file for the per-question scores just written above.
    analyze_aggr_acc(ann_file, scored_file, stats_out, MODEL_KEY, MODEL_KEY + "_f1")
    print(f"\nWrote per-category stats to {stats_out}")


if __name__ == "__main__":
    main()
