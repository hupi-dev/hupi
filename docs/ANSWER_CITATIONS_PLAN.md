# Plan: answer citations ("why was this answer given")

## Context

Asked directly: can HUPI show which memories fed a given answer, given
consolidation, grounding, and retrieval-time prompt injection already
exist. Real design tension, worked through before writing this doc:
`Refs` (what retrieval injected into context) tells you what was
*available* to the model, not what it actually *used* — a true "why"
needs generation-level attribution, which is more expensive (an extra
LLM call, the same shape as consolidation's existing grounding check).

Decision: two phases, not one.

- **Phase 1 (this pass)**: retrieval-level citations — expose `Refs`
  plus a human-readable snippet per reference, opt-in via a header.
  Honestly labeled as "memory available for this answer," not causal
  "why." Reuses data that's already computed at retrieval time; no new
  LLM cost.
- **Phase 2 (not started, flagged for later)**: generation-level
  attribution — verify which specific facts the *answer* actually
  relies on, via a grounding-check-style LLM call
  (`internal/consolidation/grounding.go`'s pattern: numbered facts +
  reference text + a single JSON verdict). Real, currently-known cost:
  one extra LLM call per explained response, only paid when a caller
  actually asks for it.

## Design — Phase 1

**New type**, `internal/gateway/handler.go` (alongside `RetrievalResult`):

```go
type Citation struct {
    Ref     identity.Ref `json:"ref"`
    Snippet string       `json:"snippet"`
}
```

`RetrievalResult` gains `Citations []Citation` — purely additive, no
interface signature change (`Retriever.Retrieve`'s signature is
unchanged), so no existing caller/test double breaks.

**`internal/store/retrieve.go`**: every ref-producing helper
(`fusedSearchSummaries`, `vectorSearchEntities`, `vectorSearchEpisodes`,
`graphWalkRelationships`, `keywordSearchEpisodes`,
`keywordSearchEntities`, `buildAnchor`, plus `retrieve()`'s own inline
stage-1 entity-match loop) gains one more shared out-parameter,
`citations *[]gateway.Citation`, matching the existing `sb
*strings.Builder` / `strongHit *bool` pointer-out-param convention
already used throughout this file — not a return-type change, so the
blast radius is one new parameter per function, not a signature
rewrite. At every existing `refs = append(refs, identity.Ref{...})`,
also append a `gateway.Citation{Ref: ..., Snippet: ...}` using the exact
same text already being written to `sb` for that reference — no new
text formatting, just capturing it a second time into a citation.

**Summaries get the good case for free**: `appendKeyFacts` already picks
the single most query-relevant fact via `mostRelevantFactIndex` and
labels it "(most relevant)" in the context text (`8a257ca`, already
merged). Changing `appendKeyFacts`'s signature to also return
`(facts []string, err error)` lets the call site reuse that exact same
ranking for the citation snippet — the summary's prose plus its most
relevant fact, not the whole raw fact dump.

**Entities/episodes get the plain case**: no fact-level ranking exists
for entity attributes (a whole entity's attributes are one blob) or
episodes (a single raw exchange) — the citation snippet is just the same
formatted text already injected into context. A per-attribute "most
relevant" ranking for entities (mirroring `mostRelevantFactIndex`) is a
real, identified future improvement, not built in this pass.

**`internal/gateway`**: new request header `X-Hupi-Explain: on`
(matching `X-Hupi-Memory`/`X-Hupi-Capture`'s existing convention).
`chatCompletionResponse` (types.go) gains `Citations []Citation
\`json:"hupi_citations,omitempty"\`` — an additive top-level field,
invisible to strict OpenAI clients that don't look for it.
`handleNonStream` populates it from `result.Citations` only when the
header is set; otherwise it stays nil/omitted. Streaming
(`handleStream`) is explicitly out of scope for this pass — no
per-chunk citation mechanism exists yet, flagged as a real gap, not
silently ignored.

## Sequenced steps

1. Add `Citation` type + `RetrievalResult.Citations` field.
2. Thread `citations *[]gateway.Citation` through every ref-producing
   function in `internal/store/retrieve.go`; change `appendKeyFacts` to
   also return the fact list.
3. Wire `X-Hupi-Explain` header + `hupi_citations` response field in
   `internal/gateway`.
4. Tests: unit tests on the citation-snippet logic (summary
   most-relevant-fact reuse in particular), plus an end-to-end
   `handler_test.go`-style test asserting citations appear when the
   header is set and are omitted when it isn't.

## Non-goals for this pass

- **Phase 2** (generation-level attribution, verifying the answer
  actually relies on a cited fact) — real, flagged, not started.
- **Streaming response citations** — no mechanism designed yet.
- **Per-attribute entity ranking** — entities cite their whole
  attribute blob, not a single most-relevant attribute.
- **Persisted "explain a past answer" endpoint** (`Refs` is already
  persisted per episode, so this is plausible later, but out of scope
  now — this pass is request-time only).

## Verification

- Unit tests for the new citation-building logic (no real DB needed for
  the pure parts, e.g. `mostRelevantFactIndex` reuse).
- A real end-to-end test against `internal/gateway`'s existing
  `fakeRetriever`/`postChatCompletion` test harness
  (`handler_test.go`), matching `TestHandleChatCompletions_OnRetrieveSeesExactResult`'s
  pattern: assert `hupi_citations` is present and correctly shaped with
  the header set, absent without it.
- `go build ./...` and `go vet ./...` clean.
