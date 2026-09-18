import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setActiveTextEditor } from './test/vscode-mock';

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
import { HupiChatViewProvider } from './chatViewProvider';

type ToWebview =
  | { type: 'userEcho'; text: string }
  | { type: 'delta'; text: string }
  | { type: 'done' }
  | { type: 'error'; message: string };

function fakeWebviewView() {
  const posted: ToWebview[] = [];
  let receiveHandler: ((message: unknown) => Promise<void>) | undefined;
  const webview = {
    options: undefined as unknown,
    html: '',
    cspSource: 'fake-csp',
    postMessage: async (msg: ToWebview) => {
      posted.push(msg);
      return true;
    },
    onDidReceiveMessage: (cb: (message: unknown) => Promise<void>) => {
      receiveHandler = cb;
      return { dispose() {} };
    },
    asWebviewUri: (uri: unknown) => uri,
  };
  return {
    webviewView: { webview } as unknown as vscode.WebviewView,
    posted,
    send: (message: unknown) => receiveHandler!(message),
  };
}

const fakeCfg = { baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'hupi_sk_test' };

// opts.messages passed to streamChat is a *live reference* to the
// provider's internal history array, which the source mutates (pushes
// the assistant's reply) immediately after streamChat resolves — reading
// `.mock.calls` after the fact would see that later mutation, not the
// state at call time. Every mocked implementation below snapshots a copy
// up front instead.
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

function makeProvider() {
  const context = { extensionUri: { path: '/fake' } } as unknown as vscode.ExtensionContext;
  const provider = new HupiChatViewProvider(context, context.extensionUri as unknown as vscode.Uri);
  const { webviewView, posted, send } = fakeWebviewView();
  provider.resolveWebviewView(webviewView);
  return { posted, send };
}

describe('HupiChatViewProvider', () => {
  it('ignores an empty/whitespace-only message', async () => {
    const { posted, send } = makeProvider();
    await send({ type: 'send', text: '   ' });
    expect(loadConfig).not.toHaveBeenCalled();
    expect(posted).toEqual([]);
  });

  it('streams a successful reply and reports done', async () => {
    mockStreamChat(async (_client, opts) => {
      opts.onDelta('Hel');
      opts.onDelta('lo');
      return 'Hello';
    });
    const { posted, send } = makeProvider();

    await send({ type: 'send', text: 'hi there' });

    expect(posted).toEqual([{ type: 'delta', text: 'Hel' }, { type: 'delta', text: 'lo' }, { type: 'done' }]);
    expect(lastMessages()).toEqual([{ role: 'user', content: 'hi there' }]);
  });

  it('carries prior turns forward on a second send', async () => {
    mockStreamChat(async () => 'first reply');
    const { send } = makeProvider();
    await send({ type: 'send', text: 'first' });

    mockStreamChat(async () => 'second reply');
    await send({ type: 'send', text: 'second' });

    expect(lastMessages()).toEqual([
      { role: 'user', content: 'first' },
      { role: 'assistant', content: 'first reply' },
      { role: 'user', content: 'second' },
    ]);
  });

  it('prompts sign-in and reports the error when loadConfig throws OidcSignInRequiredError', async () => {
    vi.mocked(loadConfig).mockRejectedValue(new OidcSignInRequiredError());
    const { posted, send } = makeProvider();

    await send({ type: 'send', text: 'hi' });

    expect(posted).toEqual([{ type: 'error', message: 'Sign in to HUPI (HUPI: Sign In) to continue.' }]);
    expect(promptSignInRequired).toHaveBeenCalledOnce();
    expect(streamChat).not.toHaveBeenCalled();
  });

  it('reports a generic config error without prompting sign-in', async () => {
    vi.mocked(loadConfig).mockRejectedValue(new Error('providers.yaml missing'));
    const { posted, send } = makeProvider();

    await send({ type: 'send', text: 'hi' });

    expect(posted).toEqual([{ type: 'error', message: 'Config error: providers.yaml missing' }]);
    expect(promptSignInRequired).not.toHaveBeenCalled();
  });

  it('rolls back the user turn and reports the failure when the request fails', async () => {
    mockStreamChatOnce(async () => {
      throw new Error('upstream 502');
    });
    const { posted, send } = makeProvider();
    await send({ type: 'send', text: 'this one fails' });

    expect(posted).toEqual([
      {
        type: 'error',
        message: 'HUPI request failed: upstream 502. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).',
      },
    ]);

    // Rolled back: the next send's history starts fresh, not carrying the
    // failed turn forward.
    mockStreamChatOnce(async () => 'ok now');
    await send({ type: 'send', text: 'retry' });
    expect(lastMessages()).toEqual([{ role: 'user', content: 'retry' }]);
  });

  it('does not report an error for a deliberately superseded (aborted) send', async () => {
    let capturedSignal: AbortSignal | undefined;
    mockStreamChatOnce((_client, opts) => {
      capturedSignal = opts.signal;
      return new Promise((_resolve, reject) => {
        opts.signal?.addEventListener('abort', () => reject(new Error('aborted')));
      });
    });
    const { posted, send } = makeProvider();

    const firstSend = send({ type: 'send', text: 'first, will be superseded' });
    // handleSend is async — it doesn't reach `this.inFlight = controller`
    // (and therefore start actually listening on the mocked signal) until
    // after `await loadConfig(...)` resolves. Wait for that to actually
    // happen before firing the second send, or its abort() call races
    // ahead of the first controller even existing yet.
    await vi.waitFor(() => expect(messageSnapshots.length).toBe(1));
    mockStreamChatOnce(async () => 'second reply');
    const secondSend = send({ type: 'send', text: 'second, supersedes the first' });

    await Promise.all([firstSend, secondSend]);

    expect(capturedSignal?.aborted).toBe(true);
    expect(posted.some((m) => m.type === 'error')).toBe(false);
  });

  it('clear aborts any in-flight request and resets history', async () => {
    mockStreamChatOnce(
      (_client, opts) => new Promise((_resolve, reject) => opts.signal?.addEventListener('abort', () => reject(new Error('aborted')))),
    );
    const { send } = makeProvider();
    const inFlight = send({ type: 'send', text: 'will be cleared' });
    await vi.waitFor(() => expect(messageSnapshots.length).toBe(1));
    await send({ type: 'clear' });
    await inFlight;

    mockStreamChatOnce(async () => 'fresh reply');
    await send({ type: 'send', text: 'after clear' });
    expect(lastMessages()).toEqual([{ role: 'user', content: 'after clear' }]);
  });

  it('prefixes the message with file context when there is an active editor', async () => {
    __setActiveTextEditor({
      document: {
        getText: () => 'const x = 1;',
        languageId: 'typescript',
        uri: { path: 'src/index.ts' },
      },
      selection: { isEmpty: true },
    });
    mockStreamChat(async () => 'reply');
    const { send } = makeProvider();

    await send({ type: 'send', text: 'what does this do?' });

    const content = lastMessages()[0].content;
    expect(content).toContain('Context — visible file from');
    expect(content).toContain('const x = 1;');
    expect(content.endsWith('what does this do?')).toBe(true);
  });

  it('sends the message unprefixed when there is no active editor', async () => {
    mockStreamChat(async () => 'reply');
    const { send } = makeProvider();

    await send({ type: 'send', text: 'no editor open' });

    expect(lastMessages()).toEqual([{ role: 'user', content: 'no editor open' }]);
  });
});
