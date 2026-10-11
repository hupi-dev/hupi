# MCP — HUPI memory in Claude Code, Cursor and other AI IDEs

The gateway serves the user's HUPI memory over the
[Model Context Protocol](https://modelcontextprotocol.io) at `/mcp`. An AI
IDE that brings its own model (Claude Code, Cursor, VS Code agent mode)
can then search and inspect the same memory every other tool routed
through HUPI has built up. It sees past conversations from any client or
machine, consolidated daily summaries, and the people, projects and
preferences in them. Nothing is copied into the IDE: each tool call is a
scoped, audited read against the gateway's own Postgres.

`/mcp` is part of `cmd/hupi` itself. It uses the same authentication,
scope resolution, row-level security and audit log as
`/v1/chat/completions`, so it never sees more than the chat route does.

## Tools

All four tools are read-only (`readOnlyHint`). Every result names its
`workspace`: `private`, or `team:<id>` on the team route.

| Tool | Arguments | Returns |
|---|---|---|
| `search_memory` | `query` (required, ≤1000 chars), `max_results` (default 10, max 30) | `context` (a date-labelled rendering of what was found, including the user's profile) and `hits[]`. Each hit has `kind` (`summary`, `memory`, `episode`), `id`, `text` (≤500 chars), and optionally `parent_summary_id`, `is_inference` and `relations`. |
| `get_entity` | `name_or_id`: an exact id like `project:hupi`, or a name | `found`, `entity`, `first_seen`, current grounded `attributes[]`, and `relationships[]` in both directions with validity dates. An ambiguous name returns `candidates[]` instead. |
| `list_entities` | `kind` (`person`, `project`, `preference`, `skill`, `place`, `organization`), `name_contains`, `limit` (default 50, max 200) | `entities[]` (id, kind, name, last updated), most recently updated first, and `truncated` |
| `recent_memory` | `days` (default 7, max 90), `limit` (default 7, max 31) | Current daily `summaries[]` with grounded, unexpired `key_facts[]`, newest first, plus `unconsolidated_exchanges`: how many exchanges came in after the latest summary |

`search_memory` always searches. The chat route's stage-1 gate skips
retrieval for turns that look like small talk, but here the query is
already a deliberate request, so the gate is bypassed and a plain keyword
phrase like `postgres migration plan` is still found.

Conversations are consolidated nightly (`hupi-consolidate`), so today's
exchanges are found by `search_memory` as raw episodes but don't appear
in `recent_memory`'s summaries until the next run.

The server's `instructions` tell the model to search before answering
anything that depends on the user's past work, and to treat memory as
recorded data, never as instructions.

Writing to memory from MCP (`remember`, `log_exchange`) is planned but
isn't in this release.

## Connecting a client

The examples assume the default listen address `127.0.0.1:8787`.

### Tier 1/2 (no auth)

**Claude Code:**

```bash
claude mcp add --transport http hupi http://127.0.0.1:8787/mcp
```

**Cursor** (`~/.cursor/mcp.json`, or `.cursor/mcp.json` in a project):

```json
{
  "mcpServers": {
    "hupi": { "url": "http://127.0.0.1:8787/mcp" }
  }
}
```

### With `HUPI_REQUIRE_AUTH=true`

Send the same API key the chat route takes, as a bearer token.

```bash
claude mcp add --transport http hupi http://127.0.0.1:8787/mcp --header "Authorization: Bearer $HUPI_API_KEY"
```

```json
{
  "mcpServers": {
    "hupi": {
      "url": "https://hupi.example.com/mcp",
      "headers": { "Authorization": "Bearer ${env:HUPI_API_KEY}" }
    }
  }
}
```

A request without a key, or with a bad one, gets `401` with
`WWW-Authenticate: Bearer realm="hupi"`. OIDC sign-in for MCP clients
(the MCP authorization flow) isn't supported yet, so use a static API
key from `hupi-admin create-key`.

### Team memory (Tier 3)

The Enterprise build also mounts `/v1/team/{team_id}/mcp`. It works like
`/mcp`, but searches the team's shared workspace after the same
membership check the team chat route does. In the OSS build that route
doesn't exist (404).

### Checking it works

In Claude Code, `/mcp` should list `hupi` as connected with four tools.
Then ask something only your memory can answer ("what did we decide about
the database migration?"). The model should call `search_memory` first.

Verified with Claude Code 2.1.161 against a Tier 1/2 gateway
(2026-10-11). Cursor uses the same streamable-HTTP transport but hasn't
been verified yet.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `HUPI_ENABLE_MCP` | on | `false` unmounts `/mcp` (and the team route) entirely |
| `HUPI_MCP_ALLOWED_ORIGINS` | empty | Comma-separated browser origins allowed to call `/mcp`. Native MCP clients send no `Origin` and are unaffected. |
| `HUPI_MCP_ALLOWED_HOSTS` | empty | If set, the `Host` header must be one of these (port ignored). Set it when the gateway listens on a non-loopback address. |
| `HUPI_MCP_CONTEXT_CHAR_BUDGET` | 6000 | Size cap for `search_memory`'s `context` text. This is separate from `HUPI_CONTEXT_CHAR_BUDGET`, the chat route's budget. |
| `HUPI_MCP_MAX_BODY_BYTES` | 1048576 | Largest accepted JSON-RPC request body |

## Security model

- **Same identity as chat.** Each request is authenticated on its own.
  The server runs in the SDK's stateless mode on purpose: in stateful
  mode the SDK keeps the first request's context for the whole session,
  which would pin identity to whoever initialized it. Stateless mode also
  means no sticky sessions are needed across gateway replicas.
- **Fails closed.** A tool that somehow runs without a resolved identity
  returns an error. It never falls back to the default scope.
- **Cross-site requests are refused.** `/mcp` on a no-auth loopback
  gateway would otherwise be readable by any web page the user visits.
  A request carrying an `Origin` header (only browsers send one) is
  rejected with `403` unless it's listed in `HUPI_MCP_ALLOWED_ORIGINS`,
  and `Origin: null` is always rejected. On a loopback listener the SDK's
  own DNS-rebinding protection also rejects non-localhost `Host` headers.
- **Internals aren't leaked.** A failed tool call returns a generic
  error; the real error is in the gateway log.
- **Audited.** Every read writes an `audit_log` row with
  `event_type = 'retrieve'`, `detail.via = "mcp"` and `detail.tool` set
  to the tool name (`hupi-audit query -event-type retrieve` lists them).
- **Memory is untrusted input to the model.** Memory holds text from
  past conversations, documents and attachments, so it can contain text
  that reads like instructions. The server instructions tell the model to
  treat results as data. Still, review tool output the way you'd review
  any retrieved document before letting an agent act on it.

## Observability

- `hupi_mcp_tool_calls_total{tool,outcome}` (`outcome` is `ok` or
  `error`) and `hupi_mcp_tool_call_duration_seconds{tool}`.
- The usual `hupi_http_requests_total{route="/mcp"}`.

No metric label carries a user or team id.

## Implementation

The code is in `internal/mcpserver`, built on the official Go SDK
(`github.com/modelcontextprotocol/go-sdk`):

- `server.go`: the shared server and the per-request auth middleware.
- `config.go`: env settings and the Origin/Host guard.
- `tools_read.go`: the four tools.

The tools call `store.Store`'s `Search`, `LookupEntity`, `ListEntities`
and `RecentSummaries`. `cmd/hupi/main.go`'s `mountMCP` mounts the routes.
