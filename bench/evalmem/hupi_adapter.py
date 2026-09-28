"""HUPI adapter for EvalMem (github.com/ZeyuuLiu/EvalMem) — real
docs/EVALMEM_INTEGRATION_PLAN.md step 4.

Implements EvalMem's own BaseMemoryAdapter contract by shelling out to
three small, real Go tools in this repo — no HUPI logic is
reimplemented in Python, the same "no reimplemented metric/scoring
logic" discipline this whole benchmark effort has followed throughout
(docs/BENCHMARKS.md):

  - cmd/hupi-ingest-turns   -> ingest_conversation
  - cmd/hupi-export-memory  -> export_full_memory
  - cmd/hupi-answer-question -> retrieve_original / generate_online_answer
                                 / generate_oracle_answer

This file lives in this repo (not EvalMem's own), the same way
bench/score_locomo.py lives here and imports LoCoMo's own scoring code
from a separately-fetched clone at runtime — EvalMem is a separate,
external dependency, not something we vendor or fork. To actually use
this adapter against a real EvalMem checkout, no edit to that checkout's
own registry.py is required: EvalMem's scripts/run_eval_pipeline.py can
load an adapter directly via --adapter-module/--adapter-class. Use the
module-name form (--adapter-module hupi_adapter, with this directory on
PYTHONPATH), not the raw .py file-path form — the latter hits a real bug
in EvalMem 0.6.6's own loader for any dataclass-using module (see this
directory's README.md for the exact invocation and why). Any real
evaluation run is still deliberately not done here — see
docs/EVALMEM_INTEGRATION_PLAN.md's own step 6/7 gate.
"""

from __future__ import annotations

import json
import math
import subprocess
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional

from memory_eval.adapters.base import BaseMemoryAdapter  # type: ignore[import-not-found]
from memory_eval.eval_core.models import AdapterTrace, RetrievedItem  # type: ignore[import-not-found]


def _bm25_scores(query_tokens: List[str], docs_tokens: List[List[str]], k1: float = 1.5, b: float = 0.75) -> List[float]:
    """Minimal, dependency-free Okapi BM25 over an in-memory corpus.

    rank_bm25 isn't an existing dependency of this adapter or of EvalMem
    itself (its own agentic-rag extra pulls in a much heavier stack —
    langchain, sentence-transformers — deliberately not adopted here, see
    this module's docstring), and this corpus (one conversation's
    exported memory) is small enough that a compact, self-contained
    implementation is simpler and cheaper than adding a new dependency
    for it. Real term-frequency saturation and document-length
    normalization, unlike find_memory_records' previous plain
    token-overlap count.
    """
    n_docs = len(docs_tokens)
    if n_docs == 0:
        return []
    doc_lens = [len(d) for d in docs_tokens]
    avgdl = sum(doc_lens) / n_docs

    doc_freq: Dict[str, int] = {}
    for doc in docs_tokens:
        for term in set(doc):
            doc_freq[term] = doc_freq.get(term, 0) + 1
    idf = {term: math.log(1 + (n_docs - freq + 0.5) / (freq + 0.5)) for term, freq in doc_freq.items()}

    scores: List[float] = []
    for doc, dl in zip(docs_tokens, doc_lens):
        term_freq: Dict[str, int] = {}
        for term in doc:
            term_freq[term] = term_freq.get(term, 0) + 1
        length_norm = 1 - b + b * (dl / avgdl if avgdl else 1.0)
        score = 0.0
        for term in query_tokens:
            f = term_freq.get(term, 0)
            if f == 0:
                continue
            score += idf.get(term, 0.0) * (f * (k1 + 1)) / (f + k1 * length_norm)
        scores.append(score)
    return scores


@dataclass(frozen=True)
class HupiMemoryAdapterConfig:
    family: str = "hupi"
    flavor: str = "hupi_native"
    # Directory containing the three built Go binaries below — build
    # them with `go build -o <bin_dir>/<name> ./cmd/<name>` from this
    # repo first; this adapter never builds them itself.
    bin_dir: str = "."
    answer_model: str = ""  # providers.yaml profile name; "" = active_chat_provider
    consolidate_bin: str = "./hupi-consolidate"
    skip_consolidate: bool = False  # smoke tests only -- a real evaluation needs consolidation to have actually run


class HupiMemoryAdapter(BaseMemoryAdapter):
    """Real HUPI gateway, real nightly consolidation, real retrieval —
    driven through the three Go CLI tools above, exactly the same code
    path a real HUPI deployment uses. Every request also sets
    X-Hupi-Capture: off (see cmd/hupi-answer-question's own doc comment)
    so repeated diagnostic calls against the same run_ctx never
    accumulate as real episodes -- the exact episode-pollution failure
    mode found and documented in docs/BENCHMARKS.md §6.
    """

    family = "hupi"
    flavor = "hupi_native"

    def __init__(self, config: Optional[HupiMemoryAdapterConfig] = None):
        super().__init__()
        self.config = config or HupiMemoryAdapterConfig()
        self.family = self.config.family
        self.flavor = self.config.flavor
        # retrieve_original and generate_online_answer are called
        # independently by EvalMem's Retrieval and Generation probes
        # (which run concurrently -- ParallelThreeProbeEvaluator's own
        # thread pool) for the exact same real native-mode call. Without
        # this cache each one separately shells out to
        # hupi-answer-question, silently doubling real retrieval+
        # generation cost per question. Keyed by (scope, query), not
        # top_k -- HUPI's native answer/context don't depend on top_k
        # (see retrieve_original's own doc comment).
        self._native_cache: Dict[str, Dict[str, Any]] = {}
        self._native_cache_lock = threading.Lock()

    def capabilities(self) -> Dict[str, Any]:
        out = super().capabilities()
        out.update(
            {
                "family": self.family,
                "flavor": self.flavor,
                "supports_real_native_runtime": True,
                "supports_lightweight_fallback": False,
                "native_runtime_status": "real_gateway_and_consolidation",
                # We don't implement hybrid_retrieve_candidates -- the
                # base class default of True here would misrepresent
                # what this adapter actually does (find_memory_records'
                # plain token-overlap match is the only candidate path).
                "supports_high_recall_candidates": False,
            }
        )
        return out

    def _bin(self, name: str) -> str:
        return str(Path(self.config.bin_dir) / name)

    def _run_json(self, args: List[str], stdin_payload: Optional[Any] = None) -> Dict[str, Any]:
        proc = subprocess.run(
            args,
            input=json.dumps(stdin_payload).encode("utf-8") if stdin_payload is not None else None,
            capture_output=True,
        )
        if proc.returncode != 0:
            raise RuntimeError(
                f"{args[0]} exited {proc.returncode}: {proc.stderr.decode('utf-8', errors='replace')}"
            )
        return json.loads(proc.stdout.decode("utf-8"))

    def _model_args(self) -> List[str]:
        return ["-answer-model", self.config.answer_model] if self.config.answer_model else []

    @staticmethod
    def _to_hupi_turns(conversation: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
        """Translate a turn list into cmd/hupi-ingest-turns's expected
        shape (dia_id, session_index, session_datetime per turn).

        The real end-to-end pipeline (memory_eval.pipeline.runner's
        _conversation_to_turns, verified against actual source, not
        assumed) hands adapters a simpler shape than EvalMem's own
        LoCoMo sample builder uses internally: flat
        {turn_index, speaker, text, timestamp} dicts, where timestamp is
        that turn's *session's* raw LoCoMo date string, repeated for
        every turn in the same session, with no explicit session index.
        A new session is exactly where that timestamp value changes --
        _conversation_to_turns already emits turns in session order, so
        this is a safe, order-preserving re-derivation, not a guess.

        Tolerates turns that already carry session_index/session_datetime
        (e.g. EvalMem's own locomo_builder-shaped flattening) by passing
        those straight through instead of re-deriving them.
        """
        out: List[Dict[str, Any]] = []
        session_index = -1
        last_ts: Any = object()  # sentinel, never equals a real timestamp string
        for turn in conversation:
            if "session_index" in turn:
                out.append(dict(turn))
                continue
            ts = str(turn.get("timestamp") or turn.get("session_datetime") or "")
            if ts != last_ts:
                session_index += 1
                last_ts = ts
            turn_index = int(turn.get("turn_index", len(out)))
            out.append(
                {
                    "dia_id": turn.get("dia_id") or f"D{session_index}:{turn_index}",
                    "speaker": turn.get("speaker", ""),
                    "text": turn.get("text", ""),
                    "turn_index": turn_index,
                    "session_index": session_index,
                    "session_datetime": ts,
                }
            )
        return out

    def ingest_conversation(self, sample_id: str, conversation: List[Dict[str, Any]]) -> Dict[str, Any]:
        args = [self._bin("hupi-ingest-turns"), "-sample-id", sample_id]
        args += self._model_args()
        if self.config.consolidate_bin:
            args += ["-consolidate-bin", self.config.consolidate_bin]
        if self.config.skip_consolidate:
            args += ["-skip-consolidate"]
        run_ctx = self._run_json(args, stdin_payload=self._to_hupi_turns(conversation))
        run_ctx["sample_id"] = sample_id
        return run_ctx

    def export_full_memory(self, run_ctx: Any) -> List[Dict[str, Any]]:
        export = self._run_json(
            [
                self._bin("hupi-export-memory"),
                "-scope-kind",
                run_ctx.get("scope_kind", "private"),
                "-scope-owner",
                run_ctx["scope_owner"],
            ]
        )
        # Flatten HUPI's own {entities, summaries, relationships} export
        # into EvalMem's generic "list of {id, text, meta}" record shape
        # (the same shape generic_text_adapter.py's own export uses) so
        # find_memory_records/the Encoding Examiner can score over one
        # uniform corpus regardless of which HUPI table a fact lives in.
        records: List[Dict[str, Any]] = []
        for e in export.get("entities", []):
            records.append(
                {
                    "id": f"entity:{e['id']}",
                    "text": f"{e['name']} ({e['kind']}): {e['attributes']}",
                    "meta": {"source": "hupi_entity", "kind": e["kind"]},
                }
            )
        for s in export.get("summaries", []):
            records.append(
                {
                    "id": f"summary:{s['id']}",
                    "text": s["text"],
                    "meta": {"source": "hupi_summary", "period": s["period"], "level": s["level"]},
                }
            )
            for i, kf in enumerate(s.get("key_facts", [])):
                records.append(
                    {
                        "id": f"summary:{s['id']}:key_fact:{i}",
                        "text": kf["fact"],
                        "meta": {
                            "source": "hupi_key_fact",
                            "grounded": kf["grounded"],
                            "summary_id": s["id"],
                        },
                    }
                )
        for r in export.get("relationships", []):
            validity = ""
            if r.get("valid_from"):
                validity += f" (from {r['valid_from']})"
            if r.get("valid_until"):
                validity += f" (until {r['valid_until']})"
            records.append(
                {
                    "id": f"relationship:{r['id']}",
                    "text": f"{r['subject_id']} {r['predicate']} {r['object_id']}{validity}",
                    "meta": {
                        "source": "hupi_relationship",
                        "subject_id": r["subject_id"],
                        "object_id": r["object_id"],
                    },
                }
            )
        return records

    def find_memory_records(
        self,
        run_ctx: Any,
        query: str,
        f_key: List[str],
        memory_corpus: List[Dict[str, Any]],
    ) -> List[Dict[str, Any]]:
        # Ranks with BM25 instead of generic_text_adapter.py's plain
        # token-overlap count -- overlap count alone scores a short,
        # paraphrased summary lower than a long, mostly-irrelevant one
        # that happens to repeat a few query words, and gives no credit
        # for how rare/distinctive a matched term is. This method's real
        # job is "which exported records plausibly support F_key for the
        # Encoding Examiner," not a second retrieval system -- HUPI's own
        # real retrieval is exercised separately, by retrieve_original
        # below. An exact f_key phrase match still wins outright when it
        # happens (a real signal worth keeping), BM25 only ranks the rest.
        from memory_eval.eval_core.utils import split_tokens, text_match  # type: ignore[import-not-found]

        signals = [query] + list(f_key or [])
        signal_tokens: List[str] = []
        for signal in signals:
            signal_tokens.extend(split_tokens(str(signal)))
        if not signal_tokens:
            return []

        exact: List[Dict[str, Any]] = []
        rest: List[Dict[str, Any]] = []
        rest_tokens: List[List[str]] = []
        for record in memory_corpus:
            text = str(record.get("text", ""))
            if any(text_match(fact, text) for fact in f_key if str(fact).strip()):
                exact.append(record)
                continue
            rest.append(record)
            rest_tokens.append(list(split_tokens(text)))

        scores = _bm25_scores(signal_tokens, rest_tokens)
        ranked = sorted(zip(scores, rest), key=lambda item: item[0], reverse=True)
        matched_rest = [record for score, record in ranked if score > 0]
        return (exact + matched_rest)[:100]

    def _native_answer(self, run_ctx: Any, query: str) -> Dict[str, Any]:
        """The one real native-mode hupi-answer-question call for a given
        (scope, query), shared by retrieve_original and
        generate_online_answer -- see this adapter's own __init__ doc
        comment for why this cache exists. The lock is held across the
        real subprocess call (not just the dict access) so that if both
        probes race for the same query, the loser actually waits for the
        winner's result instead of also shelling out."""
        key = f"{run_ctx.get('scope_owner', '')}:{query}"
        with self._native_cache_lock:
            cached = self._native_cache.get(key)
            if cached is not None:
                return cached
            result = self._answer(run_ctx, query)
            self._native_cache[key] = result
            return result

    def retrieve_original(self, run_ctx: Any, query: str, top_k: int = 5) -> List[Dict[str, Any]]:
        result = self._native_answer(run_ctx, query)
        context = result.get("retrieved_context") or ""
        if not context:
            return []
        # HUPI's real ContextMessage is one assembled prose block, not a
        # ranked list of discrete chunks the way most systems this
        # framework was built against return -- one top-ranked record is
        # the honest representation, not a fabricated split into chunks
        # HUPI's own retrieval never actually produced.
        return [{"id": "hupi_context_message", "text": context, "score": 1.0, "meta": {"source": "hupi_retrieve"}}][
            : max(1, top_k)
        ]

    def generate_online_answer(self, run_ctx: Any, query: str, top_k: int = 5) -> str:
        result = self._native_answer(run_ctx, query)
        return result.get("answer") or "I don't know"

    def generate_oracle_answer(self, run_ctx: Any, query: str, oracle_context: str) -> str:
        result = self._answer(run_ctx, query, oracle_context=oracle_context)
        return result.get("answer") or "I don't know"

    def build_trace_for_query(self, run_ctx: Any, query: str, oracle_context: str, top_k: int) -> AdapterTrace:
        # Not called by EvalMem's own primary pipeline
        # (memory_eval.pipeline.runner.ThreeProbeEvaluationPipeline uses
        # the granular methods above directly, verified against its
        # actual FullEvalAdapterProtocol) -- implemented anyway since
        # every other adapter in this repo implements it, for the same
        # alternate EvalAdapterProtocol callers that might use it.
        memory_view = self.export_full_memory(run_ctx)
        raw_items = self.retrieve_original(run_ctx, query, top_k=top_k)
        retrieved = [
            RetrievedItem(
                id=str(item.get("id", "")),
                text=str(item.get("text", "")),
                score=float(item.get("score", 0.0) or 0.0),
                meta=dict(item.get("meta", {})) if isinstance(item.get("meta", {}), dict) else {},
            )
            for item in raw_items
        ]
        return AdapterTrace(
            memory_view=memory_view,
            retrieved_items=retrieved,
            answer_online=self.generate_online_answer(run_ctx, query, top_k=top_k),
            answer_oracle=self.generate_oracle_answer(run_ctx, query, oracle_context),
            raw_trace={"memory_system": self.family, "mode": self.flavor},
        )

    def _answer(self, run_ctx: Any, query: str, oracle_context: Optional[str] = None) -> Dict[str, Any]:
        args = [
            self._bin("hupi-answer-question"),
            "-scope-kind",
            run_ctx.get("scope_kind", "private"),
            "-scope-owner",
            run_ctx["scope_owner"],
            "-question",
            query,
        ]
        args += self._model_args()
        if oracle_context is not None:
            args += ["-oracle", "-oracle-context", oracle_context]
        return self._run_json(args)


__all__ = ["HupiMemoryAdapterConfig", "HupiMemoryAdapter"]
