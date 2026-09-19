import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setConfig, registerInlineCompletionItemProvider } from './test/vscode-mock';

vi.mock('./hupiClient', () => ({ createClient: vi.fn(), chat: vi.fn() }));
vi.mock('./config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./config')>();
  return { ...actual, loadConfig: vi.fn() };
});

import { createClient, chat } from './hupiClient';
import { loadConfig } from './config';
import { HupiInlineCompletionProvider, registerInlineCompletions } from './inlineCompletionProvider';

const fakeCfg = { baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'hupi_sk_test' };

beforeEach(() => {
  __resetVscodeMock();
  vi.mocked(loadConfig).mockReset().mockResolvedValue(fakeCfg);
  vi.mocked(createClient).mockReset().mockReturnValue({} as never);
  vi.mocked(chat).mockReset();
});

function fakeDocument(text: string, offset: number, languageId = 'typescript') {
  return {
    getText: () => text,
    offsetAt: () => offset,
    languageId,
  } as unknown as vscode.TextDocument;
}

function fakeCancellationToken() {
  const state = { cancelled: false };
  let handler: (() => void) | undefined;
  const token = {
    get isCancellationRequested() {
      return state.cancelled;
    },
    onCancellationRequested: (cb: () => void) => {
      handler = cb;
      return { dispose() {} };
    },
  } as unknown as vscode.CancellationToken;
  return {
    token,
    // Real VS Code flips isCancellationRequested and fires the callback
    // together — this fake does too, since the provider checks the flag
    // directly after the debounce/model-call race, not just the callback.
    cancel: () => {
      state.cancelled = true;
      handler?.();
    },
  };
}

function fakePosition() {
  return {} as unknown as vscode.Position;
}

describe('registerInlineCompletions', () => {
  it('registers a provider for all files', () => {
    const context = {} as unknown as vscode.ExtensionContext;
    registerInlineCompletions(context);
    expect(registerInlineCompletionItemProvider).toHaveBeenCalledWith(
      { pattern: '**' },
      expect.any(HupiInlineCompletionProvider),
    );
  });
});

describe('HupiInlineCompletionProvider', () => {
  it('does nothing when inline suggestions are disabled (the default)', async () => {
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();

    const result = await provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result).toBeUndefined();
    expect(loadConfig).not.toHaveBeenCalled();
    expect(chat).not.toHaveBeenCalled();
  });

  it('does not call the model when cancelled during the debounce', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 1000 });
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token, cancel } = fakeCancellationToken();

    const pending = provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );
    cancel();
    const result = await pending;

    expect(result).toBeUndefined();
    expect(chat).not.toHaveBeenCalled();
  });

  it('sends prefix/suffix built around the cursor and returns a cleaned suggestion', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    vi.mocked(chat).mockResolvedValue('```typescript\nreturn x + 1;\n```');
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();
    const text = 'function f() {\n  \n}';
    const offset = text.indexOf('  ') + 2;

    const result = await provider.provideInlineCompletionItems(
      fakeDocument(text, offset),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result).toHaveLength(1);
    expect(result![0].insertText).toBe('return x + 1;');
    const call = vi.mocked(chat).mock.calls[0][1];
    expect(call.messages[1].content).toContain(text.slice(0, offset));
    expect(call.messages[1].content).toContain(text.slice(offset));
    expect(call.maxTokens).toBe(256);
  });

  it('strips a leading echoed <CURSOR> marker from the response', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    vi.mocked(chat).mockResolvedValue('<CURSOR>\nconsole.log("hi");');
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();

    const result = await provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result![0].insertText).toBe('console.log("hi");');
  });

  it('returns undefined when the cleaned completion is empty', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    vi.mocked(chat).mockResolvedValue('   ');
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();

    const result = await provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result).toBeUndefined();
  });

  it('fails silently (no thrown error, no popup) when loadConfig rejects', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    vi.mocked(loadConfig).mockRejectedValue(new Error('providers.yaml missing'));
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();

    const result = await provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result).toBeUndefined();
    expect(chat).not.toHaveBeenCalled();
  });

  it('fails silently when the model request itself rejects', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    vi.mocked(chat).mockRejectedValue(new Error('upstream 502'));
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token } = fakeCancellationToken();

    const result = await provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );

    expect(result).toBeUndefined();
  });

  it('discards a completion that resolved after the request was cancelled', async () => {
    __setConfig({ 'inlineSuggestions.enabled': true, 'inlineSuggestions.debounceMs': 0 });
    let resolveChat!: (v: string) => void;
    // Only cancel once chat() has actually been called (i.e. past the
    // debounce), so this exercises the post-chat isCancellationRequested
    // check specifically — not the earlier during-debounce short-circuit
    // already covered by the test above.
    const chatCalled = new Promise<void>((resolveCalled) => {
      vi.mocked(chat).mockImplementation(() => {
        resolveCalled();
        return new Promise((resolve) => (resolveChat = resolve));
      });
    });
    const provider = new HupiInlineCompletionProvider({} as unknown as vscode.ExtensionContext);
    const { token, cancel } = fakeCancellationToken();

    const pending = provider.provideInlineCompletionItems(
      fakeDocument('const x = 1;', 12),
      fakePosition(),
      {} as vscode.InlineCompletionContext,
      token,
    );
    await chatCalled;
    cancel();
    resolveChat('some suggestion');
    const result = await pending;

    expect(result).toBeUndefined();
  });
});
