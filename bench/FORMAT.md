# Benchmark data formats — confirmed against real downloaded data

This is Step 1 of the LoCoMo/LongMemEval harness plan: lock in the
*actual* field shapes before writing adapter code against assumptions.
Everything below was checked against the real files fetched by
`bench/fetch-data.sh`, not against the README/paper descriptions alone —
in two places the real data disagreed with (or simply wasn't covered
by) the documentation, noted below.

## LoCoMo (`bench/data/locomo/data/locomo10.json`)

10 conversations. Per conversation:

```json
{
  "sample_id": "...",
  "conversation": {
    "speaker_a": "Caroline",
    "speaker_b": "Melanie",
    "session_1_date_time": "1:56 pm on 8 May, 2023",
    "session_1": [
      {"speaker": "Caroline", "dia_id": "D1:1", "text": "Hey Mel! ..."}
    ],
    "session_2_date_time": "...",
    "session_2": [...]
    // up to session_N, N varies per conversation (up to 35 per the paper)
  },
  "qa": [
    {"question": "...", "answer": "7 May 2023", "evidence": ["D1:3"], "category": 2}
  ]
}
```

**Gotchas found in the real data, not just the docs:**
- `session_N_date_time` is a free-text human string
  (`"1:56 pm on 8 May, 2023"`), not ISO 8601 — needs a custom Go time
  parse, not `time.Parse(time.RFC3339, ...)`.
- `qa[].answer` is **not always a string** — some entries have it as a
  JSON number (e.g. `"answer": 2022` for a "when" question with just a
  year). The adapter must `%v`-stringify defensively, not assume
  `string`.
- `category` is an **integer 1–5**, and the categories are not named in
  the README or paper text anywhere in this repo — the mapping only
  exists as branching logic in `task_eval/evaluation.py`
  (`eval_question_answering`), confirmed by reading it directly:
  - `1` = multi-hop (scored by splitting the answer into sub-phrases,
    partial F1 per sub-phrase — a different metric shape than the
    others)
  - `2` = single-hop
  - `3` = temporal (ground-truth answer is `;`-split and only the first
    segment is used for scoring — a real, easy-to-miss detail if
    re-implementing instead of importing their function)
  - `4` = open-domain
  - `2`/`3`/`4` all score with plain F1 via the same code path
  - `5` = adversarial — **not F1-scored at all**: correct iff the
    model's answer contains `"no information available"` or
    `"not mentioned"` (i.e., scored on whether it correctly abstains,
    not on textual similarity to any reference answer)
  - A commented-out line in the real `evaluation_stats.py`
    (`# if qa['category'] in [4, 5]:`) is a visible trace of exactly the
    kind of category-inclusion ambiguity that fueled the public Zep/Mem0
    scoring dispute — reinforces: import their scoring functions
    verbatim, don't hand-roll equivalent logic that could silently
    diverge on cases like this.

**Official scoring**: `task_eval/evaluation.py`'s `eval_question_answering(qa_list, prediction_key)`
and `task_eval/evaluation_stats.py`'s `analyze_aggr_acc(...)` — both
plain functions, importable directly. Our job is only to populate a
`<prediction_key>` (e.g. `hupi_prediction`) field on each `qa` entry with
HUPI's answer, in a copy of this same JSON shape, then call these two
functions unmodified.

## LongMemEval (`bench/data/longmemeval_s_cleaned.json`)

Not in the `xiaowu0162/LongMemEval` code repo itself — hosted separately
on HuggingFace (`xiaowu0162/longmemeval-cleaned`), fetched by
`fetch-data.sh` via direct download (~264MB for the `_s` variant). 500
question instances, confirmed identical in shape to the README's own
documentation (no discrepancies found here, unlike LoCoMo):

```json
{
  "question_id": "e47becba",
  "question_type": "single-session-user",
  "question": "...",
  "answer": "...",
  "question_date": "2023/05/30 (Tue) 23:40",
  "haystack_session_ids": ["..."],
  "haystack_dates": ["2023/05/20 (Sat) 02:21", "..."],
  "haystack_sessions": [
    [{"role": "user", "content": "..."}, {"role": "assistant", "content": "...", "has_answer": true}]
  ],
  "answer_session_ids": ["..."]
}
```

Confirmed real counts: 500 instances, all 6 documented `question_type`
values present (`single-session-user`, `single-session-assistant`,
`single-session-preference`, `temporal-reasoning`, `knowledge-update`,
`multi-session`), 30 of 500 `question_id`s end in `_abs` (abstention
questions, scored with a different judge prompt — see
`src/evaluation/evaluate_qa.py`'s `get_anscheck_prompt`).

Dates use a non-ISO format too (`2023/05/20 (Sat) 02:21`) — same custom
parsing need as LoCoMo, different format string.

**Starting with `_s` (not `_m` or `_oracle`)**: `_m` has ~500
sessions per history (10x the replay/consolidation cost for the harness
to prove out first); `_oracle` includes only the evidence sessions,
which trivially inflates recall by removing the actual search problem
this benchmark exists to test. Both remain available as later, larger
runs once the harness is proven correct on `_s`.

**Official scoring**: `src/evaluation/evaluate_qa.py <metric_model> <hyp_file> <ref_file>`,
run completely unmodified — `hyp_file` is JSONL of
`{"question_id": ..., "hypothesis": ...}`, one line per question. This
is an LLM-judge (not F1), using the exact per-`question_type` grading
prompts in `get_anscheck_prompt` — quoted in the plan file for
transparency, not re-derived.

## Still open for Step 2

- Exact Go time-parsing format strings for both date formats above.
- Whether to also fetch `longmemeval_oracle.json` early (small, useful
  as a "perfect retrieval" ceiling reference) even while deferring `_m`.
