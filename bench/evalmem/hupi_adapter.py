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
this adapter, place (or symlink) it into a real EvalMem checkout's
src/memory_eval/adapters/ directory and register it in that checkout's
registry.py the same way o_mem_adapter.py etc. are registered — see this
directory's README.md for the exact steps. That registration step, and
any real evaluation run, are still deliberately not done here — see
docs/EVALMEM_INTEGRATION_PLAN.md's own step 6/7 gate.
"""

from __future__ import annotations

import json
import subprocess
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional

from memory_eval.adapters.base import BaseMemoryAdapter  # type: ignore[import-not-found]


@dataclass(frozen=True)
class HupiAdapterConfig:
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

    def __init__(self, config: Optional[HupiAdapterConfig] = None):
        super().__init__()
        self.config = config or HupiAdapterConfig()
        self.family = self.config.family
        self.flavor = self.config.flavor

    def capabilities(self) -> Dict[str, Any]:
        out = super().capabilities()
        out.update(
            {
                "family": self.family,
                "flavor": self.flavor,
                "supports_real_native_runtime": True,
                "supports_lightweight_fallback": False,
                "native_runtime_status": "real_gateway_and_consolidation",
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

    def ingest_conversation(self, sample_id: str, conversation: List[Dict[str, Any]]) -> Dict[str, Any]:
        args = [self._bin("hupi-ingest-turns"), "-sample-id", sample_id]
        args += self._model_args()
        if self.config.consolidate_bin:
            args += ["-consolidate-bin", self.config.consolidate_bin]
        if self.config.skip_consolidate:
            args += ["-skip-consolidate"]
        run_ctx = self._run_json(args, stdin_payload=conversation)
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
        # Same simple token-overlap matching generic_text_adapter.py
        # uses. This method's real job is "which exported records
        # plausibly support F_key for the Encoding Examiner," not a
        # second retrieval system -- HUPI's own real retrieval is
        # exercised separately, by retrieve_original below.
        from memory_eval.eval_core.utils import split_tokens, text_match  # type: ignore[import-not-found]

        signals = [query] + list(f_key or [])
        signal_tokens: set = set()
        for signal in signals:
            signal_tokens.update(split_tokens(str(signal)))
        scored: List[tuple] = []
        for record in memory_corpus:
            text = str(record.get("text", ""))
            if any(text_match(fact, text) for fact in f_key if str(fact).strip()):
                scored.append((10.0, record))
                continue
            tokens = set(split_tokens(text))
            overlap = len(tokens & signal_tokens) if tokens and signal_tokens else 0
            if overlap > 0:
                scored.append((float(overlap), record))
        scored.sort(key=lambda item: item[0], reverse=True)
        return [record for _, record in scored[:100]]

    def retrieve_original(self, run_ctx: Any, query: str, top_k: int = 5) -> List[Dict[str, Any]]:
        result = self._answer(run_ctx, query)
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
        result = self._answer(run_ctx, query)
        return result.get("answer") or "I don't know"

    def generate_oracle_answer(self, run_ctx: Any, query: str, oracle_context: str) -> str:
        result = self._answer(run_ctx, query, oracle_context=oracle_context)
        return result.get("answer") or "I don't know"

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


__all__ = ["HupiAdapterConfig", "HupiMemoryAdapter"]
