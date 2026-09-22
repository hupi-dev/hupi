# HUPI Architecture

HUPI is a **memory gateway**: a small local/self-hosted service that sits
between whatever chat client you use and whatever LLM you point it at. It
speaks the OpenAI Chat Completions API on both sides — inbound (so any
existing client, script, or IDE plugin can talk to it unmodified) and
outbound (so it can forward to OpenAI, Azure OpenAI, OpenRouter, Groq, a
local Ollama/vLLM server, or anything else that implements the same schema).
Anthropic-native or other non-OpenAI-shaped APIs are supported via a thin
per-vendor adapter behind the same interface.

Memory itself lives entirely in the portable bundle described in
[docs/MEMORY_FORMAT.md](docs/MEMORY_FORMAT.md) (HPMF). HUPI's job is to keep
that bundle up to date and to pull the right slice of it into every request.

## Why a gateway instead of a client-side library

You explicitly want "plug this into any AI." A library only works if every
client you use is willing to call it. A gateway works with *any* client that
can have its `base_url` pointed somewhere else — which is already true of
essentially every OpenAI-SDK-based tool, most chat UIs, and CLI tools like
this one. It also means the memory logic runs in one place regardless of
which model you're talking to that day.

## Client compatibility tiers

"Plug into any AI" needs a real scope, not an aspirational one — a gateway
only helps a client that's willing to point somewhere else. Being explicit
about this now avoids discovering the limit later:

| Tier | Client examples | How it integrates |
|---|---|---|
| **1 — full support** | CLIs, IDE plugins, scripts, self-hosted chat UIs, anything using an OpenAI-SDK-shaped client | Point `base_url` at the HUPI gateway. Everything in this document applies. |
| **2 — possible, not built yet** | Desktop/web chat apps with a plugin or system-proxy hook but no user-facing `base_url` field | Would need an OS-level intercepting proxy or a browser extension rewriting requests. Real, but explicitly deferred — not part of the initial build. |
| **3 — out of scope** | Closed mobile apps, SaaS chat products with no extensibility point at all | No fix available short of the vendor adding one. HUPI simply doesn't reach these; treat interactions there as memory-less by design, not a bug to solve. |

## Scope: what this gives you (and what it doesn't)

Worth being explicit about, since the original framing was "human-equivalent
AI": this architecture gives a stateless reasoner very good long-term
recall of things you and it discussed. It does not give it continuity of
judgment, identity, or lived experience the way a human has it — every
request is still a fresh model invocation that happens to be handed
relevant facts.

One concrete gap this causes: if your "voice"/communication preferences
live only in scattered episodes, they get reconstructed differently by
different consolidation runs, so the *personality* the AI presents can
drift independent of the facts it recalls. The mitigation is a
deliberately **human-curated** `entities/self_model.json` record (see
[MEMORY_FORMAT.md § Entity record](docs/MEMORY_FORMAT.md)) — not
LLM-generated, edited by you directly, always included in working memory.
That buys consistency of *presentation* across models and time. It does not
close the deeper gap: this is a very well-informed tool, not a persistent
self. Keep that distinction in mind when deciding how much to lean on it.

## Component overview

```
                    ┌─────────────────────────────────────────────┐
                    │                  HUPI GATEWAY                │
                    │        (OpenAI-compatible local server)      │
                    │                                               │
  Any client  ───▶  │  1. Retrieval Engine                         │
  (Claude Code,     │     - hybrid search over HPMF bundle         │
   Cursor, a         │     - entity lookup, recency boost           │
   script, a         │     - token-budgeted context assembly        │
   phone app...)     │            │                                 │
      base_url =     │            ▼                                 │
      localhost:8787  │  2. Provider Adapter (internal Go           │
                    │     `Provider` interface) ───────────┐       │
                    │            │                          │       │
                    │            ▼                          ▼       │
                    │  3. Capture (sync, DB write)         Upstream  │
                    │     insert episode row               LLM API  │
                    │     (committed before turn completes) (OpenAI,│
                    └──────────────────┬───────────────────Azure,───┘
                                       │                    Anthropic,
                                       ▼                    Ollama, ...)
                    ┌─────────────────────────────────────────────┐
                    │        POSTGRES + PGVECTOR                    │
                    │  episodes  summaries  entities  embeddings    │
                    │  (app-level field encryption; disk-level      │
                    │   encryption is the deployment's volume)      │
                    │  HPMF is the export/import interchange format │
                    └──────────────────┬───────────────────────────┘
                                       │
                                       ▼
                    ┌─────────────────────────────────────────────┐
                    │          CONSOLIDATION ENGINE (batch)         │
                    │  daily -> weekly -> monthly -> yearly         │
                    │  rollups, entity extraction, importance       │
                    │  scoring, index rebuild                       │
                    │  (cron / scheduled task, runs against any     │
                    │   configured LLM + embedding model)           │
                    └─────────────────────────────────────────────┘
```

## Request lifecycle (a single chat turn)

1. **Client sends a normal OpenAI-shaped request** to
   `http://localhost:8787/v1/chat/completions` with whatever model name it
   thinks it's using. HUPI's config maps that model name (or a header/param)
   to an actual upstream provider profile.
2. **Retrieval Engine** takes the latest user message (+ recent turns) and:
   - Always includes a **small, fixed-size anchor**: the `self_model` entity
     (identity/voice/preferences, human-curated, cheap) and a one-line
     pointer to the latest daily summary — not the full summary text. This
     replaces unconditionally injecting a full working-memory block on
     every request, which cost tokens and risked anchoring the model on
     irrelevant recent history even for questions with nothing to do with
     it (e.g. "what's 2+2").
   - Runs a **relevance gate**, a two-stage decision, not a single check.
     The three possible outcomes are precisely defined — this is what gets
     written to `memory_gate` on the episode record, so it has to mean the
     same thing every time for the observability/audit story to work:

     | Outcome | What happened |
     |---|---|
     | `skipped` | **Stage 1 pre-check** — a cheap local scan of the message for any signal at all (known entity names, decision/preference language, a question about something previously discussed) — found *nothing*. No vector search or entity lookup is even attempted; this is the cheap-and-common case for generic/impersonal requests (e.g. "what's 2+2", "write a regex for..."). |
     | `partial` | Stage 1 found *some* signal, so **stage 2 actually runs** (vector search over summaries, high-importance episodes, *and* entities via `pgvector`, plus the stage-1 entity-name/slug lookup) — but nothing returned clears the match threshold (exact entity-id hit, or vector similarity ≥ a configured cutoff — separately calibrated per content type: 0.40 for summaries, 0.55 for episodes, 0.50 for entities, since each embeds differently and, for episodes specifically, a generic question with no real connection to the user's data measured *higher* similarity than a genuine paraphrase did — reusing the summary cutoff there was a real miscalibration, not just an untested assumption; see `vectorSimilarityThreshold`/`episodeVectorSimilarityThreshold`/`entityVectorSimilarityThreshold`, `internal/store/retrieve.go`). Covers both "the corpus is empty/genuinely doesn't have this yet" (day-one case) and "we searched but only found weak, low-confidence matches." |
     | `full` | Stage 1 triggered stage 2, and at least one result cleared the threshold — an exact entity-key match (cheapest, most common) or a high-similarity vector hit. Entities are reachable both ways now: a query that literally names an entity finds it for free in stage 1, and a paraphrase that doesn't can still find it in stage 2 via its own embedding (`schema/0012_entity_embeddings.sql`) — previously entities were *only* findable by the stage-1 substring check, so a paraphrase missed them outright even when the fact was on record. This is the content that actually gets assembled into the context block. |

     Only `skipped` means "we didn't look." Both `partial` and `full` mean
     the search ran; they differ only in whether it found anything worth
     trusting. That distinction matters for [Retrieval observability](#retrieval-observability--evaluation):
     seeing `partial` with a query that *should* have matched something
     tells you the threshold or index needs tuning; seeing `skipped` for
     that same query tells you the stage-1 pre-check itself is too
     conservative — different bugs, different fixes.
   - On `partial`/`full`, whatever stage 2 assembles is capped to a
     configured token budget (e.g. 20% of the model's context window),
     ranked by `similarity * recency_decay * importance`.
   - **Per-request opt-out**: the client can pass a header/param (e.g.
     `X-Hupi-Memory: off`) to force `skipped` regardless of the gate — for
     testing prompts, or any turn you deliberately want run with a clean
     context.
3. **Context injection**: the retrieved block is prepended as a system/
   developer message ("Relevant memory about this user: ..."), the original
   conversation is left untouched after it.
4. **Provider Adapter** forwards the augmented request to the configured
   upstream endpoint (OpenAI, Azure, Anthropic-compat shim, local model —
   swapped purely by config, no code change).
5. **Response streams back to the client** as normal — the gateway also
   buffers the full text server-side as it streams.
6. **Capture**, synchronously, before the turn is considered done — see
   below. This is the piece that changed from the original sketch, which
   treated capture as async/fire-and-forget; that made data loss on a
   crash possible for no real benefit, since the operation it was avoiding
   blocking on (importance scoring) doesn't need a network call at all.

### Capture (durable and synchronous)

Capture writes the episode record to `episodes/YYYY/MM/YYYY-MM-DD.jsonl`
**before** the gateway considers the turn complete, and it is local-disk-only
— no network round trip, so it costs low-single-digit milliseconds, not
something worth making async:

- Importance is a **cheap local heuristic** (length, keyword/decision
  markers, question density) computed inline, not an LLM call. Deeper,
  LLM-based salience judgment is deferred to consolidation, where it's
  attached to the *summary*, not written back onto the episode (see
  [MEMORY_FORMAT.md § Episode record](docs/MEMORY_FORMAT.md#episode-record-episodesyyyymmyyyy-mm-ddjsonl)).
- The write is `fsync`'d before the response is marked complete on the
  gateway's side, so a crash immediately after can lose at most the
  in-flight request, never a turn the user already saw finish.
- For streamed responses, the gateway buffers the full assistant output as
  it forwards chunks to the client. If the stream is cut short (client
  disconnect, gateway restart mid-stream), the partial text is still
  captured with `"truncated": true` rather than discarded — a partial
  memory beats no memory.
- Net effect: capture is both *faster* than the original async design (no
  scheduling/queue overhead), and strictly more durable, because durability
  no longer depends on a background worker running before the next crash.
- **Per-request opt-out**: `X-Hupi-Capture: off` skips writing this turn to
  memory at all — a separate, orthogonal header from retrieval's
  `X-Hupi-Memory: off` above (one client request can set either, both, or
  neither). Added for clients whose turns are never worth remembering,
  e.g. a ghost-text-style inline completion firing on every debounced
  typing pause (`vscode-extension`'s `inlineCompletionProvider.ts`) — before
  this existed, capture ran unconditionally regardless of how trivial the
  turn was, so turning that feature on would flood memory with a durable
  row per keystroke pause. `hupi_capture_total`'s `result` label gains a
  third value, `skipped`, alongside `ok`/`error`.

## Retrieval observability & evaluation

RAG's classic failure mode is silent: wrong or missing retrieval doesn't
error, it just produces an answer that looks normal, so there's no signal
that memory didn't do its job. Two additions make this checkable instead of
invisible:

1. **Retrieval trace on every episode.** The `memory_gate` outcome and the
   specific `retrieved_summary_ids`/`retrieved_entity_ids`/
   `retrieved_episode_ids` actually injected are written onto the episode
   record at capture time (see
   [MEMORY_FORMAT.md § Episode record](docs/MEMORY_FORMAT.md)). This turns
   "did memory work on this turn" from an unanswerable question into a
   grep.
2. **Lightweight feedback capture.** A client (or you, reviewing later) can
   append a `type: "feedback"` record referencing an episode id with a
   rating like `memory_wrong` / `memory_missing` / `memory_correct`. This
   doesn't require building a UI — a CLI one-liner (`hupi feedback
   <episode_id> memory_wrong`) is enough to start accumulating signal.
3. **Scheduled self-check.** A small, fixed set of "memory probe" questions
   you write once (e.g. "what's my current project's working directory",
   "what did I decide about the vector index last month") gets re-run
   against the live system periodically (weekly cron), and a probe whose
   retrieved context no longer contains the expected fact is flagged. This
   is a regression test for the retrieval pipeline itself, not for the
   underlying LLM.

None of this makes retrieval quality perfect — it makes failures visible
and accumulable, which is the precondition for ever tuning the gate
thresholds or ranking weights instead of guessing.

## Consolidation engine (background, scheduled)

Runs independently of any live chat (nightly cron is enough):

1. **Daily rollup**: summarize today's episodes into `summaries/daily/`,
   extract/update entities touched, write per-fact citations into
   `key_facts`.
2. **Grounding check**: a second LLM call — `active_grounding_provider` if
   set in `providers.yaml`, otherwise the same profile as consolidation —
   checks each `key_facts` entry against its cited source text and flags
   any unsupported claim as `"grounded": false` before the summary is
   accepted as retrievable memory. Worth pointing at a smaller/cheaper
   model deliberately: this is a yes/no fact check per claim, not prose
   generation, so it doesn't need frontier-model quality to be effective.
3. **Weekly/monthly/yearly rollup**: summarize the *summaries* one level
   down, not the raw episodes — keeps this step O(number of summaries),
   not O(years of raw logs) — then grounding-check those too.
4. **Index rebuild**: re-embed anything new and upsert into the `pgvector`
   embedding column/table. If `active_embedding_provider` changes, run
   `hupi-reembed` for each affected scope — every summary/episode/entity
   whose `embedding_model` no longer matches the configured provider (or
   that was never embedded at all) gets picked up and re-embedded; see
   `internal/reembed`'s doc comment for why comparing vectors across
   models isn't safe and how this stays resumable without a persisted
   cursor.

Raw episodes are **not** pruned by this pipeline. Storage is cheap relative
to the value of being able to trace a summary claim back to what was
actually said; pruning is a separate, manual, opt-in operation if you ever
want it. This is the same LLM-provider-agnostic path as live chat — it just
calls the Provider Adapter directly instead of going through the retrieval
step.

## Consolidation integrity safeguards

The daily→weekly→monthly→yearly chain is repeated LLM summarization of
LLM summarization — each hop is a chance to compound a small fabrication
into an accepted "fact." Left alone, that failure is invisible: a
hallucinated claim retrieves and reads exactly like a true one. Three
things in the design push back on this (details in
[MEMORY_FORMAT.md § Grounding & correction](docs/MEMORY_FORMAT.md#grounding--correction)):

1. **Citations are per-fact, not per-summary.** Anything treated as
   retrievable memory (`key_facts`) must point at the episode ids (or,
   above daily level, the summary chain) it came from. Free-text prose
   summaries are for human skimming and aren't retrieved as fact.
2. **A grounding-check pass runs before a summary is trusted**, comparing
   generated facts against source text and excluding unsupported ones from
   retrieval without deleting them.
3. **Corrections are new records, never in-place edits.** If you spot a
   wrong "memory" later, the fix is a new summary version with `supersedes`
   and `correction_reason` set — the wrong version stays in the bundle as a
   record of what went wrong, which is also the raw material for improving
   the grounding check over time.

This costs real, recurring LLM calls (the grounding check roughly doubles
the number of consolidation calls) — accepted deliberately, because the
alternative is a memory system that can drift from truth with no way to
detect it.

**Caveat this doesn't cover**: re-consolidating a day (new episodes
arrived, so `hupi-consolidate` regenerates and supersedes that day's
current draft) tells the regeneration to treat the existing draft as an
already-established, possibly-already-corrected record, rather than
re-deriving it from raw episodes and risking exactly the kind of drift
this section exists to prevent — see [MEMORY_FORMAT.md § Grounding &
correction](docs/MEMORY_FORMAT.md#grounding--correction)'s operator
caveat for the trade-off that creates: trusting the existing record
protects a real correction from being re-litigated, but it also means
consolidation will never notice or repair a record that's already wrong.
That's still on you, via `hupi-correct`.

## Provider abstraction

A single config file lists provider profiles; the gateway and consolidation
engine both use it. There's no Go equivalent of LiteLLM with the same
breadth, but there doesn't need to be: an internal `Provider` interface
(`ChatCompletion`, `Embed`) with one adapter implementation covers OpenAI,
Azure OpenAI, Groq, OpenRouter, and local Ollama/vLLM in a single code path
since they all speak the same OpenAI-shaped request/response schema; a
second, small adapter handles Anthropic's native API shape. "Plug in any
endpoint" is still a config edit, not new code, for anything on the OpenAI
schema — a genuinely different schema needs its own adapter, which is a
one-time, bounded cost per vendor family rather than per endpoint.

```yaml
# providers.yaml
active_chat_provider: work-openai               # what you talk to day to day
active_consolidation_provider: personal-claude  # what writes your memory
active_grounding_provider: groq-fast            # optional — omit to reuse the consolidation profile
active_embedding_provider: local-ollama

providers:
  work-openai:
    kind: openai_compat   # selects the adapter (openai_compat | anthropic)
    vendor: openai        # recorded on every episode's provider_vendor column
    api_base: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    model: gpt-4.1

  personal-claude:
    kind: anthropic
    vendor: anthropic
    api_base: https://api.anthropic.com/v1
    api_key_env: ANTHROPIC_API_KEY
    api_version: "2023-06-01"
    model: claude-sonnet-5

  groq-fast:
    kind: openai_compat
    vendor: groq
    api_base: https://api.groq.com/openai/v1
    api_key_env: GROQ_API_KEY
    model: llama-3.1-8b-instant   # grounding is a yes/no fact check per claim, doesn't need a frontier model

  local-ollama:
    kind: openai_compat
    vendor: ollama
    api_base: http://localhost:11434/v1
    api_key_env: ""
    model: nomic-embed-text
```

Switching your daily driver model is changing `active_chat_provider` and
restarting the gateway — the memory bundle doesn't care what produced it.

`active_consolidation_provider` is deliberately a **separate, more stable**
setting. Consolidation is where summaries — and therefore the "voice" and
emphasis of your memory — actually get written; if it silently tracked
whatever model you happen to be chatting with today, switching your daily
driver for unrelated reasons would incidentally change how your memory
itself reads, with no record of why. Change it rarely and on purpose, and
when you do, append an entry to `manifest.json`'s
`consolidation_provider_history` (model, date, reason) so a future read of
"why do summaries from this period sound different" has an answer instead
of being a mystery.

`active_embedding_provider` has one more constraint the others don't:
every embedding column in the schema is a fixed `vector(1536)`, and
pgvector hard-rejects any other length outright. Rather than let a
model whose native output isn't 1536-dimensional fail at the first real
write, `bootstrap.VerifyEmbedding` (`internal/provider/dimensions.go`)
checks this once at startup for every entrypoint that actually embeds
(the gateway, `hupi-consolidate`, `hupi-reembed`, `hupi-correct`,
`hupi-selfcheck`), trying up to three things in order:

1. The model's native output, unmodified — plenty of models (`ada-002`
   among them) have no truncation knob at all and simply always produce
   one fixed size; if that's already 1536, nothing else is needed.
2. If native output is *shorter* than 1536 — true of most local/offline
   embedding models (`nomic-embed-text` at 768, `bge-large-en`/
   `mxbai-embed-large`/`gte-large`/`e5-large` around 1024) — zero-pad it
   up to 1536. This is exact, not an approximation: cosine similarity
   (what pgvector's `<=>` operator computes) is invariant to appending an
   equal-length run of zeros to both operands, since the padding
   contributes nothing to either vector's dot product or norm. It works
   regardless of how the model was trained, unlike truncating a too-long
   vector below.
3. If native output is *longer* than 1536, explicitly request 1536
   dimensions (OpenAI's `text-embedding-3-*` family supports this).
   Unlike padding, this is only attempted when the provider's API
   explicitly honors it — blindly slicing a longer vector without the
   model's cooperation would silently discard meaningful content unless
   it was specifically trained to keep its meaning front-loaded
   (Matryoshka representation learning), which this can't verify from
   the outside.

Native is tried before the explicit request, not after, because asking
for a specific dimension count is itself a hard error on some
models — verified directly against OpenAI's API: `ada-002` returns "This
model does not support specifying dimensions" the moment `dimensions` is
set at all, even to its own native size. If none of the three produces
1536 dimensions — a model longer than 1536 with no truncation option —
the process refuses to start rather than silently corrupting vector
search the moment something first tries to embed. After switching to a
model that does pass this check, run `hupi-reembed` per scope to bring
existing rows onto the new model (see `internal/reembed`'s doc comment).

## Storage security: two layers, neither owned by app-managed FUSE

The earlier sketch had the gateway mount a `gocryptfs` (FUSE) volume itself.
That doesn't survive contact with "ships as a Docker container": FUSE
inside a container needs `--cap-add SYS_ADMIN` + `--device /dev/fuse` (or
`--privileged`), which most platforms customers actually deploy to
(Kubernetes, managed container hosts) restrict or refuse outright. Revised
model, two independent layers:

**Layer 1 — disk-level, the deployment's responsibility, not the app's.**
The container's data volume (Postgres's data directory) is expected to sit
on encrypted storage the customer provides — a LUKS-encrypted host disk, an
encrypted managed disk/EBS volume, whatever their platform offers. This is
documented as a deployment requirement, the same way you'd require TLS
termination or a backup policy, not something HUPI implements. If the
customer's storage isn't encrypted, that's a deployment misconfiguration to
flag, not a gap HUPI tries to paper over by reintroducing app-managed FUSE.

**Layer 2 — application-level field encryption, inside the gateway, on top of whatever Layer 1 provides (defense in depth, not a replacement for it).**
- Sensitive text columns — `input_text`/`output_text` on episodes,
  `summary`/`key_facts[].fact` on summaries, `attributes` on entities — are
  encrypted with AES-256-GCM before being written to Postgres, and
  decrypted only in the gateway process after a read.
- Envelope key scheme: a per-deployment (per-tenant, for Tier 3) data
  encryption key (DEK) does the actual field encryption; the DEK itself is
  encrypted at rest by a key-encryption key (KEK) sourced from wherever the
  deployment keeps secrets — an env var for a simple self-host, a real
  secrets manager/KMS (Vault, AWS/GCP KMS) for an enterprise one. The
  gateway never persists the KEK itself, only reads it at startup.
- **The embedding vectors themselves are not encrypted.** Similarity search
  over encrypted vectors isn't practical with standard `pgvector` — you'd
  need specialized searchable/homomorphic encryption schemes that aren't
  worth the complexity here. This is a real, smaller residual risk (a raw
  embedding can leak partial semantic information about its source text —
  active research area, "embedding inversion") rather than a theoretical
  one; it's accepted because (a) Layer 1 already covers the whole database
  including the embedding column, and (b) reconstructing exact text from a
  vector is meaningfully harder than reading a plaintext column, even if
  not impossible.
- Losing the KEK is unrecoverable by design, same as the earlier
  passphrase warning — document a backup procedure for it (a real secrets
  manager makes this closer to a non-issue than a config file does).

## Resilience & blast-radius mitigation

Routing every AI interaction through one local process concentrates both
availability and exposure into a single point: if it's down you can't talk
to any configured model, and it's the one thing that ever holds the
decrypted bundle. That concentration is inherent to "one gateway, one
memory bundle plugged into anything" — it can be mitigated, not designed
away, without giving up the goal itself:

- **Direct-mode fallback.** Clients should keep the plain upstream
  `api_base`/`api_key` on hand as a documented fallback, not just the
  gateway's. If the gateway is down, you can point a client straight at
  OpenAI/Anthropic/etc. and keep working with no memory for that session,
  rather than being blocked outright. This is a deliberate escape hatch,
  not a silent one — it should be a conscious "run without memory today"
  choice.
- **Process supervision.** Run the gateway under a supervisor (a systemd
  user unit, or equivalent) with auto-restart, so most crashes cost seconds
  of downtime, not a manual restart you might not notice for hours.
- **Localhost-only by default.** The gateway binds to `127.0.0.1`, never a
  network-visible interface, unless you deliberately opt into exposing it
  (e.g. to reach it from a phone on the same LAN) — and if you do, that
  path requires its own auth token, separate from anything else in the
  system.
- **Minimize what's decrypted at once.** The gateway queries and decrypts
  individual Postgres rows on demand rather than loading the whole
  multi-year corpus into process memory — so a compromise of the running
  process exposes what a request actually touched, not automatically the
  entire history, even though the DB as a whole is reachable from that
  process's credentials.

These reduce the frequency and severity of the failure modes; they don't
eliminate the fact that this is, by design, a single-user, single-process
trust boundary. That's a property to be deliberate about, not a bug to
chase away.

## Portability workflow, end to end

Moving the live store to Postgres changes what "portable" means, and it's
worth being explicit that this is a real trade-off, not a free change: the
old model (copy one directory to a flash drive, done) is simpler than
anything involving a database. What replaces it:

1. **Day to day**, the bundle lives in the Postgres volume the deployment
   manages — encrypted at the disk level per Layer 1 above, with Layer 2
   field encryption on top. There's no separate "the bundle" directory
   anymore; Postgres is the live store for episodes, summaries, entities,
   *and* embeddings.
2. **Backup/migration** is a `pg_dump` (schema + encrypted column values;
   the KEK travels separately, same rule as before — losing it makes the
   dump useless, which is the point) — a DB-native backup, not a
   directory copy.
3. **HPMF becomes the interchange/bootstrap format**, not the live storage
   layer: `hupi export` produces the JSONL/JSON files described in
   [MEMORY_FORMAT.md](docs/MEMORY_FORMAT.md) as a portable, human-readable
   snapshot — for moving data into a fresh instance, for interoperating
   with something that isn't HUPI at all, or as a plaintext-once-decrypted
   archive you deliberately choose to carry outside the DB. It is not
   updated live and isn't what the running system reads day to day.
4. **On a new deployment**: stand up Postgres + `pgvector`, either restore
   a `pg_dump`, or `hupi import` an HPMF export (which rebuilds the vector
   index locally from summaries/entities, same rebuild-on-demand principle
   as before), point `providers.yaml` at whatever LLM/embedding endpoint is
   available there, start the gateway.
5. Point any OpenAI-compatible client at it. Full history is available
   immediately via summaries; raw-episode re-indexing (if importing from an
   HPMF snapshot) backfills in the background.

The "carry it on a flash drive between machines you personally own"
scenario from the original design is still possible via step 3/4, just no
longer the primary, always-current mechanism — that's the cost of becoming
a deployable product with a real concurrent-write, multi-record-type store
behind it instead of a single-user local file tree.

## Suggested initial tech stack

- **Gateway**: Go, single static binary, exposing `/v1/chat/completions`,
  `/v1/embeddings`. Ships as a minimal Docker image (`distroless`/`scratch`
  base) — no interpreter, no separate runtime.
- **Provider calls**: internal `Provider` interface, one adapter for the
  OpenAI-schema family (covers OpenAI, Azure OpenAI, Groq, OpenRouter,
  Ollama, vLLM), one adapter for Anthropic's native shape.
- **Storage & vector index**: PostgreSQL + `pgvector` — single backend for
  episodes, summaries, entities, and embeddings, for all three tiers (see
  Tiers below).
- **Consolidation scheduling**: OS cron, or this session's `schedule`
  skill / scheduled-tasks if you want it managed without a separate box.
- **Encryption at rest**: deployment-provided encrypted volume for the
  Postgres data directory (documented requirement, not app-managed) plus
  AES-256-GCM application-level field encryption inside the gateway (see
  Storage security above) — always on, not an export-time extra.

## Key tradeoffs to accept going in

- **Added latency per turn** from the retrieval step (mitigated by keeping
  the vector index small — summaries, not raw text — and running keyword/
  entity lookup alongside vector search). "Keyword" here is now real BM25
  scoring (`internal/store/bm25.go`, `keywordSearchSummaries`/
  `keywordSearchEpisodes`), not just substring matching — it catches an
  exact name/ID/acronym a dense embedding can dilute or miss entirely,
  at a real cost of its own: BM25 over application-encrypted text has no
  index to search with (Postgres's own full-text search can't see through
  ciphertext), so it decrypts and scores the whole scope's matching
  corpus per query rather than using an index the way vector search's
  HNSW index does. Fine at personal/team-history scale, the same
  trade-off `stage1EntityMatches` already makes elsewhere in this file.
- **Consolidation costs real LLM calls**, now roughly doubled by the
  grounding-check pass (daily/weekly/monthly/yearly summarization, each
  verified). At years of scale this is the dominant recurring cost of the
  system, not storage — accepted deliberately in exchange for being able to
  detect drifted/fabricated "memories" instead of silently trusting them.
- **Embedding-model lock-in on the vector column only** — solved by
  treating it as rebuildable, per the memory format doc, but a full
  reindex after switching embedding providers is a real, non-instant
  operation on a multi-year corpus.
- **Field encryption adds per-request encrypt/decrypt overhead** and a hard
  dependency on the KEK being reachable at startup — a config error there
  should fail loud (refuse to start), not silently fall back to storing
  plaintext.
- **The embedding vector column is not application-encrypted** (see
  Storage security) — a smaller, accepted residual risk, not an oversight.
- **Tier 1/2 are still single-user, high-trust-content systems.** No
  multi-tenant isolation there by design; only Tier 3 adds real
  access-control and scoping, see Tiers below.
- **Postgres as the live store is heavier to stand up than a single file
  tree**, and changes the portability story (see Portability workflow) —
  accepted in exchange for one consistent backend across all three tiers
  and real concurrent-write support once Tier 3 needs it.

## Tiers

Three tiers, one Postgres/`pgvector` backend throughout — the differences
are ops wrapping and, for Tier 3 only, a real schema/access extension, not
three separate products. Tiers 1/2 are also one Go binary, built entirely
from this repo; Tier 3 is a separate binary build requiring a separate,
commercially-licensed repo — see "Licensing and the open-core split"
below for why and how.

| Tier | Who | What's different from the base system |
|---|---|---|
| **1 — Personal** | One individual, self-hosted | Nothing architecturally — this is the system as described everywhere above: one user, one `self_model`, no scoping needed because there's only ever one owner. |
| **2 — Professional (Single)** | One individual, deployed/managed by an org | Same data model as Tier 1. Adds an ops layer on top: SSO instead of a local passphrase-equivalent, org-managed KEK (real KMS instead of an env var), org-set retention/audit policy. No multi-tenancy — still one person's memory. |
| **3 — Professional (Shared)** | A team, sharing memory | The real fork: every record gains a `scope` (`private:<user_id>` vs `shared:<team_id>`), retrieval is filtered by the requester's permissions, `self_model` stays per-user even here, but entities/summaries can be team-owned and are consolidated in a neutral team voice rather than any one person's. Workspace routing (below) decides scope per session instead of per message. Requires real authn/authz that Tiers 1/2 don't need — either `hupi-admin`-provisioned API keys, or (unlike Tier 2's ops-level SSO wrapper) genuine OpenID Connect authentication against an external IdP, with team membership sourced from the IdP itself — see [OIDC.md](docs/OIDC.md). |

Because storage was unified onto Postgres/`pgvector` for all tiers, Tier 3
turns out to be a smaller fork than it looked in earlier drafts of this
doc — it no longer needs a different storage engine, only an added `scope`
dimension, access control, and workspace routing on top of the same tables.

### Workspace routing (Tier 3)

Scope is decided by *which endpoint/session you're connected through*, not
by a per-message flag — the Slack-channel-vs-DM model, chosen specifically
because a per-message "was this private?" toggle is the kind of thing
people forget under normal use, and forgetting it here means a personal
disclosure becoming team-visible. Concretely: the gateway exposes a
personal context (`/v1/chat/completions`, scope defaults to
`private:<user_id>`) alongside a per-team context (e.g.
`/v1/team/<team_id>/chat/completions`, or an authenticated `workspace_id`),
and every episode captured in a session inherits that session's scope
automatically. Moving something from private to shared is a deliberate
action (switch workspace, or an explicit promote/share step on a specific
memory) — never an inferred one.

### Licensing and the open-core split

This repo (MIT) is Tier 1/2, complete and free forever. Tier 3 — real
end-user/team authentication, the `/v1/team/...` routes, team CLI
subcommands, and the team-voice consolidation prompt — lives in a
separate, commercially-licensed repo,
[hupi-t3](https://github.com/hupi-dev/hupi-t3), not included here.

Two things made this possible without forking the codebase:

1. **Most of what Tier 3 "added" turned out to be shared infrastructure,
   not Tier-3-only code.** `identity.Scope`, `dbscope`'s RLS session
   plumbing, per-scope encryption keys, and the scope-filtering inside
   `internal/store` are used identically by every tier — Tier 1 is just
   the one-scope case of the same machinery, not a special case bypassing
   it. Those stay in this repo because Tier 1/2 depend on them too;
   pulling them out would mean forking the schema and `internal/store`
   itself, which contradicts Tier 2 being "the unchanged Tier 1 data
   model."
2. **A small set of nil-by-default hook variables** mark the actual
   Tier-3-only extension points: `gateway.MountTeamRoutes`,
   `auth.NewTeamAuthenticator`, and a couple of same-package CLI/route
   hooks in `cmd/hupi-admin`/`cmd/hupi-admin-ui`. A plain build of this
   repo alone leaves every one of them nil — team routes never mount,
   `HUPI_REQUIRE_AUTH=true` fails with a clear error instead of silently
   granting Tier 1/2's no-auth behavior, and the team CLI subcommands
   report that they need the Enterprise build. hupi-t3's files set these
   via `init()` when present.

hupi-t3's own `build.sh` is what actually produces a Tier-3-capable
binary: it clones this repo, copies hupi-t3's files onto the exact same
relative paths (`internal/auth/team.go`, `internal/gateway/team.go`,
etc.), and builds from that combined tree. That physical-overlay step
isn't incidental — Go only allows a package's `internal/` directory to
be imported by code rooted at its parent, and that check is based on the
file tree on disk at build time, not module or repo boundaries. Copying
hupi-t3's files into this repo's checkout makes them genuine members of
their original packages for that purpose, while the two repos stay
separately licensed and access-controlled otherwise.

## Suggested build order

1. Postgres schema (episodes/summaries/entities/embeddings) + field
   encryption from day one — get real data accumulating immediately, and
   *encrypted from the first record*, since retrofitting encryption after
   months of plaintext history is exactly the gap we're avoiding. Tier 1
   shape only; no `scope` column yet.
2. Gateway (Go) with pass-through (no memory injection) to prove the
   OpenAI-compatible proxy works with your actual clients.
3. Add retrieval (start with keyword/entity only, no vectors) — cheapest
   way to validate the injection point actually helps.
4. Add vector search via `pgvector`.
5. Add the consolidation cron job (daily rollup + grounding check) once you
   have enough days of episodes for a daily rollup to be meaningful.
6. Add weekly/monthly/yearly rollups and the correction/supersedes flow.
7. Add HPMF export/import tooling — interchange format, not the live
   store, so this is packaging on top of a stable schema, not
   safety-critical logic.
8. Tier 2: SSO, KMS-backed KEK, org retention policy — ops wrapper on the
   unchanged Tier 1 data model.
9. Tier 3 last, deliberately: add the `scope` column, retrieval filtering
   by permission, workspace routing, and team-voice consolidation. This is
   the one step that's a real schema/access-model change, not packaging —
   sequence it after 1-7 are solid so it's extending a proven single-user
   system rather than being built simultaneously with it.
