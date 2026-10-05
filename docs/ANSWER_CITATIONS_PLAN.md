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

- **Phase 1 (done)**: retrieval-level citations — expose `Refs`
  plus a human-readable snippet per reference, opt-in via a header.
  Honestly labeled as "memory available for this answer," not causal
  "why." Reuses data that's already computed at retrieval time; no new
  LLM cost.
- **Phase 2 (done)**: generation-level attribution — verify which
  specific facts the *answer* actually relies on, via a
  grounding-check-style LLM call
  (`internal/consolidation/grounding.go`'s pattern: numbered facts +
  reference text + a single JSON verdict). One extra real LLM call per
  explained response, only paid when a caller explicitly asks for it
  (`X-Hupi-Explain: deep`, distinct from Phase 1's free `on`).

**Since both phases shipped**: `Citation` has grown beyond the shape
shown below — fact-granularity citations (`RefKindMemory`, not just
summary/entity/episode-level), `IsInference`, `Relations`, and
`ParentSummaryID` were added by `docs/MEMORY_MODEL_REARCHITECTURE_PLAN.md`
(Phases 0, 3, and 4) once the underlying `memories`/`memory_relations`
tables existed to back them. See that doc for the current, complete
field list and how each one gets populated — this document is kept as
the original Phase 1/2 design record, not updated in place.

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

## Design — Phase 2

**`internal/gateway/attribution.go`** (new file): `attributionSystemPrompt`
+ `buildAttributionPrompt` + `attributionCheck(ctx, judge, answer,
citations) ([]bool, error)` — the same numbered-items-plus-single-JSON-
verdict shape as `groundingCheck`, swapping "is this fact grounded in
source text" for "does this answer rely on this snippet." `judge` is
`target`, the same `provider.Provider` that generated the answer — no
new provider role/config, since this only ever runs when a caller
explicitly opts in. Same safe-degrade direction as `groundingCheck`: an
unparsable or wrong-length verdict defaults every citation to "not
used" rather than failing the request.

`extractJSON` (the balanced-`{...}`-extraction helper `groundingCheck`
already relies on) is duplicated into `attribution.go` rather than
exported from `internal/consolidation` — small, pure, dependency-free,
matching this repo's existing precedent for not forcing an awkward
cross-package dependency over one helper function.

**`Citation` gains `Used *bool`** (`json:"used,omitempty"`) — nil unless
Phase 2 ran, so a caller can't confuse "not checked" with "checked and
found unused."

**Two explain levels, not one bool**: `X-Hupi-Explain: on` stays Phase-1-
only (free); `X-Hupi-Explain: deep` additionally runs `attributionCheck`
after the real answer comes back and sets `Used` on each citation. A
caller who only wants "what was available" shouldn't pay for "what was
actually used" — cost-control is the reason two levels exist, not just
API surface tidiness.

## Sequenced steps

1. ✅ Add `Citation` type + `RetrievalResult.Citations` field.
2. ✅ Thread `citations *[]gateway.Citation` through every ref-producing
   function in `internal/store/retrieve.go`; change `appendKeyFacts` to
   also return the fact list.
3. ✅ Wire `X-Hupi-Explain` header + `hupi_citations` response field in
   `internal/gateway`.
4. ✅ Tests: unit tests on the citation-snippet logic (summary
   most-relevant-fact reuse in particular), plus an end-to-end
   `handler_test.go`-style test asserting citations appear when the
   header is set and are omitted when it isn't.
5. ✅ `attributionCheck` + `X-Hupi-Explain: deep` (Phase 2). Tests:
   `attribution_test.go` (parsing, markdown-fence tolerance, mismatched-
   count and unparsable-response safe-degrade, zero-citations no-op —
   asserting no LLM call happens for an empty citation list) plus two
   end-to-end `handler_test.go` tests confirming `deep` makes exactly
   one extra upstream call and populates `Used`, while plain `on` makes
   none and leaves `Used` nil.

## Non-goals for this pass

- **Streaming response citations** — no mechanism designed yet.
- **Per-attribute entity ranking** — entities cite their whole
  attribute blob, not a single most-relevant attribute.
- **Persisted "explain a past answer" endpoint** (`Refs` is already
  persisted per episode, so this is plausible later, but out of scope
  now — this pass is request-time only).
- **A dedicated "judge" provider role for attribution** — reuses
  whatever model answered; a cheaper dedicated judge model (mirroring
  consolidation's separate `grounding` provider) is a real option if
  attribution quality or cost ever needs tuning independently of the
  answer model, not built now.

## Verification

- Unit tests for the new citation-building logic (no real DB needed for
  the pure parts, e.g. `mostRelevantFactIndex` reuse) — passing.
- Unit tests for `attributionCheck`'s parsing/safe-degrade behavior
  against a fake `provider.Provider` — passing.
- Real end-to-end tests against `internal/gateway`'s existing
  `fakeRetriever`/`postChatCompletion` test harness (`handler_test.go`):
  citations present/shaped correctly with `X-Hupi-Explain: on`, absent
  without it, `Used` populated with `deep` (asserting exactly 2 upstream
  calls) and left nil with plain `on` (asserting exactly 1) — passing.
- `go build ./...`, `go vet ./...`, and the full repo test suite
  (`go test ./...`, real Postgres via `HUPI_TEST_DATABASE_URL`) all
  clean — no regressions in the pre-existing retrieval/gateway tests.
