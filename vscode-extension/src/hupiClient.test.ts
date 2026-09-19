import { describe, expect, it, vi } from 'vitest';
import { chat, resolveBaseUrl, streamChat, type ChatMessage } from './hupiClient';

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

  it('returns an empty string when the response has no message content', async () => {
    const create = vi.fn().mockResolvedValue({ choices: [{ message: {} }] });
    const client = { chat: { completions: { create } } };

    const result = await chat(client as any, { model: '', messages: [{ role: 'user', content: 'hi' }] });

    expect(result).toBe('');
  });
});
