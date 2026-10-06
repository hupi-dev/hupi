# Master test plan — every story, what covers it, how to run the gate

This document maps HUPI's real product stories (as told in
[BUSINESS_PROCESS.md](BUSINESS_PROCESS.md)'s scenarios and
[MEMORY_SCENARIOS.md](MEMORY_SCENARIOS.md)'s question-type-by-question-type
walkthrough) to the actual, already-existing automated tests that cover
them — 507 `Test*` functions across 28 packages as of this writing, all
real-Postgres integration tests where the story needs real data, not
mocked-everything unit tests pretending to be coverage.

**Scope, deliberately**: this aggregates and gates the existing Go test
suite — it does not add new end-to-end story tests of its own. Where a
story below has no real test backing it, that's stated plainly as a gap,
not papered over with an inferred "probably fine."

**The gate itself** is [`master-test.sh`](../master-test.sh) — gofmt,
`go build ./...`, `go vet ./...`, and the full `go test ./...` against a
real Postgres instance, in that order, any failure stops the gate. It's
the same steps `.github/workflows/ci.yml`'s `go` job runs, factored out
so they're identical in CI and on your own machine:

```bash
./master-test.sh
# or, pointing at non-default connection details:
HUPI_ADMIN_DATABASE_URL=postgres://postgres:hupi@localhost:5432/hupi?sslmode=disable \
HUPI_TEST_DATABASE_URL=postgres://hupi_app:hupi_app_test_only@localhost:5432/hupi?sslmode=disable \
./master-test.sh
```

**What this gate does NOT verify**: real LLM output quality (that's
`cmd/hupi-bench`'s job — see [BENCHMARKS.md](BENCHMARKS.md)), or
anything requiring a live provider API key (grounding/contradiction/
extends/inference prompts are verified at the Go-plumbing level with a
fake provider here; their actual real-GPT-4.1 behavior is verified
separately and recorded in
[MEMORY_MODEL_REARCHITECTURE_PLAN.md](MEMORY_MODEL_REARCHITECTURE_PLAN.md),
not re-run on every gate pass since that would mean a paid API call on
every CI run).

---

## 1. Core product stories ([BUSINESS_PROCESS.md](BUSINESS_PROCESS.md) §5-10)

### "A conversation turn happens, memory captures it"
`internal/store`'s capture suite: `TestCapture_WritesAuditLogEntry`,
`TestCapture_AuditActorIsIndividualNotWorkspace`,
`TestHandleChatCompletions_CapturesByDefault`,
`TestHandleChatCompletions_CaptureOptOut`,
`TestHandleChatCompletions_MemoryOptOutSkipsRetrievalButStillCaptures`,
`TestHandleChatCompletions_DocumentAttachmentReachesCapturedEpisode`.

### "The AI misremembered something" (§8)
The full feedback → trace → correct → re-verify loop:
`TestHandleFeedback_ReturnsBadRequestWhenEpisodeNotInScope`,
`TestCapture_RejectsFeedbackReferringToEpisodeInAnotherScope`,
`TestCapture_AcceptsFeedbackReferringToOwnEpisode`,
`TestTrace_ResolvesRefKindMemoryAttributeAndKeyFact`
(`internal/store/trace_test.go`), the full `hupi-correct` round trip —
`TestCurrentContent_DumpTemplateThenCorrectPreservesUntouchedAttributes`,
`TestCorrect_ReplacesEntityAttributesWholesale`,
`TestCorrect_RejectsAlreadySupersededTarget`,
`TestCorrect_ConcurrentCorrectionsOfSameTargetDoNotFork`,
`TestCorrect_WritesAuditLogWithGivenActor`.
The *automatic* half of this same story (§8 step 6 — contradiction
detection without a human filing feedback first):
`TestCheckCrossPeriodContradictions_AppliesCorrection`,
`TestFindRelatedSummaries_ExcludesSupersededIncludesCorrection`.

### "Standing up a shared team" (§6) / scope isolation generally
`TestScopeIsolation_Entities`, `TestScopeIsolation_Episodes`,
`TestSelfModelAnchorsToActingUser`, and the full RLS enforcement suite:
`TestRLS_DeniesCompletelyUnscopedRead`,
`TestRLS_RejectsWriteOutsideWorkspace`,
`TestRLS_TwoScopePolicyIsExactNotBroad`,
`TestRLS_SummaryKeyFacts_DeniesCompletelyUnscopedRead`,
`TestRLS_SummaryKeyFacts_DeniesCrossScopeRead`,
`TestRLS_SummaryKeyFacts_RejectsWriteOutsideWorkspace`. These *must* run
connected as the restricted `hupi_app` role, never the Postgres
superuser — see `master-test.sh`'s own setup step; connecting as
superuser bypasses RLS and makes every one of these pass for the wrong
reason (or fail to catch a real regression at all).

### "A security incident" (§7) — audit trail, encryption, key rotation
`internal/crypto` (field encryption round-trip), the full
`internal/rotate` suite (`TestRotate_FullLifecycle`,
`TestRotate_ResumesAfterInterruption`,
`TestRotate_ConcurrentStartsAgreeOnOneRotation`,
`TestRotate_ConcurrentWriteLandsOnNewVersion`,
`TestRotate_PruneRefusesBeforeCompletion`,
`TestRotate_PruneDeletesOldVersionOnceCompleted`), `internal/audit`.

### "Changing which AI you use" (§9)
`internal/provider` (27 tests — both adapters' wire-format handling,
independent of which vendor is configured) plus
`TestRunRollup_ThreadsKnownEntitiesIntoPrompt` and the consolidation
suite's own provider-agnostic design (never hardcodes a vendor).

### "Running it day to day" (§10) — backup/restore, re-embedding, ops
The full `internal/hpmf` export/import suite (12 tests, including
the two newest: `TestExportImportRoundTrip_PreservesKeyFactFields`,
`TestExportImportRoundTrip_PreservesMemoryRelations`), `internal/reembed`,
`internal/backfillmemories` (5 tests), `cmd/hupi-dashboard` (12 tests —
the ops-visibility surface itself).

### Tier 3 / shared-team specifics
`TestEntityRelationships_AllowsValidDateOrderingsAndUnknownEnds`,
`TestEntityRelationships_RejectsValidUntilBeforeValidFrom`, the demo
hosted-session suite (`internal/demo`, 14 tests — guest provisioning,
rate limits, sweep) as the closest real stand-in for "anonymous
multi-tenant traffic" this OSS build exercises end-to-end.

---

## 2. Memory behavior scenarios ([MEMORY_SCENARIOS.md](MEMORY_SCENARIOS.md) A-N)

| Scenario | Representative test(s) |
|---|---|
| A. Single-fact recall, quiet day | `internal/consolidation`'s `TestGenerateDailySummary_*`, `internal/store`'s basic retrieve tests |
| B/C. Busy-day dilution, clustering | `TestClusterSourcesKeepsHighlySimilarSourcesTogether`, `TestClusterSourcesSeparatesDistinctTopics`, `TestClusterSourcesRespectsMaxClustersPerDay`, `TestMaxClustersPerDayDefaultAndOverride` |
| D. Cross-summary budget starvation | the guarantee-line write path tests in `internal/store` (search `guarantee`) |
| E/F. Aggregation reasoning | `internal/gateway/aggregation_test.go` (pre-existing gofmt staleness noted in `master-test.sh`, not a coverage gap) |
| G. Contradicting facts across time | `TestCheckCrossPeriodContradictions_AppliesCorrection`, `TestFindRelatedSummaries_PrefersSpecificSharedEntityOverHub`, `TestFindRelatedSummaries_ExcludesSupersededIncludesCorrection` |
| H. Temporal relevance | `TestFusedSearchSummaries_TemporalBoostMagnitudeAffectsSelectionOrder`, the full `temporal_retrieve_test.go` suite (6 tests) |
| I. Cross-session preference/recommendation | `internal/store`'s recommendation-shaped retrieve tests |
| J. Guarantee lottery | same guarantee-line tests as D |
| K. Multi-hop questions | `internal/store`'s graph-walk tests (`HUPI_ENABLE_RELATIONSHIP_GRAPH_WALK`) |
| L. Human-driven correction | same `hupi-correct` suite as §8 above |
| M. Correct abstention | covered at the benchmark/QA-prompt level (`internal/qaprompt`, 12 tests), not a unit test shape |
| N. Fact relations and inference (`updates`/`extends`/`derives`) | `TestCheckCrossPeriodContradictions_AppliesCorrection` (`updates`), `TestCheckExtends_ParsesResponse`, `TestCheckOneRelatedSummaryForExtends_RecordsRelation`, `TestCheckCrossPeriodContradictions_ExtendsGatedByFlag` (`extends`), `TestExtractInferences_ParsesResponse`, `TestStoreSummary_WritesInferenceAndDerivesRelation` (`derives`), `TestRetrieve_CitationMarksInferredFact`, `TestRetrieve_CitationSurfacesUpdatesRelation` (citation surfacing), `TestCurrentContent_DumpTemplateThenCorrectPreservesInferredFact` (the real bug fixed in this effort) |

---

## 3. Known gaps — honestly, not papered over

- **No automated test replays a real benchmark end to end as part of this
  gate.** `cmd/hupi-bench` exists and is itself tested (9 tests covering
  its own plumbing — resume, baseline mode, prediction-file shape), but
  actually running it against LoCoMo/LongMemEval needs a live LLM key and
  real cost, so it's a separate, manually-triggered process
  ([BENCHMARKS.md](BENCHMARKS.md)), not part of this pass/fail gate.
- **`extends`/`derives`/inference-extraction prompts are only verified at
  the Go-plumbing level here** (fake provider, fixed canned responses) —
  their real behavior against GPT-4.1 is recorded as a point-in-time,
  manually-run verification in
  [MEMORY_MODEL_REARCHITECTURE_PLAN.md](MEMORY_MODEL_REARCHITECTURE_PLAN.md),
  not re-verified on every gate run.
- **No E2E test exercises the real `hupi` gateway binary as a running
  server process** (every test calls `gateway.Handler` directly via
  `httptest`, which is a real, thorough test of the handler logic, but
  never proves the actual compiled `cmd/hupi` binary starts, binds a
  port, and serves `/healthz`/`/readyz`/`/metrics` correctly end to end).
- **Tier 3's actual enterprise-licensed code is not in this repo**
  (`internal/auth`'s `TeamAuthenticator` hook is nil by default — see
  [CODE_GUIDE.md §6](CODE_GUIDE.md)), so none of this gate's test runs
  ever exercise real team authentication, only the OSS scope-isolation
  mechanics underneath it.
- **`cmd/hupi-admin`, `cmd/hupi-audit`, `cmd/hupi-export`,
  `cmd/hupi-export-memory`, `cmd/hupi-import`, `cmd/hupi-ingest-turns`,
  `cmd/hupi-answer-question`, `cmd/hupi-correct`, `cmd/hupi-backfill-memories`,
  `cmd/hupi-rotate-key`, `cmd/hupi-trace` have no `main_test.go` of their
  own** — each one's real logic lives in, and is tested via, the
  `internal/*` package it thinly wraps (confirmed in
  [BINARIES.md](BINARIES.md)'s own survey), but the `main()` wiring
  itself (flag parsing, exit codes) is untested in isolation for these
  11 binaries. Lower risk than it sounds — the wrapped logic is the part
  that can actually be wrong — but worth naming plainly rather than
  implying flag-parsing is covered when it isn't.

---

## 4. Running it

Locally: `./master-test.sh` (see its own header comment for every env
var it reads and their defaults). In CI: wired into
`.github/workflows/ci.yml`'s `go` job directly, plus a `master-test-gate`
job that requires every job (`go`, `vscode-extension`, `site`, `deploy`)
to succeed — that job's name is the one to mark as a required status
check in this repo's branch protection settings if you want this to
actually block a merge, not just report red.
