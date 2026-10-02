# API Reference — Routes, Triggers, and Call Chains

The gateway (`cmd/hupi`) exposes exactly two memory/chat routes in this
OSS build, plus two Kubernetes probe endpoints that carry no application
logic. This document covers each of the two in full: when it's
triggered, what it expects and returns, every status code it can
produce, and the complete function-by-function call chain from the HTTP
layer down to Postgres and back — including every external network call
(LLM/embedding providers) along the way.

Both are registered in `cmd/hupi/main.go`:

```go
mux.HandleFunc("POST /v1/chat/completions", handler.HandleChatCompletions)
mux.HandleFunc("POST /v1/feedback",         handler.HandleFeedback)
```

No other methods are accepted on either path (`405 Method Not Allowed`
otherwise), and there is no other *memory-bearing* route in this OSS
build — the gateway is deliberately a narrow surface.

**Team-scoped routes aren't part of this OSS build.** `cmd/hupi` also
calls `gateway.MountTeamRoutes(mux, handler)` unconditionally right
after mounting the two routes above (`internal/gateway/handler.go`'s own
doc comment on that var) — a nil-by-default hook, only set by `team.go`'s
`init()` when the closed-source Tier-3 extension is linked in. `nil`
means "no team routes to mount," the correct and only state in this
repo, not an error. The underlying scope/identity machinery a team route
would use (`identity.ScopeKindShared`, `Identity.HasTeam`, `auth.Store`'s
`team_members` query) does exist in this repo, but nothing in the two
routes below ever exercises it: `resolveScope` (`handler.go`) always
resolves both `actingUser` and `workspace` to the caller's own private
scope here, team membership or not — only Tier-3's team routes would
ever call the shared internals below with `actingUser != workspace`. The
`POST /v1/team/{team_id}/...` HTTP handlers themselves
(`HandleTeamChatCompletions`, `resolveTeamScope`, `HandleTeamFeedback`)
live only in the Tier-3 extension, not in this repository, so this
document doesn't cover them. See `docs/TIER3_PLAN.md` for what Tier 3
adds.

The only other two routes are Kubernetes health probes, added for
containerized deployment (`docs/GAP_CLOSURE_PLAN.md` §5) — neither
touches the memory pipeline:

```go
mux.HandleFunc("GET /healthz", handleLiveness)          // always 200, no dependencies
mux.HandleFunc("GET /readyz",  handleReadiness(deps.DB)) // 200 if db.PingContext succeeds within 2s, else 503
```

`/healthz` is what a Kubernetes liveness probe should point at (a
container that can't even answer this should be restarted);
`/readyz` is what a readiness probe should point at (a container that's
up but can't reach Postgres shouldn't receive traffic yet, but also
shouldn't be restarted for a transient DB blip).

This document only covers the gateway (`cmd/hupi`) — the memory/chat
surface end users' API keys reach. `cmd/hupi-admin-ui` is a second,
separate HTTP surface, a JSON API for operator provisioning
(users/teams/API keys/operator credentials) and audit-log queries, bound
to `127.0.0.1` by default and authenticated with named operator
credentials rather than end-user API keys. It never exposes memory
content (episodes/summaries/entities) — see
[ADMIN_UI.md](ADMIN_UI.md) for its full route list and auth model.

---

## `POST /v1/chat/completions`

The private-scope chat endpoint — an OpenAI-compatible drop-in. Any
existing client pointed at this `base_url` works unmodified.

**Auth**: optional, depending on deployment. If `HUPI_REQUIRE_AUTH=true`
was set when `cmd/hupi` started, requires `Authorization: Bearer <api key>`.
Otherwise every request resolves to `identity.DefaultUserID` — no header
needed (Tier 1/2 mode).

**Request body** (`chatCompletionRequest`, `internal/gateway/types.go`):

```json
{
  "model": "gpt-4.1",
  "messages": [{"role": "user", "content": "..."}],
  "stream": false,
  "temperature": 0.7,
  "max_tokens": 1024,
  "attachments": [
    {"type": "document", "filename": "resume.pdf", "content_type": "application/pdf", "data": "<base64>"},
    {"type": "image", "filename": "vacation.jpg", "content_type": "image/jpeg", "data": "<base64>"}
  ]
}
```

`attachments` is optional and additive — absent/`nil` is byte-for-byte
unchanged behavior for every existing client. Each entry is `type:
"document"|"image"`, raw bytes base64-encoded in `data`. A `document`
(`.txt`/`.docx`/`.pdf`, sniffed from magic bytes, not from
`filename`/`content_type`) is extracted to plain text; an `image`
(`content_type` must be one of `image/png`, `image/jpeg`, `image/webp`,
`image/gif`) is captioned by the configured vision provider. Both are
merged, with a provenance marker, into the last user message's content
before retrieval's query is built — see ARCHITECTURE.md § Attachments.
Capped at 5 attachments per request and 8 MiB per attachment (checked
after base64 decode, before any parsing/upstream call).

**Optional header**: `X-Hupi-Memory: off` — bypasses retrieval entirely
for this one request (not even the anchor is injected).

**Response** (non-streaming, `chatCompletionResponse`):

```json
{
  "id": "ep_...",
  "object": "chat.completion",
  "created": 1234567890,
  "model": "gpt-4.1",
  "choices": [{"index": 0, "message": {"role": "assistant", "content": "..."}, "finish_reason": "stop"}],
  "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
  "hupi_attachment_warnings": ["resume.pdf: no extractable text (scanned/image-only PDF?)"]
}
```

`hupi_attachment_warnings` is only present when at least one attachment
degraded (failed to extract/caption) rather than failing the whole
request — same additive pattern as `hupi_citations`
(`X-Hupi-Explain`). On a streamed response it appears only on the
terminal chunk.

If `"stream": true`, the response is `text/event-stream` — standard
OpenAI-shaped `data: {...}` chunks, terminated by `data: [DONE]`.

**Status codes**:

| Code | When |
|---|---|
| 200 | Success (streaming or not) |
| 400 | Malformed JSON body, `messages` is empty, an invalid message `role`, or a malformed `attachments` entry (more than 5, over 8 MiB, bad base64, disallowed image MIME, or attachments present with no user message to attach them to) |
| 401 | `HUPI_REQUIRE_AUTH=true` and the `Authorization` header is missing or the key is invalid |
| 405 | Any method other than POST |
| 413 | Request body exceeds the configured size limit (`HUPI_MAX_REQUEST_BODY_BYTES`, default 20 MiB) |
| 500 | The response writer doesn't support streaming (should not happen in practice — `net/http`'s default writer does) |
| 502 | The upstream LLM provider call itself failed |

**Call chain**:

1. `gateway.Handler.HandleChatCompletions` (`internal/gateway/handler.go:247`) — method check, then `h.resolveScope(r)`.
2. `resolveScope` (`handler.go:410`) -> `resolveIdentity` (`handler.go:385`):
   - `h.Auth == nil` -> `identity.Identity{UserID: identity.DefaultUserID}`.
   - else: `bearerToken(r)` extracts the header; `h.Auth.Resolve(ctx, token)` -> `auth.Store.Resolve` (`internal/auth/auth.go:59`) — `SELECT user_id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL`, then `SELECT team_id FROM team_members WHERE user_id = $1`.
   - `resolveScope` returns `identity.PrivateScope()` — both `actingUser` and `workspace` are this same value for the private route.
3. `handleChatCompletionsScoped(w, r, scope, scope)` (`handler.go:261`):
   1. Wraps `r.Body` in `http.MaxBytesReader(w, r.Body, maxRequestBodyBytes())` (`handler.go:31`) before decoding — an oversized body maps to `413`, distinct from the generic `400` a decode error otherwise gets.
   2. Decode JSON into `chatCompletionRequest`; validate `messages` non-empty and every message's `role` is valid (`provider.Role.Valid()`).
   3. Unless `X-Hupi-Memory: off`: `h.Retriever.Retrieve(ctx, actingUser, workspace, messages)` — the concrete implementation is `store.Store.Retrieve` (`internal/store/retrieve.go:59`) — **run against the original, unmerged messages**, so a turn's retrieval query doesn't balloon with a document's full extracted text:
      - `dbscope.Run(ctx, s.db, actingUser, workspace, fn)` (`internal/dbscope/dbscope.go`) — opens a transaction, sets 4 RLS session variables.
      - `buildAnchor(ctx, tx, actingUser, workspace)` — `SELECT` the `self_model` entity scoped to `actingUser` (decrypted via `s.keys.GetOrCreate(ctx, actingUser)` -> `crypto.KeyStore.GetOrCreate`, `internal/crypto/keystore.go`), and the latest `daily` summary's period scoped to `workspace`.
      - If the message is non-empty: `stage1EntityMatches` (`retrieve.go:293`) — a `SELECT` over `entities` scoped to `workspace`, matched by substring against the message text.
      - For each match: `formatEntity` (`retrieve.go:340`) — point `SELECT` + decrypt.
      - Stage 2 also runs `vectorSearchEntities` (`retrieve.go:440`) against the same embedded query used for summaries/episodes below — a second chance for an entity whose name/slug isn't a literal substring of the message (`schema/0012_entity_embeddings.sql`), excluding whatever `stage1EntityMatches` already found so nothing is rendered twice.
      - *(transaction commits here)*
      - If stage 1 found nothing at all: return `Gate: skipped`.
      - Else: `s.embedder.Embed(ctx, ...)` — **external network call** to `active_embedding_provider`, via whichever adapter (`provider.OpenAICompat.Embed` or `provider.Anthropic.Embed` — the latter always errors, Anthropic has no embeddings endpoint) the registry resolves.
      - A second `dbscope.Run(ctx, s.db, workspace, workspace, fn)`: `vectorSearchSummaries` and `vectorSearchEpisodes` (`retrieve.go:295`, `:347`) — `pgvector` cosine-distance queries, each decrypting matches above the similarity threshold.
      - This is `Store.retrieve` — the exported `Store.Retrieve` wrapping it then opens a third, separate `dbscope.Run(actingUser, workspace, fn)` to `audit.Write` a `retrieve` event (`event_type`, `actor = actingUser.Owner`, gate + ref count in `detail`) — best-effort, a failed audit write logs but doesn't fail the turn (docs/GAP_CLOSURE_PLAN.md §4.3).
      - Returns `gateway.RetrievalResult{Gate, ContextMessage, Refs}`.
   4. If `ContextMessage` is non-empty, prepend it as a `system`-role message.
   5. If `req.Attachments` is non-empty: `h.mergeAttachments(ctx, messages, req.Attachments)` (`internal/gateway/attachments.go`) — validates every attachment first (type, base64, size cap, image MIME allowlist; any failure here is the `400` above), then processes each concurrently:
      - `document` -> `ingest.Extract` (`internal/ingest/ingest.go`) — sniffs magic bytes (`%PDF-`, a zip signature with a `word/document.xml` entry, else plain text/UTF-8 repair), dispatches to the matching extractor, under a 10s timeout with panic recovery (one malformed attachment can't crash the shared gateway process).
      - `image` -> `h.Registry.Vision().(provider.VisionCapable).DescribeImage(ctx, provider.ImageInput{...}, instruction)` — **external network call** to `active_vision_provider` (falls back to `active_chat_provider` if unset), under a 20s timeout.
      - Each result is appended to the **last user message's** content with a provenance marker (`[Attached file: resume.pdf]...[End of attached file: resume.pdf]` / `[Shared image: vacation.jpg]...`); a per-attachment extraction/captioning failure degrades to a placeholder marker plus an entry in the returned warnings list, never a failed request.
      - Because this mutates the same `[]provider.Message` already threaded through the rest of the request, every step below (capture, embedding, consolidation) needs no further changes — file/image content is just more characters in the one captured input string.
   6. `resolveProvider(req.Model)` (`handler.go:434`) — `h.Registry.Named(model)` if it matches a configured profile name, else `h.Registry.Chat()` (`provider.Registry`, `internal/provider/registry.go`).
   7. If `req.Stream`: `handleStream(...)` (`handler.go:598`), else `handleNonStream(...)` (`handler.go:504`).
4. **`handleNonStream`**:
   1. `target.ChatCompletion(ctx, provider.ChatRequest{...})` — **external network call** to the vendor API (`provider.OpenAICompat.ChatCompletion` or `provider.Anthropic.ChatCompletion`).
   2. `buildEpisode(...)` (`handler.go:723`) — computes `estimateImportance` (`internal/gateway/importance.go`), `hashText` (`internal/gateway/hash.go`), a new id via `newEpisodeID` (`internal/gateway/id.go`) — against the attachment-merged message content, so the captured episode (and everything consolidation later derives from it) includes the extracted/captioned text.
   3. `h.Capturer.Capture(ctx, scope, ep)` -> `store.Store.Capture` (`internal/store/capture.go:22`): resolves the scope's encryptor, encrypts `input_text`/`output_text`, `dbscope.Run(scope, scope)` -> `INSERT INTO episodes (...)`, then (same transaction) `audit.Write` a `capture` event — `actor = ep.ActorUserID` (the individual team member for a team-routed request, distinct from `scope` which is the team's shared workspace — see `handleChatCompletionsScoped` passing `actingUser.Owner` down as `actor`), `target_ref` = the new episode. **A capture failure here is logged, not surfaced to the client** — the answer still returns.
   4. Encode and write the JSON response, including `hupi_attachment_warnings` if any attachment degraded.
5. **`handleStream`** (same steps 1-3, different transport): forwards each delta to the client via SSE as it arrives; buffers the full text; on stream end (or client disconnect / provider error, flagged as `truncated`), calls `Capture` the same way as above — the terminal `data: [DONE]` is deliberately held back until `Capture` returns, so "the turn is done" is signaled to the client only after the write attempt has happened, not before. `hupi_attachment_warnings`, if any, rides on that terminal chunk.

---

## `POST /v1/feedback`

Records a signal that a specific past episode's memory was right, wrong,
or missing — the mechanism ARCHITECTURE.md calls "the cheapest possible
signal channel" for retrieval quality.

**Auth**: same as `/v1/chat/completions` (optional, depending on
`HUPI_REQUIRE_AUTH`).

**Request body** (`feedbackRequest`):

```json
{
  "episode_id": "ep_...",
  "rating": "memory_wrong",
  "note": "optional free text"
}
```

`rating` must be exactly one of `memory_correct`, `memory_wrong`,
`memory_missing` (`gateway.FeedbackRating`, `handler.go`).

**Response** (`feedbackResponse`, HTTP 201):

```json
{"id": "ep_..."}
```

(the id of the **new** feedback episode row, not the episode being rated)

**Status codes**:

| Code | When |
|---|---|
| 201 | Feedback recorded |
| 400 | Malformed JSON, missing `episode_id`, or an invalid `rating` value |
| 401 | Auth required and missing/invalid |
| 405 | Non-POST |
| 500 | The write to Postgres failed |

Note the difference from chat capture: **a failed write here is a hard
error to the client (500), not logged-and-ignored.** Persisting feedback
*is* the entire point of the request — there's no "answer" to protect the
user from losing, unlike a chat turn.

**Call chain**:

1. `HandleFeedback` (`handler.go:322`) -> `resolveScope(r)` (401 on failure) -> `handleFeedbackScoped(w, r, scope)` (`handler.go:353`).
2. Decode `feedbackRequest`; validate `episode_id` non-empty and `rating` is one of the three valid values.
3. Build a `gateway.Episode{Type: "feedback", RefersTo, Rating, Note}` (the rest of the struct's fields are zero-valued — same table, different row shape, per `MEMORY_FORMAT.md`).
4. `h.Capturer.Capture(ctx, scope, ep)` -> `store.Store.Capture` — same function as chat capture, branching internally on `ep.Type == "feedback"` to also encrypt `Note` and populate `refers_to`/`rating` instead of `memory_gate`/`retrieved_refs`.
5. On success: `201` with the new episode's id.

---

## Summary table

| Route | Auth | Success | Client errors | Server/upstream errors |
|---|---|---|---|---|
| `POST /v1/chat/completions` | optional | 200 | 400, 401, 413 | 500, 502 |
| `POST /v1/feedback` | optional | 201 | 400, 401 | 500 |

(Team-scoped routes aren't part of this OSS build — see the note at the
top of this document.)

## External calls made per route

Every route can make 0-4 outbound network calls, none of them wrapped in
an open database transaction (`docs/HARDENING_PLAN.md` D3):

| Call | Made when | Provider role |
|---|---|---|
| Image captioning | Once per image attachment, concurrently | `active_vision_provider` (falls back to `active_chat_provider` if unset) |
| Embedding | Stage-1 pre-check found any signal at all | `active_embedding_provider` |
| Chat completion | Always (the point of the request) | `active_chat_provider`, or a named profile if `model` matched one |
| — | (Consolidation's LLM/grounding calls happen entirely outside the HTTP path — see `docs/CODE_GUIDE.md §5`) | |

(Document attachments — `.txt`/`.docx`/`.pdf` — make no network call at
all; extraction via `internal/ingest` is pure local parsing.)

## Non-HTTP triggers

Everything that isn't an HTTP route — the cron jobs and CLIs — is covered
in [CODE_GUIDE.md §5](CODE_GUIDE.md#5-non-http-entry-points-call-chain-by-call-chain).
