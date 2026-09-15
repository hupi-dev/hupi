# HUPI — How It Works, End to End

This is a single narrative walkthrough of the whole system as it actually
exists in this repo today: what's stored, where, how a live chat turn
flows through the gateway, how memory gets written by consolidation, and
how the observability loop (trace/feedback/self-check) closes over it.

It complements, rather than replaces, the other docs:

- [ARCHITECTURE.md](../ARCHITECTURE.md) — the design and the *why* behind
  each choice, plus the tradeoffs accepted along the way.
- [MEMORY_FORMAT.md](MEMORY_FORMAT.md) — the record schema (episode,
  summary, entity) as a portable interchange format.
- [INSTALL.md](INSTALL.md) — the actual, verified install procedure for
  all three tiers.
- [CODE_GUIDE.md](CODE_GUIDE.md) — the structural map: project layout,
  package dependency graph, and the exact call chain for every non-HTTP
  entry point (cron jobs, CLIs).
- [API_REFERENCE.md](API_REFERENCE.md) — every HTTP route's request/
  response shape, status codes, and full call chain down to Postgres.
- [BUSINESS_PROCESS.md](BUSINESS_PROCESS.md) — what this product is, who
  it's for, and worked end-to-end usage scenarios, written for a
  non-code audience.

This document is deliberately code-grounded: it names real tables, real
files, real functions, real env vars. Section 9 is an explicit list of
where the design and the current code diverge — read that before assuming
everything described elsewhere is fully built.

## 1. The one-paragraph mental model

A Go binary (`cmd/hupi`) sits between any OpenAI-compatible client and any
configured LLM vendor, speaking the OpenAI chat-completions shape on both
sides. Every fact HUPI knows lives in Postgres + pgvector
(`schema/0001_init.sql`) — not in any model's context window. A live chat
turn pulls relevant memory in, forwards the augmented request upstream,
and durably records what happened. A separate, offline process
(`cmd/hupi-consolidate`, run by cron) periodically reads raw interaction
history and distills it into grounded, citable summaries — the thing
retrieval actually searches over. Three small tools
(`hupi-trace`, `/v1/feedback`, `hupi-selfcheck`) exist purely so you can
check whether any of this is actually working, instead of trusting it
blindly.

## 2. What's stored, and how

Everything lives in four tables (`schema/0001_init.sql`):

| Table | Holds | Notes |
|---|---|---|
| `episodes` | Every chat turn (`type='interaction'`) and every feedback submission (`type='feedback'`) | Append-only in practice — nothing updates `input_text`/`output_text` once written |
| `summaries` | Grounding-checked rollups at `daily`/`weekly`/`monthly`/`yearly` level | Has its own `embedding vector(1536)` column — this is what retrieval actually vector-searches |
| `summary_key_facts` | Per-fact citations + grounding result, child of `summaries` | Lets you query "every ungrounded fact ever produced" directly |
| `entities` | The long-lived knowledge graph: people, projects, preferences, skills, and the special `self_model` | Updated in place — it's current state, not history |

**Encrypted fields** (`bytea` columns: `input_text`, `output_text`, `note`,
`summary`, `fact`, `attributes`) are AES-256-GCM ciphertext
(`internal/crypto/field.go`), encrypted/decrypted only inside the Go
process — Postgres itself never sees plaintext for these columns. The key
comes from an envelope scheme wired up in `internal/bootstrap/bootstrap.go`:
a KEK read from `HUPI_KEK` (base64, env var) unwraps a DEK persisted at
`HUPI_WRAPPED_DEK_PATH` (generated once, on first run, if that file doesn't
exist yet). This is Layer 2 of the two-layer encryption model; Layer 1 —
the Postgres data directory sitting on an encrypted disk — is the
deployment's responsibility, not the app's (see ARCHITECTURE.md § Storage
security for why app-managed FUSE mounting was rejected).

The `embedding` vector column is **not** application-encrypted (searching
encrypted vectors isn't practical with plain `pgvector`) — a small, named,
accepted residual risk, not an oversight.

`docs/MEMORY_FORMAT.md`'s directory-tree format (episodes/summaries/entities
as JSONL/JSON) is not what's running day to day — it's the export/import
interchange shape, produced on demand, not the live store.

## 3. A live chat turn, step by step

Entry point: `gateway.Handler.HandleChatCompletions`
(`internal/gateway/handler.go`), mounted at `POST /v1/chat/completions` by
`cmd/hupi/main.go`.

1. **Decode.** The request is parsed as an ordinary OpenAI chat-completions
   body (`model`, `messages`, `stream`, ...).
2. **Retrieve, unless opted out.** If the request doesn't carry
   `X-Hupi-Memory: off`, the handler calls `Retriever.Retrieve` — see
   §4 below for exactly what that does. If it errors, the handler logs and
   degrades to no memory for this turn rather than failing the request.
3. **Inject.** If retrieval produced any `ContextMessage`, it's prepended
   as a `system`-role message ahead of the client's own messages. Nothing
   after it is touched.
4. **Resolve the provider.** `resolveProvider(req.Model)` first checks
   whether the client's `model` field matches a *named profile* in
   `providers.yaml` (e.g. a client asking for `"personal-claude"` reaches
   that exact profile); otherwise it falls back to
   `active_chat_provider`. Whatever the client sent is **not** forwarded
   upstream as the literal model string — each adapter (`OpenAICompat`,
   `Anthropic`) uses its own configured `Model()` unless the request
   explicitly overrides it, so a profile name never accidentally becomes
   an invalid upstream model id.
5. **Call upstream**, streaming or not (`handleNonStream` /
   `handleStream`). Streaming buffers the full text server-side while
   forwarding each delta to the client immediately.
6. **Capture — synchronously, before the turn is considered done**
   (`Store.Capture`, `internal/store/capture.go`). This is a single
   Postgres insert, no network calls beyond that one round trip:
   - Cheap local heuristic importance score (`estimateImportance`, keyword
     + question-mark based — no LLM call).
   - `hash` = sha256 of input+output, for dedup.
   - `memory_gate`, `retrieved_summary_ids`, `retrieved_entity_ids` are
     written straight from step 2's result — this is the retrieval trace,
     captured as a side effect of normal operation, not a separate
     logging path.
   - For a stream cut short (client disconnect, upstream error mid-stream),
     `truncated: true` is set and whatever text arrived is still captured
     — partial memory beats none.
   - For a non-streaming turn, capture failure is logged but the user
     still gets their answer (losing this turn's memory beats losing the
     answer). For streaming, the client-visible terminal `data: [DONE]` is
     deliberately held back until after the capture attempt — "before the
     turn is considered done" means before the client is *told* it's done,
     not merely before the content finishes arriving.
7. **Respond.**

## 4. Retrieval, in detail

`Store.Retrieve` (`internal/store/retrieve.go`) always does two things,
independent of each other:

**The fixed anchor** (`buildAnchor`) — unconditional, every single request:
- The `self_model` entity's `attributes` (hand-curated identity/voice, not
  LLM-generated — see MEMORY_FORMAT.md § "The self_model entity").
- A one-line pointer to the *period* of the latest daily summary, not its
  full text.

**The relevance gate** — a precise two-stage decision, not a fuzzy one:

| Outcome | What happened |
|---|---|
| `skipped` | Stage 1 (`stage1EntityMatches` + `stage1KeywordSignal` — both pure string matching, zero network calls beyond the one entity-table query) found no signal at all. No vector search ever runs. |
| `partial` | Stage 1 found signal, so stage 2 ran (entity fetch + pgvector search), but nothing cleared the threshold. |
| `full` | Stage 2 ran and something cleared the threshold: an exact entity-id match (always counted as a strong hit), or a summary whose cosine similarity to the embedded query is ≥ `0.75` (`vectorSimilarityThreshold`, `internal/store/retrieve.go`). |

Only `skipped` means "never looked" — `partial` and `full` both mean the
search ran and differ only in whether it found anything trustworthy. That
distinction is what makes `memory_gate` on a captured episode diagnostic
rather than just a label: an unexpected `skipped` points at the stage-1
heuristic being too conservative; an unexpected `partial` points at the
threshold or the index itself.

Stage 2's vector search runs against both `summaries`
(`vectorSearchSummaries`) and `episodes` (`vectorSearchEpisodes`) — top `5`
each by cosine distance, `pgvector`'s `<=>` operator, both reusing a single
query embedding computed once per request. Episodes only show up here if
consolidation has already embedded them (see §5's
`embedHighImportanceEpisodes`) — summaries remain the primary surface;
episode-level search exists for content that hasn't been folded into a
summary yet, or where consolidation is behind. Everything assembled —
anchor + matched entities + matched summaries + matched episodes — is
capped at a crude ~2000-character budget (`contextCharBudget`) before
being handed back as `ContextMessage`; a real build should count tokens
against the target model's tokenizer instead of characters.

## 5. Consolidation: how memory actually gets written

Consolidation never runs on the request path — it's a separate binary,
`cmd/hupi-consolidate`, meant for cron. A slow or crashed run delays memory
getting written; it can't block or break a live conversation.

**`Runner.RunDaily(ctx, date)`** (`internal/consolidation/runner.go`):

1. Load every `type='interaction'` episode for that calendar day, decrypt
   `input_text`/`output_text`.
2. Nothing happened that day → no-op, not an error.
3. Otherwise, call the consolidation LLM (`active_consolidation_provider`
   — deliberately a *different, more stable* setting than
   `active_chat_provider`, so switching your daily-driver chat model
   doesn't incidentally change how your memory itself gets written) with a
   prompt that includes every episode's id and text, instructing it to
   return one JSON object: a human-skimmable `summary`, a list of
   `key_facts` each self-citing which episode id(s) it came from, and any
   `entities_touched`.
4. **`storeSummary`** (`internal/consolidation/store.go`) then:
   - Runs the **grounding check** — a *second*, independent LLM call
     (`active_grounding_provider` if configured, else the same profile as
     consolidation) that sees only the raw source text and the bare fact
     strings, not the model's own citations, and returns one boolean per
     fact. This is what stops a plausible-but-fabricated claim from
     quietly becoming trusted memory — it doesn't trust the generating
     model's self-report, it re-checks independently.
   - Assigns an id: `sum_<period>_<level>_v<N>`, `N` one past the highest
     existing version for that period+level.
   - In one transaction: inserts the `summaries` row (`status='draft'`,
     `grounding_checked=true`), inserts one `summary_key_facts` row per
     fact (`grounded` = the check's verdict; facts that fail aren't
     deleted, just excluded from retrieval's trust), and upserts every
     touched entity.
   - After commit (not inside the transaction — a failed embed shouldn't
     roll back an already-durable summary), embeds the summary text and
     writes it to the `embedding` column.
5. Finally, `embedHighImportanceEpisodes` embeds any of that day's
   episodes clearing an importance threshold (`0.6`) that don't have an
   embedding yet — the write side of §4's episode-level vector search.
   Runs after the summary is stored, not before: a failure here doesn't
   block the summary, since summaries are the primary retrieval surface
   and this is supplementary.

**`Runner.RunRollup(ctx, level, sourceLevel, period, sourcePeriods)`** is
the identical machinery one level up — it summarizes prior-level summaries
(e.g. seven dailies into a weekly) instead of raw episodes, which is what
keeps consolidation cost bounded by *number of summaries*, not years of raw
logs. Per-fact episode citations only make sense at the daily level; above
that, traceability runs through `source_summary_periods` on the summary
record itself.

**`Runner.Correct(ctx, oldSummaryID, output, reason)`** is the only
sanctioned way to fix a wrong summary: it reloads the original source
material, re-runs the grounding check against the replacement content
(typically hand-authored by whoever spotted the error, not the same LLM
that got it wrong), and stores a new version with `supersedes`/
`correction_reason` set — never an in-place edit. `cmd/hupi-correct` is
the CLI entry point.

## 6. Closing the loop: trace, feedback, self-check

None of the above is trustworthy by itself — RAG's classic failure mode is
silent: wrong or missing retrieval doesn't error, it just produces an
answer that looks normal. Three small pieces make that checkable:

- **`hupi-trace <episode_id>`** (`cmd/hupi-trace`) decrypts one episode and
  everything its `retrieved_summary_ids`/`retrieved_entity_ids` point at,
  and prints it. Turns "why did it say that" from an unanswerable question
  into a command.
- **`POST /v1/feedback`** (`gateway.Handler.HandleFeedback`) records a
  `type='feedback'` episode — `{episode_id, rating, note}` where rating is
  `memory_correct` / `memory_wrong` / `memory_missing`. Unlike a chat
  turn's capture (best-effort — there's an answer to protect), a failed
  feedback write returns a real error to the caller, since persisting *is*
  the entire point of the request.
- **`hupi-selfcheck probes.yaml`** (`cmd/hupi-selfcheck`,
  `internal/selfcheck`) re-runs a small, hand-written set of "does memory
  still know this" questions against the live `Retriever` on a schedule,
  and fails loud (non-zero exit) if a probe's expected gate or expected
  substring stops showing up — a regression test for the retrieval
  pipeline itself, independent of whatever the underlying LLM does.

## 7. Provider abstraction, recap

Every vendor call goes through one interface, `provider.Provider`
(`internal/provider/provider.go`): `Name`/`Vendor`/`Model`,
`ChatCompletion`, `StreamChatCompletion`, `Embed`. Two adapters implement
it — `OpenAICompat` (covers OpenAI, Azure OpenAI, Groq, OpenRouter, local
Ollama/vLLM, anything sharing that wire schema) and `Anthropic` (its own
system-prompt handling, auth headers, SSE event shape). `provider.Registry`
holds one instance per named profile from `providers.yaml`
(`providers.yaml.example` in the repo root is the template) and exposes
four roles: `Chat()`, `Consolidation()`, `Grounding()` (falls back to
`Consolidation()` if unset), `Embedding()`.

## 8. Deployment shape

Five binaries, one shared config/DB/key wiring path
(`internal/bootstrap.Load`):

| Binary | Runs | Purpose |
|---|---|---|
| `cmd/hupi` | Long-lived | The gateway server, `127.0.0.1:8787` by default (`HUPI_LISTEN_ADDR` to change) |
| `cmd/hupi-consolidate` | Cron, ~daily | `RunDaily` for a given `-date` (default: yesterday) |
| `cmd/hupi-trace` | On demand | Decrypt and print one episode's retrieval trace |
| `cmd/hupi-selfcheck` | Cron, e.g. weekly | Run `probes.yaml` against live retrieval, exit non-zero on failure |
| `cmd/hupi-correct` | On demand, by hand | Write a corrected, superseding version of a wrong summary |

Required env vars for all of them: `HUPI_DATABASE_URL`, `HUPI_KEK`
(base64 32-byte key), optionally `HUPI_PROVIDERS_CONFIG` (default
`providers.yaml`) and `HUPI_WRAPPED_DEK_PATH` (default
`hupi.dek.wrapped`, bootstrapped on first run). Missing/invalid config
fails startup outright — verified behavior, not aspirational: running the
gateway with no `providers.yaml` present exits with a clear error, never a
silent fallback to an unencrypted or unconfigured state.

## 9. Design vs. what's actually built — read this before trusting the rest

Everything above describes real, building, passing-`go vet` code. A full,
detailed accounting of every remaining gap between the design docs and
this codebase — what closing each one would take, and why they're
prioritized the way they are — lives in
[DESIGN_VS_BUILT.md](DESIGN_VS_BUILT.md). The short version, kept here so
this section doesn't overstate what exists:

**Closed** (were gaps, now built): entity attributes merge rather than
overwrite (`mergeAttributes`); corrections are a real, working path
(`Runner.Correct`, `cmd/hupi-correct`) rather than schema-only; vector
search covers both `summaries` and high-importance `episodes`
(`vectorSearchEpisodes`, fed by `embedHighImportanceEpisodes`).

**Still open**:

- **Weekly/monthly/yearly rollups have no scheduler.** `Runner.RunRollup`
  exists and works given a period + source period list, but nothing in
  this repo computes ISO week/month/year boundaries and calls it — that's
  calendar logic still owed to the cron layer.
- **Tier 3 (Professional Shared) doesn't exist in code at all** — no
  `scope` column, no per-user access control, no workspace routing. Tiers
  1/2 (this whole document) are what's built.
- **The consolidation LLM's structured output is parsed with a naive
  `{...}` substring extraction** (`extractJSON`), not a real
  structured-output/tool-calling mode — fragile against a model that
  doesn't follow the "JSON only" instruction closely.
- **No live client has actually been pointed at this gateway.** Everything
  here is verified by `go build`/`go vet`/`gofmt` and one manual run
  confirming the fail-loud-on-missing-config path — there is no
  integration test against a real Postgres instance or a real upstream
  LLM yet.
