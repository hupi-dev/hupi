import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setActiveTextEditor, createChatParticipant } from './test/vscode-mock';

vi.mock('./hupiClient', () => ({
  createClient: vi.fn(),
  streamChat: vi.fn(),
}));
vi.mock('./config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./config')>();
  return { ...actual, loadConfig: vi.fn() };
});
vi.mock('./oidcAuth', () => ({
  promptSignInRequired: vi.fn(),
}));

import { createClient, streamChat, type StreamChatOptions } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { registerChatParticipant } from './chatParticipant';

const fakeCfg = { baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'hupi_sk_test' };

// Same reasoning as chatViewProvider.test.ts's messageSnapshots: opts.messages
// must be captured at call time, not read back from .mock.calls afterward.
let messageSnapshots: { role: string; content: string }[][] = [];
function lastMessages(): { role: string; content: string }[] {
  return messageSnapshots[messageSnapshots.length - 1];
}
type StreamChatImpl = (client: never, opts: StreamChatOptions) => Promise<string>;

function mockStreamChat(impl: StreamChatImpl) {
  return vi.mocked(streamChat).mockImplementation((client, opts) => {
    messageSnapshots.push(opts.messages.map((m) => ({ role: m.role, content: m.content })));
    return impl(client as never, opts);
  });
}
function mockStreamChatOnce(impl: StreamChatImpl) {
  return vi.mocked(streamChat).mockImplementationOnce((client, opts) => {
    messageSnapshots.push(opts.messages.map((m) => ({ role: m.role, content: m.content })));
    return impl(client as never, opts);
  });
}

beforeEach(() => {
  __resetVscodeMock();
  messageSnapshots = [];
  vi.mocked(loadConfig).mockReset().mockResolvedValue(fakeCfg);
  vi.mocked(createClient).mockReset().mockReturnValue({} as never);
  vi.mocked(streamChat).mockReset();
  vi.mocked(promptSignInRequired).mockReset();
});

function fakeCancellationToken() {
  let handler: (() => void) | undefined;
  return {
    token: {
      isCancellationRequested: false,
      onCancellationRequested: (cb: () => void) => {
        handler = cb;
        return { dispose() {} };
      },
    } as unknown as vscode.CancellationToken,
    cancel: () => handler?.(),
  };
}

function fakeRequest(prompt: string): vscode.ChatRequest {
  return { prompt } as unknown as vscode.ChatRequest;
}

function fakeStream() {
  const chunks: string[] = [];
  return {
    stream: { markdown: (text: string) => chunks.push(text) } as unknown as vscode.ChatResponseStream,
    chunks,
  };
}

function registerAndGetHandler() {
  const context = { extensionUri: vscode.Uri.file('/fake') } as unknown as vscode.ExtensionContext;
  registerChatParticipant(context);
  const call = createChatParticipant.mock.calls[0];
  expect(call?.[0]).toBe('hupi.chat');
  return call![1] as (
    request: vscode.ChatRequest,
    chatContext: vscode.ChatContext,
    stream: vscode.ChatResponseStream,
    token: vscode.CancellationToken,
  ) => Promise<void>;
}

describe('registerChatParticipant', () => {
  it('registers under id hupi.chat', () => {
    registerAndGetHandler();
  });

  it('ignores an empty/whitespace-only prompt', async () => {
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('   '), { history: [] }, stream, token);

    expect(loadConfig).not.toHaveBeenCalled();
    expect(chunks).toEqual([]);
  });

  it('streams a successful reply as markdown deltas', async () => {
    mockStreamChat(async (_client, opts) => {
      opts.onDelta('Hel');
      opts.onDelta('lo');
      return 'Hello';
    });
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('hi there'), { history: [] }, stream, token);

    expect(chunks).toEqual(['Hel', 'lo']);
    expect(lastMessages()).toEqual([{ role: 'user', content: 'hi there' }]);
  });

  it('reconstructs prior turns from context.history', async () => {
    mockStreamChat(async () => 'second reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    // Real VS Code marks these constructors private — application code
    // only ever receives instances via context.history, never constructs
    // one — so building fake history turns for a test needs the same
    // `as any` bypass the mock's own doc comment already documents this
    // pattern for.
    const RequestTurn = vscode.ChatRequestTurn as unknown as new (
      prompt: string,
      command: string | undefined,
      references: unknown[],
      participant: string,
      toolReferences: unknown[],
    ) => vscode.ChatRequestTurn;
    const ResponseTurn = vscode.ChatResponseTurn as unknown as new (
      response: vscode.ChatResponseMarkdownPart[],
      result: unknown,
      participant: string,
    ) => vscode.ChatResponseTurn;
    const history = [
      new RequestTurn('first', undefined, [], 'hupi.chat', []),
      new ResponseTurn([new vscode.ChatResponseMarkdownPart('first reply')], {}, 'hupi.chat'),
    ];

    await handler(fakeRequest('second'), { history }, stream, token);

    expect(lastMessages()).toEqual([
      { role: 'user', content: 'first' },
      { role: 'assistant', content: 'first reply' },
      { role: 'user', content: 'second' },
    ]);
  });

  it('prompts sign-in and shows the message when loadConfig throws OidcSignInRequiredError', async () => {
    vi.mocked(loadConfig).mockRejectedValue(new OidcSignInRequiredError());
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(chunks).toEqual(['Sign in to HUPI (HUPI: Sign In) to continue.']);
    expect(promptSignInRequired).toHaveBeenCalledOnce();
    expect(streamChat).not.toHaveBeenCalled();
  });

  it('shows a generic config error without prompting sign-in', async () => {
    vi.mocked(loadConfig).mockRejectedValue(new Error('providers.yaml missing'));
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(chunks).toEqual(['Config error: providers.yaml missing']);
    expect(promptSignInRequired).not.toHaveBeenCalled();
  });

  it('shows the failure message when the request fails for a reason other than cancellation', async () => {
    mockStreamChatOnce(async () => {
      throw new Error('upstream 502');
    });
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('this one fails'), { history: [] }, stream, token);

    expect(chunks).toEqual([
      'HUPI request failed: upstream 502. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).',
    ]);
  });

  it('wires the cancellation token to the abort signal and shows no error on cancel', async () => {
    let capturedSignal: AbortSignal | undefined;
    mockStreamChatOnce(
      (_client, opts) =>
        new Promise((_resolve, reject) => {
          capturedSignal = opts.signal;
          opts.signal?.addEventListener('abort', () => reject(new Error('aborted')));
        }),
    );
    const handler = registerAndGetHandler();
    const { token, cancel } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    const pending = handler(fakeRequest('will be cancelled'), { history: [] }, stream, token);
    await vi.waitFor(() => expect(capturedSignal).toBeDefined());
    cancel();
    await pending;

    expect(capturedSignal?.aborted).toBe(true);
    expect(chunks).toEqual([]);
  });

  it('prefixes the prompt with file context when there is an active editor', async () => {
    __setActiveTextEditor({
      document: {
        getText: () => 'const x = 1;',
        languageId: 'typescript',
        uri: { path: 'src/index.ts' },
      },
      selection: { isEmpty: true },
    });
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('what does this do?'), { history: [] }, stream, token);

    const content = lastMessages()[0].content;
    expect(content).toContain('Context — visible file from');
    expect(content).toContain('const x = 1;');
    expect(content.endsWith('what does this do?')).toBe(true);
  });
});
