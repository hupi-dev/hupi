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

export interface StreamChatOptions {
  model: string;
  messages: ChatMessage[];
  onDelta: (text: string) => void;
  signal?: AbortSignal;
  /** "on" (free, retrieval-level "what was available") or "deep" (one
   *  extra real LLM call, generation-level "what was actually used" —
   *  see internal/gateway/attribution.go). Omit to get today's behavior
   *  (no citations at all). */
  explain?: 'on' | 'deep';
  /** Called at most once, after streaming completes, only when the
   *  server actually included citations (X-Hupi-Explain was set and the
   *  server does not just come back empty). */
  onCitations?: (citations: Citation[]) => void;
}

/**
 * Streams a chat completion, calling onDelta for each text chunk as it
 * arrives, and returns the fully-assembled response text once the stream
 * completes. Callers that don't need incremental rendering can just ignore
 * onDelta calls and use the return value.
 */
export async function streamChat(client: OpenAI, opts: StreamChatOptions): Promise<string> {
  const stream = await client.chat.completions.create(
    {
      model: opts.model,
      messages: opts.messages,
      stream: true,
    },
    {
      signal: opts.signal,
      headers: opts.explain ? { 'X-Hupi-Explain': opts.explain } : undefined,
    },
  );

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
  }
  return full;
}

export interface ChatOptions {
  model: string;
  messages: ChatMessage[];
  signal?: AbortSignal;
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
}

/** Non-streamed variant — used by multi-file edit (needs the whole
 *  response parsed at once anyway) and inline completions (a single short
 *  ghost-text suggestion, no incremental rendering to do). */
export async function chat(client: OpenAI, opts: ChatOptions): Promise<string> {
  const res = await client.chat.completions.create(
    {
      model: opts.model,
      messages: opts.messages,
      stream: false,
      max_tokens: opts.maxTokens,
      temperature: opts.temperature,
    },
    { signal: opts.signal, headers: opts.headers },
  );
  return res.choices[0]?.message?.content ?? '';
}
