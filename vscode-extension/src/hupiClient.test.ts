import { describe, expect, it, vi } from 'vitest';
import { DEFAULT_REQUEST_TIMEOUT_MS, chat, resolveBaseUrl, streamChat, type ChatAttachment, type ChatMessage } from './hupiClient';

describe('resolveBaseUrl', () => {
  it('resolves the private route when no teamId is set', () => {
    expect(resolveBaseUrl({ baseUrl: 'http://localhost:8787' })).toBe('http://localhost:8787/v1');
  });

  it('strips a trailing slash from baseUrl', () => {
    expect(resolveBaseUrl({ baseUrl: 'http://localhost:8787/' })).toBe('http://localhost:8787/v1');
  });

  it('resolves the team route when teamId is set', () => {
    expect(resolveBaseUrl({ baseUrl: 'https://hupi.example.com', teamId: 'acme-eng' })).toBe(
      'https://hupi.example.com/v1/team/acme-eng',
    );
  });

  it('treats a whitespace-only teamId as unset', () => {
    expect(resolveBaseUrl({ baseUrl: 'http://localhost:8787', teamId: '   ' })).toBe(
      'http://localhost:8787/v1',
    );
  });

  it('URL-encodes a teamId with special characters', () => {
    expect(resolveBaseUrl({ baseUrl: 'http://localhost:8787', teamId: 'team/with slash' })).toBe(
      'http://localhost:8787/v1/team/team%2Fwith%20slash',
    );
  });
});

// streamChat is thin glue over the openai SDK's async iterator — these
// tests fake that iterator directly rather than mocking the SDK's HTTP
// layer, since the SDK's own request-building isn't this module's
// responsibility to re-test.
function fakeStreamClient(chunks: string[]): { chat: { completions: { create: ReturnType<typeof vi.fn> } } } {
  return {
    chat: {
      completions: {
        create: vi.fn().mockResolvedValue({
          [Symbol.asyncIterator]: async function* () {
            for (const c of chunks) {
              yield { choices: [{ delta: { content: c } }] };
            }
          },
        }),
      },
    },
  };
}

describe('streamChat', () => {
  it('accumulates deltas and returns the full text', async () => {
    const client = fakeStreamClient(['Hel', 'lo', ', world']);
    const deltas: string[] = [];
    const messages: ChatMessage[] = [{ role: 'user', content: 'hi' }];

    const full = await streamChat(client as any, {
      model: '',
      messages,
      onDelta: (d) => deltas.push(d),
    });

    expect(deltas).toEqual(['Hel', 'lo', ', world']);
    expect(full).toBe('Hello, world');
  });

  it('ignores chunks with no delta content', async () => {
    const client = fakeStreamClient(['a', '', 'b']);
    const deltas: string[] = [];

    const full = await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: (d) => deltas.push(d),
    });

    expect(deltas).toEqual(['a', 'b']);
    expect(full).toBe('ab');
  });

  it('sends X-Hupi-Explain when explain is set, and omits it otherwise', async () => {
    const create = vi.fn().mockResolvedValue({
      [Symbol.asyncIterator]: async function* () {
        yield { choices: [{ delta: { content: 'hi' } }] };
      },
    });
    const client = { chat: { completions: { create } } };

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      explain: 'deep',
    });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ headers: { 'X-Hupi-Explain': 'deep' } }));

    create.mockClear();
    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
    });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ headers: undefined }));
  });

  it('sends DEFAULT_REQUEST_TIMEOUT_MS when timeoutMs is omitted, or the override when set (finding B25)', async () => {
    const create = vi.fn().mockResolvedValue({
      [Symbol.asyncIterator]: async function* () {
        yield { choices: [{ delta: { content: 'hi' } }] };
      },
    });
    const client = { chat: { completions: { create } } };

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
    });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ timeout: DEFAULT_REQUEST_TIMEOUT_MS }));

    create.mockClear();
    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      timeoutMs: 15_000,
    });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ timeout: 15_000 }));
  });

  it('calls onCitations once with the terminal chunk’s hupi_citations, and not for chunks without it', async () => {
    const create = vi.fn().mockResolvedValue({
      [Symbol.asyncIterator]: async function* () {
        yield { choices: [{ delta: { content: 'Hel' } }] };
        yield { choices: [{ delta: { content: 'lo' } }] };
        yield {
          choices: [{ delta: {}, finish_reason: 'stop' }],
          hupi_citations: [{ ref: { kind: 'summary', scope: { kind: 'private', owner: 'user:1' }, id: 'sum_1' }, snippet: 'text', used: true }],
        };
      },
    });
    const client = { chat: { completions: { create } } };
    const seen: unknown[] = [];

    const full = await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      onCitations: (c) => seen.push(c),
    });

    expect(full).toBe('Hello');
    expect(seen).toHaveLength(1);
    expect(seen[0]).toEqual([{ ref: { kind: 'summary', scope: { kind: 'private', owner: 'user:1' }, id: 'sum_1' }, snippet: 'text', used: true }]);
  });

  it('never calls onCitations when the server never included hupi_citations', async () => {
    const client = fakeStreamClient(['a']);
    const onCitations = vi.fn();

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      onCitations,
    });

    expect(onCitations).not.toHaveBeenCalled();
  });

  it('sends attachments on the request body when provided, and omits the field otherwise', async () => {
    const create = vi.fn().mockResolvedValue({
      [Symbol.asyncIterator]: async function* () {
        yield { choices: [{ delta: { content: 'hi' } }] };
      },
    });
    const client = { chat: { completions: { create } } };
    const attachment: ChatAttachment = { type: 'document', filename: 'notes.txt', data: 'aGVsbG8=' };

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      attachments: [attachment],
    });
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ attachments: [attachment] }), expect.anything());

    create.mockClear();
    await streamChat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }], onDelta: () => {} });
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ attachments: undefined }), expect.anything());
  });

  it('calls onAttachmentWarnings once with the terminal chunk’s hupi_attachment_warnings, and not for chunks without it', async () => {
    const create = vi.fn().mockResolvedValue({
      [Symbol.asyncIterator]: async function* () {
        yield { choices: [{ delta: { content: 'Hel' } }] };
        yield {
          choices: [{ delta: {}, finish_reason: 'stop' }],
          hupi_attachment_warnings: ['scan.pdf: no extractable text found'],
        };
      },
    });
    const client = { chat: { completions: { create } } };
    const seen: string[][] = [];

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      onAttachmentWarnings: (w) => seen.push(w),
    });

    expect(seen).toEqual([['scan.pdf: no extractable text found']]);
  });

  it('never calls onAttachmentWarnings when the server never included hupi_attachment_warnings', async () => {
    const client = fakeStreamClient(['a']);
    const onAttachmentWarnings = vi.fn();

    await streamChat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      onDelta: () => {},
      onAttachmentWarnings,
    });

    expect(onAttachmentWarnings).not.toHaveBeenCalled();
  });
});

describe('chat', () => {
  it('returns the non-streamed response content and forwards maxTokens/temperature', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: { content: 'hello' } }] });
    const client = { chat: { completions: { create } } };

    const result = await chat(client as any, {
      model: 'gpt-test',
      messages: [{ role: 'user', content: 'hi' }],
      maxTokens: 256,
      temperature: 0.2,
    });

    expect(result).toBe('hello');
    expect(create).toHaveBeenCalledWith(
      expect.objectContaining({ model: 'gpt-test', stream: false, max_tokens: 256, temperature: 0.2 }),
      expect.anything(),
    );
  });

  it('forwards custom per-request headers (e.g. memory/capture opt-outs)', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: { content: 'hello' } }] });
    const client = { chat: { completions: { create } } };

    await chat(client as any, {
      model: '',
      messages: [{ role: 'user', content: 'hi' }],
      headers: { 'X-Hupi-Memory': 'off', 'X-Hupi-Capture': 'off' },
    });

    expect(create).toHaveBeenCalledWith(
      expect.anything(),
      expect.objectContaining({ headers: { 'X-Hupi-Memory': 'off', 'X-Hupi-Capture': 'off' } }),
    );
  });

  it('returns an empty string when the response has no message content', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: {} }] });
    const client = { chat: { completions: { create } } };

    const result = await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }] });

    expect(result).toBe('');
  });

  it('sends DEFAULT_REQUEST_TIMEOUT_MS when timeoutMs is omitted, or the override when set (finding B25)', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: { content: 'hello' } }] });
    const client = { chat: { completions: { create } } };

    await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }] });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ timeout: DEFAULT_REQUEST_TIMEOUT_MS }));

    create.mockClear();
    await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }], timeoutMs: 15_000 });
    expect(create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ timeout: 15_000 }));
  });

  it('sends attachments on the request body when provided, and omits the field otherwise', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: { content: 'hello' } }] });
    const client = { chat: { completions: { create } } };
    const attachment: ChatAttachment = { type: 'image', filename: 'photo.png', content_type: 'image/png', data: 'aGVsbG8=' };

    await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }], attachments: [attachment] });
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ attachments: [attachment] }), expect.anything());

    create.mockClear();
    await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }] });
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ attachments: undefined }), expect.anything());
  });
});
