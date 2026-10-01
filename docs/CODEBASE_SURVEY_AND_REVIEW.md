# HUPI codebase survey and business-logic review

This document has two parts. **Part 1** is a process survey — what actually
happens, subsystem by subsystem, grounded in a full read of the real source
(not inferred from names or prior docs). **Part 2** is a consolidated code
review — every finding from that reading, deduplicated and ranked by real
severity, with exact file:line references and a concrete failure scenario for
each one. Nothing here is speculative: every claim was checked against the
actual code at the time of writing (branch `review/full-codebase-survey`,
based on `main` @ `551ff54`), and several findings were further cross-checked
against currently-running code before being accepted.

**One load-bearing cross-reference up front**: two of this review's own
findings (A4, A5 below) were already fixed on a separate branch
(`fix/guarantee-line-budget-starvation`, commits `ef97aea`/`2491ad6`,
[PR #17](https://github.com/hupi-dev/hupi/pull/17)) at the time this
review was conducted — that PR has since merged into `main`. They're
included here because they were real bugs in the code this review
actually read, not because they were unknown.

---

# Part 1: Process survey

## 1. The basic lifecycle

A conversation turn arrives at the gateway as an OpenAI-shaped chat
completion request. The handler resolves the caller's scope, retrieves
relevant memory, injects it as context, calls the configured LLM, and
captures the turn as a new episode. Overnight, a cron job consolidates each
scope's new episodes into a daily summary (clustering busy days, grounding
every extracted fact against source text, merging entity attributes,
detecting contradictions against other periods), and on calendar boundaries
rolls daily summaries up into weekly/monthly/yearly ones. Separately,
operators can rotate encryption keys, re-embed after a model change, export
or import a portable encrypted snapshot, and provision users/teams/API keys
through an admin console — all while Postgres Row-Level Security enforces
tenant isolation underneath every one of these paths.

## 2. HTTP gateway and request lifecycle

`internal/gateway/handler.go` is the real HTTP surface. `HandleChatCompletions`
resolves scope first: with no `Auth` configured (Tier 1/2, the OSS default),
every request resolves to a fixed default private scope, no header needed;
with `Auth` set, a `Bearer` token resolves through `Authenticator.Resolve`,
and any failure — missing header or rejected token — collapses to a generic
401 so a client can't use error content to probe why a token was rejected
(the real reason is logged server-side only).

Unless `X-Hupi-Memory: off`, `Retriever.Retrieve` is called; a retrieval
error **degrades to no-memory rather than failing the turn**. The result's
`ContextMessage` is prepended as a system message. If the question looked
ordering-shaped (`NeedsAggregationPass`), a separate aggregation-reasoning
pass (`aggregation.go`) may inject one more deterministic hint (e.g. a
computed "closest consecutive pair" for a "two events in a row" question) —
this pass re-checks the narrower shape it actually helps with before paying
for its own extraction LLM call, and any failure anywhere in it degrades to
no hint, never blocking the turn.

The provider call then happens two ways: non-streaming (`handleNonStream`,
await the full response) or streaming (`handleStream`, SSE, draining a
channel from the provider adapter). In both cases the response is captured
back as a new episode (hash-deduped, cheap local importance heuristic, no
network call) — unless `X-Hupi-Capture: off`. A capture failure is logged,
not fatal, except for `/v1/feedback`, where the opposite tradeoff applies
deliberately: a failed write **is** a hard 500, since persisting feedback is
the entire point of that request. `X-Hupi-Explain: on|deep` adds citations
to the response; `deep` additionally runs one more real LLM call
(`attributionCheck`) asking the model which citations it actually used.

Team-scoped routing is a pure extension seam: `gateway.MountTeamRoutes` and
`auth.NewTeamAuthenticator` are nil-by-default function-pointer variables;
the real Tier-3 implementation lives in a separate, closed-source repo and
simply isn't present here. When nil, no team routes are mounted at all —
not routes that always 403, genuinely absent.

## 3. The retrieval engine

`Store.Retrieve` (`internal/store/retrieve.go`, the largest file in the
repo) runs in two transactions split around the one embedding network call.
It always loads an "anchor" first — the acting user's own `self_model`
(personal voice, even inside a team workspace) plus a one-line pointer to
the latest daily summary. A cheap, no-LLM "stage 1" gate then decides
whether to search further at all: exact entity name/slug substring matches,
or a keyword/question-shape signal. If neither fires, retrieval stops at
`GateSkipped` — anchor only.

Stage 2 embeds the query once and reuses it everywhere. Two query-shape
detectors widen thresholds for that one call: a recommendation-seeking
question (`looksLikeRecommendationRequest`) widens entity search; an
ordering/counting question (`looksLikeOrderingRequest`) widens summary
search — this is the *second* attempt at that idea, after a first attempt
(widening only the final-selection count) was tried and reverted on real
evidence that the candidate pool itself was empty, not under-selected.

Summaries are searched via Reciprocal Rank Fusion: a vector pass and a BM25
keyword pass (BM25 over *every* summary in scope, since there's no index
over encrypted text) are combined (`fused = 1/(1+rank+1)` per signal
present), boosted if the candidate's own dated period overlaps a timeframe
the question implies (temporally-wrong candidates are excluded from the pool
entirely, not just deprioritized, with a backoff if that would empty the
set), then MMR-selected for diversity. Rendering is a deliberate two-pass
write: every picked summary's single best "guaranteed" fact is written
first, *before any* summary's deeper fact list — so the final budget cut
can only ever land on depth content, never on a guarantee that hasn't been
written yet. Below a small fact-count threshold, every fact is guaranteed
together rather than picking one (a real fix for a measured coin-flip
otherwise). Above it, the single highest-relevance fact is chosen via a
full ranking function, not a winner-take-all tie-break.

Episodes are searched by vector (only ones consolidation chose to embed, a
stricter similarity threshold than summaries since raw text is ungrounded)
and by keyword (every interaction episode, including ones never embedded),
each independently, no fusion between them. Entities are searched the same
two ways, plus the recommendation-widened path. A graph walk over
`entity_relationships` runs last, seeded from every entity found by any
mechanism above — measured three separate times to have no benefit on
either public benchmark, but left enabled by default since it's cheap and
bounded.

Everything accumulates into one string, then one global truncate to a
configurable character budget (default 2000, published benchmark runs use
20000) is the final backstop. The gate is `GateFull` if anything matched
above its own threshold anywhere, else `GatePartial`.

## 4. Consolidation engine

`cmd/hupi-consolidate` enumerates every user/team nightly and calls
`Runner.RunDaily` per scope, continuing past any one scope's failure.
Below a per-day episode-count threshold, one LLM call produces the day's
summary; above it, episodes are embedded, clustered by cosine similarity
(union-find, capped at a max cluster count via centroid-merging), and each
cluster gets its own summary call, mechanically concatenated — never a
further LLM compression pass, since that would reintroduce the exact
dilution clustering exists to fix. On topic-diverse busy days, a second,
independent per-episode extraction pass adds further insurance facts.

A second, independent LLM call (`groundingCheck`) re-verifies every
extracted fact against the raw source text — deliberately never shown the
model's own self-cited source episode, so a plausible-sounding false
citation can't talk its way past the check. An ungrounded fact is stored,
not deleted, just excluded from retrieval. Entity attribute merging is a
flat per-key overlay; a model-declared `SupersedesKeys` list lets a later
extraction explicitly retire an old, differently-named key for the same
real fact, closing a real production incident where two keys for the same
concept sat side by side forever.

After the day's summary is durably stored, a best-effort cross-period
contradiction pass looks for other current summaries (any level/period)
sharing a touched entity, asks a narrow LLM call whether they conflict, and
if so, surgically edits the older summary's facts/prose and writes it
through the same machinery a human-driven correction uses (`Runner.Correct`,
which itself refuses to act if the target is already superseded by someone
else). Rollups are not strictly write-once: a stale-check compares each
rollup's own creation time against its sources' most recent change and
regenerates/cascades upward when needed, triggered by same-day
re-consolidation or by a contradiction correction.

## 5. Identity, auth, crypto, and row-level security

`identity.Scope{Kind, Owner}` is the universal tenancy key — ids alone
aren't globally unique (two teams can each have an entity called the same
slug), so every scoped table's real primary key includes scope. Every
scoped query goes through `dbscope.Run`/`SetSession`, which sets four
Postgres session variables; RLS policies on `episodes`/`summaries`/
`entities`/`entity_relationships` check those variables via
`current_setting(..., true)`, which returns NULL when unset — and
`NULL = anything` is never true, so a transaction that skips this setup is
denied every row, not granted all of them. This is backed by connecting as
a non-owner `hupi_app` role (RLS is bypassable by table owners/superusers in
Postgres, so this role choice is load-bearing, not cosmetic).

`crypto.KeyStore` wraps one DEK per scope under a single deployment-wide
KEK, versioned for rotation: writes always resolve the current version
fresh (so a long-running process notices another process's rotation on its
very next write); reads resolve whatever version the row itself recorded.
Credentials (API keys, operator tokens) are generated as high-entropy random
values, hashed before storage, and returned to the caller exactly once —
never logged, never compared byte-by-byte in Go (every check is a SQL
equality lookup against a stored hash).

## 6. Data lifecycle: key rotation, re-embedding, HPMF, audit

Key rotation is a resumable state machine: `Start` mints a new key version
and flips every subsequent write onto it immediately; `Continue` walks
episodes → summaries (and their key facts) → entities in fixed-size
batches, each fully committed in one transaction so a crash mid-batch rolls
back cleanly and a resumed run naturally picks up only still-old-version
rows; `Prune` refuses unless the rotation is marked complete and
independently re-verifies via a live count that nothing still references an
old version before deleting it. Re-embedding needs no persisted cursor at
all — "pending" is a live predicate (`embedding is null or model mismatch`),
self-correcting after any crash, with each row's embedding committed
independently.

HPMF export writes every version of every summary (including corrected,
no-longer-current ones — a correction's history is part of the integrity
story), each row decrypted with its own recorded key version, then
age-encrypts the packaged result. Import, without `-merge`, requires an
empty target scope and fails loudly otherwise; episodes dedupe by content
hash and remap colliding global ids; summaries skip any period that already
has any version, otherwise reinsert the whole version chain with
`supersedes` rewritten through a fresh id map.

## 7. Admin provisioning and the public demo

`hupi-admin` provisions users/teams/operators directly, audits every
state-changing action, and fails closed (clear error, no partial writes) for
the two Tier-3-gated subcommands when the enterprise hooks are absent.
`hupi-admin-ui` wraps every `/api/*` route in Basic-Auth operator resolution
plus a same-origin CSRF check in one place, verified to be the only path
into that mux — there is no alternate, unwrapped route table.

The public demo provisions a real, ordinary scoped user per guest session,
behind a token hashed the same way as every other credential, capped on
messages-per-session and consolidate-calls-per-session via atomic
check-and-increment UPDATE statements (genuinely race-safe). A sweep job
deletes expired/backstopped guest sessions and all their data on a cron
cadence.

## 8. Provider abstraction, bootstrap, and selfcheck

One `Provider` interface is implemented per wire *shape*, not per vendor —
every OpenAI-compatible vendor (OpenAI, Azure, Groq, OpenRouter, local
Ollama/vLLM) shares one adapter, differing only by config; Anthropic gets
its own adapter because its wire shape genuinely differs (top-level system
prompt, different auth headers, no embeddings endpoint at all). `Registry`
validates that every configured role resolves to a defined profile at
construction time — a typo in `providers.yaml` fails the whole process at
startup, never at first real use. `bootstrap.Load` is a strict, mostly
hard-fail startup sequence; `VerifyEmbedding` is deliberately separate from
it (several CLI entrypoints never embed and shouldn't be blocked by an
unrelated provider outage) and exists specifically to catch an embedding
dimension mismatch at startup rather than as a `pgvector` write-path crash
hours later. `selfcheck` runs hand-written probes against a live retriever
and exits non-zero on any failure, surfaced purely through ordinary
Kubernetes Job-failure alerting — no in-repo paging integration.

## 9. Schema and migrations

Fifteen migrations evolved the schema from a single-tenant prototype
(episodes/summaries/entities only) through scoping, RLS, key-rotation
support, audit logging, vector search for entities, embedding-model
tracking, the public demo, and an entity-relationship graph. Every
`NOT NULL` column added to an existing table correctly supplies a
`DEFAULT`, so none of the `ALTER TABLE`s would fail against pre-existing
rows. `entity_relationships` is the one table whose foreign keys are
deliberately composite (`scope_kind, scope_owner, subject_id`) rather than
a bare reference to `entities(id)`, specifically because entity ids aren't
globally unique post-scoping — exactly the kind of real cross-scope leak a
bare FK would otherwise allow.

## 10. The VS Code extension

Authentication is either OIDC (Authorization Code + PKCE against a
configured issuer, token refresh with a 2-minute margin) or a manually
pasted API key — both held only in VS Code's `SecretStorage`, never
settings.json, never logged. The chat participant (`@hupi`) and the sidebar
both attach the active editor's visible file/selection (capped at 8000
chars) to every turn automatically, relying on VS Code's own chat history
or a small in-memory array respectively; the sidebar aborts any in-flight
stream before starting a new one. Inline completions are off by default,
debounced, and fail silently by design on error (the sidebar/participant
already surface errors clearly). Inline Edit and Multi-File Edit are built
on the same `streamChat`/`chat` helpers but, as detailed in Part 2, don't
wire in cancellation the way the other two surfaces do.

---

# Part 2: Consolidated findings

Findings are grouped by severity as assessed during review, then by
subsystem. Each cites exact file:line and a concrete failure scenario.
**[Already fixed, unmerged]** marks the two findings resolved on
`fix/guarantee-line-budget-starvation` but not yet in `main`.

## A. Likely real bugs

**A1. ✅ FIXED (merged, PR #18) — Streaming responses leak a goroutine and an open HTTP connection on client disconnect or context cancellation mid-stream.**
`internal/gateway/handler.go` (`handleStream`'s drain loop) stops reading
from the provider's channel the instant `ctx.Done()` fires. Both provider
adapters (`internal/provider/openai_compat.go`, `anthropic.go`) feed that
channel from a background goroutine via an **unbuffered** `out <- chunk`
with no `select` on cancellation — so the producer goroutine blocks forever
on its next send, its `defer resp.Body.Close()` never runs, and both the
goroutine and the HTTP connection leak. Confirmed independently by both the
gateway and provider reviewers. Trigger: any client that disconnects or
whose request context is cancelled while a streamed answer is still being
generated — a routine occurrence for chat UIs with a "stop generating"
button or for any reverse proxy with an aggressive timeout.

**A2. ✅ FIXED (merged, PR #19) — `attributionCheck` failures are indistinguishable from a positively-verified "not used," contradicting the documented `nil`-means-"not checked" contract.**
`internal/gateway/handler.go`/`attribution.go`. `Citation.Used`'s own doc
comment promises `nil` unless the check actually ran. But when the judge
LLM call succeeds yet returns unparsable or miscounted JSON,
`attributionCheck` logs a warning and returns an **all-false slice with no
error** — the handler's only nil-preserving branch is `attrErr != nil`, so
this specific failure mode silently reports every citation as "confirmed
unused" to any caller building UI on top of `hupi_citations`.

**A3. ✅ FIXED (merged, PR #20) — Switching the active embedding provider without re-embedding silently mixes incompatible vector spaces, with nothing catching it.**
`bootstrap.VerifyEmbedding` only checks vector *length* compatibility, not
model *identity*; the retrieval nearest-neighbor queries
(`internal/store/retrieve.go`, vector search over summaries/episodes/
entities) apply no `embedding_model` filter at all. After a provider switch
(e.g. a locally-hosted model → a cloud model, same 1536-length output),
every retrieval query until a `hupi-reembed` backfill completes
cosine-compares query vectors against two genuinely incompatible embedding
spaces as if they were comparable — no error, no log line, no self-check
probe catches it. This is the only place this mismatch is tracked at all
(the consolidation-side backfill queries know about `embedding_model`;
retrieval doesn't).

**A4. ✅ FIXED (merged, PR #17) — Cross-summary guarantee-line budget starvation.**
`internal/store/retrieve.go`, `fusedSearchSummaries` pass 1. No per-summary
cap on the "guaranteed fact" line means the highest-ranked summaries among
many candidates can exhaust the whole context budget before a lower-ranked
but still-correct summary's own guarantee is ever written. Real, confirmed
regression (a previously-correct LongMemEval question came back
consistently wrong after an unrelated fix changed which fact gets
guaranteed, shifting line lengths enough to tip the budget). Fixed on
`fix/guarantee-line-budget-starvation` (`ef97aea`) via a fair, N-aware
per-summary cap. Real-verified 4/4 correct, stable across 4 full sample
re-runs. **Status: fixed and merged into `main` ([PR #17](https://github.com/hupi-dev/hupi/pull/17)).**

**A5. ✅ FIXED (merged, PR #17) — Episode text has no per-entry budget cap and no excerpt-centering.**
`internal/store/retrieve.go`, `vectorSearchEpisodes`/`keywordSearchEpisodes`.
Every matched episode's full USER/ASSISTANT text is written uncapped; the
only backstop is one global tail-truncate with no awareness of where in a
long, multi-turn episode's own text the relevant detail sits. Real,
confirmed failure case where a needed detail sat ~18,000 characters into one
matched episode. Fixed on the same branch (`2491ad6`) via a per-episode cap
plus a density-weighted centered excerpt. Real-verified 5/5 correct, stable
across 4 full sample re-runs. **Status: fixed and merged into `main` ([PR #17](https://github.com/hupi-dev/hupi/pull/17)).**

**A6. ✅ FIXED ([PR #21](https://github.com/hupi-dev/hupi/pull/21)) — The consolidation pipeline's "best-effort" post-commit steps aren't actually best-effort — they propagate into a hard failure and skip further real work.**
`internal/consolidation/store.go` (`storeSummary`'s post-commit
`embedSummary`/`embedEntities` calls) and `runner.go`
(`embedHighImportanceEpisodes`, the entity-embedding backfill). Each
function's own doc comment states a failure here "shouldn't fail the whole
run" — but the error return propagates through `RunDaily` to
`cmd/hupi-consolidate`'s scope loop, incrementing its failure counter and
making the whole cron invocation exit non-zero. Worse, because `RunDaily`
returns immediately on this error, the same scope/day also silently skips
cross-period contradiction detection and the embedding backfills that would
otherwise have run after it. A transient embedding-provider timeout on an
otherwise fully-correct consolidation day (summary, facts, entities, audit
log all committed) produces a false "failed" alert and real, silently lost
downstream work.

**A7. ✅ FIXED ([PR #22](https://github.com/hupi-dev/hupi/pull/22)) — A daily summary consolidated for the first time after its week's rollup already exists never triggers a refresh of that now-stale rollup.**
`internal/consolidation/runner.go`. `refreshRollupsCovering` is only called
when `existingCurrentID != ""` — reasoning that nothing existed to be stale
before. That doesn't hold for a day that had zero episodes when its week's
rollup first ran and is later backfilled (out-of-order capture, import, a
manual re-run for that specific date): this produces that day's genuinely
first summary, so the refresh is skipped even though the existing rollup is
now provably stale by the code's own staleness check. Nothing in the
ordinary cron cadence ever revisits a past calendar boundary, so the gap is
permanent until an operator manually re-triggers the rollup.

**A8. `rotate.Start` has an unlocked check-then-act race that can orphan a key version under concurrent rotations of the same scope.**
`internal/rotate/rotate.go`. Two simultaneous `Start` calls for a scope with
no existing rotation can both read "not in progress," both proceed, and
each resolve `CreateNextVersion` against whatever the *other* has already
advanced — the loser's upsert overwrites the `key_rotations` progress row
with a `from/to` pair that no longer reflects reality, leaving rows the
other process already migrated untracked by any version in the surviving
row. `Prune`'s own independent live-count safety check does prevent data
loss (it refuses to delete a version still referenced), but the scope is
left with a permanent extra, undeletable key version and an inaccurate
progress row.

**A9. `hupi-reembed`'s audit entry can be permanently lost for work that genuinely happened.**
`cmd/hupi-reembed/main.go`. `LogRun` is gated on *this invocation's*
processed count, not on whether unaudited work exists anywhere. If the
process is killed after real embedding writes but before its own `LogRun`
call, the next invocation sees nothing pending (the live predicate is
already satisfied) and never logs — that batch of real data changes is now
permanently absent from `audit_log`, with no self-healing retry path.

**A10. HPMF export/import audit entries are written after all real data writes commit, in a separate final step — and a crashed non-merge import can't be cleanly retried.**
`internal/hpmf/export.go`/`import.go`. A crash between the real writes
committing and that final audit insert leaves a scope with genuinely
changed/imported data and zero audit trail. For import specifically,
without `-merge`, a retry of the identical command now fails outright
(`scopeIsEmpty` is false) with no indication that `-merge` is the required
remediation — the omission can't self-heal the way a merge-mode retry
eventually would.

**A11. The demo's daily session cap is defeated by the sweep job, making the real achievable session volume roughly 8x the intended cap.**
`internal/demo/store.go`, `cmd/hupi-demo-sweep/main.go`. The cap counts
`demo_sessions` rows created in the last 24 hours, but the sweep job
(meant to run every ~15 minutes) hard-deletes a session's row once its
~3-hour TTL expires — not mark it expired, delete it, cascading from the
guest user's own deletion. A session created at hour 0 stops counting
against the 24-hour cap by hour ~3:15. With defaults (TTL 3h, cap
150/day), the real achievable daily session count is roughly 8x the stated
150 — directly undermining the cap's one stated purpose (bounding
aggregate real LLM spend on an unauthenticated public endpoint).

**A12. Inline Edit and Multi-File Edit have no cancellation support at all, unlike every other call site in the same extension.**
`vscode-extension/src/inlineEdit.ts`, `multiFileEdit.ts`. Both wrap their
gateway call in a non-cancellable progress notification and pass no
`AbortSignal` into `streamChat`/`chat` — contrast with the chat participant
and sidebar, which both wire an `AbortController`. A slow or hung gateway
leaves the user with no way to cancel short of reloading the window, and
re-triggering the command starts a second concurrent request with no
supersede/abort of the first.

**A13. The extension sends up to 8000 characters of the active file on every turn with no UI indication and no opt-out.**
`vscode-extension/src/chatViewProvider.ts`/`chatParticipant.ts`. The
echoed user message in the sidebar transcript shows only the user's typed
text — the actual prepended file-context block sent to the gateway is never
shown. A user asking an unrelated question while a sensitive file happens
to be open or scrolled into view has no way to see, from the UI, that the
whole visible file was transmitted (and, per this repo's own capture model,
potentially persisted into memory) — and no setting exists to disable the
behavior.

## B. Plausible edge cases

- **Mid-stream provider errors aren't counted in `ProviderCallErrorsTotal`** — only the initial-connect failure path increments it; a failure arriving via a stream chunk's `Err` field only logs. (`internal/gateway/handler.go`)
- **`docs/API_REFERENCE.md` documents team-route functions/line numbers that don't exist in this OSS build** (`HandleTeamChatCompletions`, `resolveTeamScope`) — stale doc from before the current nil-hook mechanism replaced an earlier inline implementation.
- **Demo's `clientIP` trusts the leftmost `X-Forwarded-For` entry unconditionally** — defeatable under the common "append" reverse-proxy pattern, which would let an attacker fabricate a value and bypass the per-IP session limiter (not confirmed exploitable without the real proxy config, but the one documented purpose of the limiter depends on it).
- **Retry logic applies identically to `Embed` (idempotent) and `ChatCompletion`/stream (billed, not idempotent) with no idempotency key** — a network error arriving after the upstream provider already received and processed the request can cause a real duplicate-billed/duplicate-generated retry. (`internal/provider/retry.go`)
- **`migrateLegacyDEK`'s existence-check and insert aren't wrapped in one transaction** — two processes racing through `bootstrap.Load` on first deploy could both pass the check (safe in the end due to `on conflict do nothing`, but recoverable-only, not truly atomic).
- **The shipped `probes.yaml` only exercises the happy-path/full-gate shape** — no probe asserts a `partial` gate, a correctly-skipped irrelevant query, or a non-default scope; a regression that always over-matches to "full gate" would pass every shipped probe.
- **`vectorSearchEpisodes` is the one retrieval path that never received the timeframe hard-filter fix five sibling paths got** — a "last month"-shaped query can still surface a temporally-wrong raw episode verbatim via the vector path alone. (`internal/store/retrieve.go`)
- **`temporalRelevanceBoost`'s value is numerically double what its own doc comment claims** (`reciprocalRank(0) = 0.5`, not 1.0, given the file's own `rrfK`) — not shown to cause an observed failure, but the boost is roughly 2x stronger than its stated design intent.
- **`truncateToBudget`/`hardTruncate` do raw byte-slicing, not rune-aware** — can produce invalid UTF-8 at a multi-byte character boundary for any non-ASCII content, with no test covering this.
- **BM25 keyword-search SQL has no `ORDER BY`, and the subsequent sort isn't stable** — which tied-score candidates survive a truncation cap can vary run-to-run, a reproducibility risk given how heavily this project's own iteration leans on exact before/after benchmark comparisons.
- **The additive RRF fusion formula structurally favors "found twice, weakly" over "found once, solidly"** — confirmed by the project's own prior investigation as a real multi-hop regression cause, explicitly left as an open, undecided tradeoff rather than fixed; still present in current code unchanged.
- **Graph-walk relationship traversal has been measured three separate times to have no benefit on either public benchmark, yet remains enabled by default** and still runs (bounded cost) on every qualifying request, competing for the same fixed context budget that findings A4/A5 show is a real, recurring bottleneck.
- **A near-empty/punctuation-only query still pays the full embedding+search cost** and silently reduces to vector-only (keyword search's own gate requires at least one real token), with nothing in the result indicating keyword search didn't actually run for that turn.
- **Correction fork race**: `Runner.Correct`'s "not already superseded" guard and its actual write are separate transactions with a network round-trip in between, and `summaries.supersedes` has no unique constraint — two concurrent corrections of the same target (an operator's manual correction racing the automatic contradiction detector, or two different triggering days both correcting the same older summary) can both pass the guard and both successfully fork history.
- **The per-episode insurance extraction pass is silently skipped whenever the clustering embedding call fails or collapses to one cluster** — reintroducing the exact pre-clustering dilution behavior on precisely the busy, topic-diverse days the mechanism exists to protect, with only a log line and no retry.
- **`summary_key_facts.source_episode_ids` citations are never validated for referential existence** — unlike the relationship-existence check, which does validate — so a hallucinated or stale episode-id citation on an otherwise-correctly-grounded fact is stored permanently unchecked. (`internal/consolidation/store.go`)
- **`episodes.refers_to` crosses scopes with no ownership check** — the feedback endpoint builds a feedback row pointing at any client-supplied episode id with no verification it belongs to the caller's own scope. Low practical exploitability (ids are high-entropy and unguessable, no content is read back through this path), but a real, unvalidated cross-scope reference that a future tool joining on `refers_to` could surface confusingly.
- **`summary_key_facts` has no RLS and no scope columns of its own; four direct (non-join) query call sites rely entirely on caller-side sequencing** (verify the parent summary's scope first, then query facts by `summary_id`) with no independent enforcement — correct today, but structurally fragile and exactly the condition the schema's own comment warns future code against. Call sites: `internal/consolidation/contradiction.go:104`, `internal/consolidation/runner.go:430` (`CurrentContent`), `internal/hpmf/export.go:261`, `internal/rotate/rotate.go:408`.
- **No synchronization between concurrent `rotate` and `reembed` runs on the same scope** — a narrow timing window where a reembed batch reads a row's key version just before a concurrent rotation-and-immediate-prune removes that version can abort the batch with a confusing (but non-corrupting) error.
- **A dangling `supersedes` reference is silently discarded during HPMF import** if the predecessor version isn't present in the bundle (e.g. a partial/filtered export) — the imported row ends up indistinguishable from an original, non-corrected summary, with no warning and no stats field reflecting the lost lineage.
- **`CreateSession`'s own daily-cap check is a separate, un-locked TOCTOU race** independent of finding A11 — the count-check and the insert aren't tied together by a transaction, lock, or constraint.
- **`entity_relationships` has no `CHECK` that `valid_from <= valid_until`** — a buggy caller could insert a relationship that "ends before it starts" with nothing to stop it.
- **`audit_log` has no index on `target_ref`** — a natural "every event about entity X" query is a full scan over an append-only, ever-growing table.
- **Migration order/idempotency-probe logic is duplicated between `install.sh` and `schema/migrate.sh`** — they agree today, but nothing enforces that a future migration gets added to both, risking silent divergence between the bare-metal and containerized install paths.
- **No request timeout is configured on the VS Code extension's chat/completion calls** — a hung gateway has no app-level safety net beyond the SDK's own default and manual cancellation (itself missing for two of four call sites, see A12).
- **`multiFileEdit`'s block parser silently drops a file's proposed edit entirely if the model's response is truncated mid-file** — no user-visible warning distinguishes "nothing needed to change" from "the response got cut off."
- **Reopening the VS Code sidebar doesn't replay prior conversation history into the webview** — the extension host still holds it (and will send it on the next turn), but the visible panel is blank, which could confuse a user about what context the next message actually carries.
- **Inline completion's silent-failure-by-design means a persistent bad API key produces zero visible feedback anywhere** (no output channel, no log) — reasonable for ghost text specifically, but makes this failure mode hard to self-diagnose.

## C. Minor / stylistic

- No validation that a chat message's `role` is one of the expected values before forwarding upstream (low blast radius — the vendor API would reject it anyway).
- `X-Hupi-Memory`/`X-Hupi-Capture` header checks are case-sensitive exact matches (`"off"` only) — fails toward the safe default, but a client sending `"Off"` silently gets the opposite of what it asked for.
- Provider error-wrapping loses the provider's own name specifically on context-cancellation returns, unlike every other error path in the same function.
- No panic recovery around the consolidation scope loop — contradicts the stated "one scope's failure never blocks another's" design intent in the one case (a panic, not a returned error) that intent doesn't actually cover; no concrete panic path was found.
- `nextSummaryID`'s version computation has a narrow concurrent-write race (fails as a clean DB error, not silent corruption).
- Non-merge HPMF import gives no error hint suggesting `-merge` as the fix after a partial-failure retry.
- Large key-rotation batch sizes hold a transaction (and its row locks) open longer than typical OLTP work, in tension with the feature's own "online, no downtime" goal.
- The demo's `IPRateLimiter` doc comment calls itself a "fixed-window" limiter; it's actually a sliding-window log (arguably the more accurate algorithm, just mislabeled).
- Admin-UI list endpoints (`listUsers`, `listOperators`, `whoami`) write no audit row — matches the narrower design-doc scope ("detail-page views"), but technically contradicts a broader, looser claim elsewhere in that same doc's own prose ("every view...").
- `entity_relationships` has no index on `predicate` alone (low value today given likely table size).
- The audit-log event-type `CHECK` constraint has been widened via a non-atomic drop-then-add across three separate migrations.

## D. Confirmed clean — explicitly checked, nothing found

Stated explicitly because thoroughness was the point of this review, not
just to pad the findings list:

- **No cross-tenant query path was found anywhere** that touches a scoped
  table without going through `dbscope.Run`/`SetSession` first — the single
  most important thing this review checked, across `internal/store`,
  `internal/consolidation`, `internal/rotate`, `internal/reembed`,
  `internal/hpmf`, and `internal/demo`.
- **No SQL-injection surface found** — every query uses parameter
  placeholders; no query text is ever built via string concatenation of
  untrusted input.
- **No credential logging or insecure comparison found**, anywhere: API
  keys, operator tokens, and OIDC tokens are hashed-and-compared via SQL
  equality (never a Go-level byte comparison of secret material), never
  logged, never echoed into error messages, never stored outside
  `SecretStorage`/hashed DB columns.
- **All four Tier-3 nil-hooks, across every call site found (six total),
  degrade safely by default** — no path was found where an absent
  enterprise extension silently grants elevated access instead of simply
  omitting the feature.
- **`KeyStore.GetVersion` on an already-pruned version fails clearly**, with
  no panic and no silent fallback to a wrong key; `Prune` itself
  independently re-verifies via a live count that nothing still references
  a version before deleting it.
- **Key rotation and re-embedding are both genuinely resumable** — verified
  by tracing actual transaction boundaries (not just trusting doc
  comments): a crash mid-batch loses at most the in-flight batch/row, never
  double-processes, never skips.
- **Row-Level Security fails closed when session variables are unset**,
  confirmed against the actual policy SQL (`NULL` comparisons never match)
  and a dedicated existing test.

---

## Severity summary

| Severity | Count |
|---|---|
| Likely real bugs | 13 (✅ 7 fixed — A1/A2/A3 merged PR #18/#19/#20, A4/A5 merged PR #17, A6 PR #21, A7 PR #22; 6 remaining) |
| Plausible edge cases | 24 |
| Minor / stylistic | 10 |
| Confirmed clean | 7 areas |

Of the two highest-leverage findings, **A6** (consolidation's best-effort
steps aren't best-effort — a transient provider hiccup used to produce a
false alarm *and* silently drop real downstream work) is now fixed
([PR #21](https://github.com/hupi-dev/hupi/pull/21)). **A11** (the
demo's cost-control cap is defeated by its own sweep job by roughly 8x)
remains open. Both were genuine business-logic gaps between what the
code's own comments say the design intends and what it actually does,
not edge-case corner
cutting.
