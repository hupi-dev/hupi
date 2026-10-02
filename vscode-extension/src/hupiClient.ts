// The HUPI gateway (cmd/hupi) is byte-for-byte OpenAI-compatible
// (internal/gateway/types.go) — this module is deliberately just the
// official `openai` SDK pointed at HUPI's baseURL, not a hand-rolled HTTP
// client. Kept free of any `vscode` import so it's unit-testable in plain
// Node (see hupiClient.test.ts) and reusable from smoke-test.ts.
import OpenAI from 'openai';

export interface HupiConfig {
  /** Root URL of the HUPI gateway, e.g. "http://localhost:8787" — no
   *  trailing slash, no "/v1" suffix (that's added here). */
  baseUrl: string;
  apiKey: string;
  /** Profile name from the deployment's providers.yaml. Empty string is
   *  valid and means "use HUPI's configured default chat provider" — see
   *  internal/gateway/handler.go's resolveProvider: an unmatched/empty
   *  model falls back to Registry.Chat(), it's not an error. */
  model: string;
  /** If set, routes through HUPI's team endpoint instead of the private
   *  one (internal/gateway/handler.go's HandleTeamChatCompletions). */
  teamId?: string;
}

export interface ChatMessage {
  role: 'system' | 'user' | 'assistant';
  content: string;
}

// Mirrors internal/gateway/types.go's chatAttachment exactly — additive,
// optional file/image content for a turn (internal/ingest extracts
// document text; a vision-capable provider captions an image), merged
// into the last user message's content server-side before capture. See
// the file-ingestion design doc: this is never forwarded to the real
// OpenAI/Anthropic API as-is, so it's a HUPI-only extension to the
// request body, the same way hupi_citations is a HUPI-only extension to
// the response.
export interface ChatAttachment {
  type: 'document' | 'image';
  filename?: string;
  content_type?: string;
  /** Base64-encoded raw bytes. */
  data: string;
}

// The OpenAI SDK's ChatCompletionCreateParams types are closed
// interfaces with no index signature, so HUPI's own additive
// `attachments` field (never part of the real OpenAI wire shape) needs
// a cast to write, the same way HupiCitationsExtension needs one to
// read — this is that cast's request-side counterpart.
interface HupiAttachmentsExtension {
  attachments?: ChatAttachment[];
}

// Mirrors internal/gateway/handler.go's Citation/identity.Ref exactly —
// one entry per memory (summary/entity/episode) that fed an answer, with
// the exact snippet that was injected into context for it. `used` is
// only ever set when X-Hupi-Explain: deep ran the extra attribution
// check (internal/gateway/attribution.go); undefined means "not
// checked," not "checked and found unused" — see docs/ANSWER_CITATIONS_PLAN.md.
export interface CitationRef {
  kind: string;
  scope: { kind: string; owner: string };
  id: string;
}

export interface Citation {
  ref: CitationRef;
  snippet: string;
  used?: boolean;
}

// The OpenAI SDK's ChatCompletionChunk/ChatCompletion response types are
// closed interfaces with no index signature, so HUPI's own additive
// hupi_citations field (never part of the real OpenAI wire shape) needs
// a cast to read rather than a typed property — this is that cast's one
// shared shape, used at both read sites below.
interface HupiCitationsExtension {
  hupi_citations?: Citation[];
}

// Same cast-to-read pattern as HupiCitationsExtension, for the other
// additive response field: a non-fatal note per attachment that didn't
// extract/caption cleanly (internal/gateway/types.go's
// chatCompletionResponse.AttachmentWarnings).
interface HupiAttachmentWarningsExtension {
  hupi_attachment_warnings?: string[];
}

/**
 * HUPI's private route is POST {root}/v1/chat/completions; its team route
 * is POST {root}/v1/team/{teamId}/chat/completions. The openai SDK always
 * POSTs to `${baseURL}/chat/completions`, so the two modes only differ in
 * which baseURL we hand it — no custom request path logic needed anywhere
 * else in this extension.
 */
export function resolveBaseUrl(cfg: Pick<HupiConfig, 'baseUrl' | 'teamId'>): string {
  const root = cfg.baseUrl.replace(/\/+$/, '');
  const teamId = cfg.teamId?.trim();
  if (teamId) {
    return `${root}/v1/team/${encodeURIComponent(teamId)}`;
  }
  return `${root}/v1`;
}

export function createClient(cfg: HupiConfig): OpenAI {
  return new OpenAI({
    baseURL: resolveBaseUrl(cfg),
    // The SDK requires a non-empty string even when the deployment has
    // HUPI_REQUIRE_AUTH unset (Tier 1/2, no auth needed) — "unused" is
    // never actually checked by HUPI in that mode, but keeps the SDK happy.
    apiKey: cfg.apiKey || 'unused',
  });
}

// docs/CODEBASE_SURVEY_AND_REVIEW.md finding B25: the four/five call
// sites into this module each wire a `signal` for *manual* cancellation
// (the user clicking Cancel, or a new request superseding an in-flight
// one), but none of that fires on its own if the gateway accepts the
// connection and then just hangs — the only ceiling was the openai SDK's
// own hardcoded 10-minute default (`timeout: 600000` in its core.js).
// This default gives every call site an app-chosen, scenario-appropriate
// deadline instead, still overridable per call via `timeoutMs`. The SDK
// itself races this against any caller-supplied `signal` (see its
// `fetchWithTimeout`), so manual cancellation and this timeout compose,
// they don't replace each other.
export const DEFAULT_REQUEST_TIMEOUT_MS = 120_000;

export interface StreamChatOptions {
  model: string;
  messages: ChatMessage[];
  onDelta: (text: string) => void;
  signal?: AbortSignal;
  /** Overrides DEFAULT_REQUEST_TIMEOUT_MS for this call. */
  timeoutMs?: number;
  /** "on" (free, retrieval-level "what was available") or "deep" (one
   *  extra real LLM call, generation-level "what was actually used" —
   *  see internal/gateway/attribution.go). Omit to get today's behavior
   *  (no citations at all). */
  explain?: 'on' | 'deep';
  /** Called at most once, after streaming completes, only when the
   *  server actually included citations (X-Hupi-Explain was set and the
   *  server does not just come back empty). */
  onCitations?: (citations: Citation[]) => void;
  /** File/image content for this turn — see ChatAttachment's own doc
   *  comment. Omitted/undefined is byte-for-byte unchanged behavior. */
  attachments?: ChatAttachment[];
  /** Called at most once, after streaming completes, only when at least
   *  one attachment didn't extract/caption cleanly (see
   *  internal/gateway/attachments.go) — e.g. a scanned/image-only PDF,
   *  or a vision provider timeout. Never called for a clean turn. */
  onAttachmentWarnings?: (warnings: string[]) => void;
}

/**
 * Streams a chat completion, calling onDelta for each text chunk as it
 * arrives, and returns the fully-assembled response text once the stream
 * completes. Callers that don't need incremental rendering can just ignore
 * onDelta calls and use the return value.
 */
export async function streamChat(client: OpenAI, opts: StreamChatOptions): Promise<string> {
  const body: OpenAI.ChatCompletionCreateParamsStreaming & HupiAttachmentsExtension = {
    model: opts.model,
    messages: opts.messages,
    stream: true,
    attachments: opts.attachments,
  };
  const stream = await client.chat.completions.create(body, {
    signal: opts.signal,
    timeout: opts.timeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
    headers: opts.explain ? { 'X-Hupi-Explain': opts.explain } : undefined,
  });

  let full = '';
  for await (const chunk of stream) {
    const delta = chunk.choices[0]?.delta?.content ?? '';
    if (delta) {
      full += delta;
      opts.onDelta(delta);
    }
    // Only ever set on the terminal chunk (see internal/gateway/handler.go's
    // handleStream) — every other chunk's cast just yields undefined.
    const citations = (chunk as unknown as HupiCitationsExtension).hupi_citations;
    if (citations && citations.length > 0) {
      opts.onCitations?.(citations);
    }
    const warnings = (chunk as unknown as HupiAttachmentWarningsExtension).hupi_attachment_warnings;
    if (warnings && warnings.length > 0) {
      opts.onAttachmentWarnings?.(warnings);
    }
  }
  return full;
}

export interface ChatOptions {
  model: string;
  messages: ChatMessage[];
  signal?: AbortSignal;
  /** Overrides DEFAULT_REQUEST_TIMEOUT_MS for this call — see that
   *  constant's doc comment (finding B25). */
  timeoutMs?: number;
  /** Caps response length — used by inline completions to keep ghost-text
   *  suggestions short and fast; omitted elsewhere (streamed chat/inline
   *  edit let the model finish naturally). */
  maxTokens?: number;
  temperature?: number;
  /** Extra headers for this request only — e.g. X-Hupi-Memory/
   *  X-Hupi-Capture: off (see internal/gateway/handler.go's per-request
   *  opt-outs, ARCHITECTURE.md § Capture) for a request that shouldn't be
   *  retrieved-from or written to memory at all. */
  headers?: Record<string, string>;
  /** File/image content for this turn — see ChatAttachment's own doc
   *  comment. Not used by today's callers (multi-file edit, inline
   *  completions), but plumbed through for API symmetry with
   *  StreamChatOptions. */
  attachments?: ChatAttachment[];
}

/** Non-streamed variant — used by multi-file edit (needs the whole
 *  response parsed at once anyway) and inline completions (a single short
 *  ghost-text suggestion, no incremental rendering to do). */
export async function chat(client: OpenAI, opts: ChatOptions): Promise<string> {
  const body: OpenAI.ChatCompletionCreateParamsNonStreaming & HupiAttachmentsExtension = {
    model: opts.model,
    messages: opts.messages,
    stream: false,
    max_tokens: opts.maxTokens,
    temperature: opts.temperature,
    attachments: opts.attachments,
  };
  const res = await client.chat.completions.create(body, {
    signal: opts.signal,
    timeout: opts.timeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS,
    headers: opts.headers,
  });
  return res.choices[0]?.message?.content ?? '';
}
