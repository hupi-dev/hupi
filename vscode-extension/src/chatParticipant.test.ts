import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setActiveTextEditor, __setConfig, __setFileContents, createChatParticipant } from './test/vscode-mock';

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
let explainSnapshots: (StreamChatOptions['explain'] | undefined)[] = [];
let attachmentSnapshots: (StreamChatOptions['attachments'] | undefined)[] = [];
function lastMessages(): { role: string; content: string }[] {
  return messageSnapshots[messageSnapshots.length - 1];
}
function lastAttachments(): StreamChatOptions['attachments'] | undefined {
  return attachmentSnapshots[attachmentSnapshots.length - 1];
}
type StreamChatImpl = (client: never, opts: StreamChatOptions) => Promise<string>;

function mockStreamChat(impl: StreamChatImpl) {
  return vi.mocked(streamChat).mockImplementation((client, opts) => {
    messageSnapshots.push(opts.messages.map((m) => ({ role: m.role, content: m.content })));
    explainSnapshots.push(opts.explain);
    attachmentSnapshots.push(opts.attachments);
    return impl(client as never, opts);
  });
}
function mockStreamChatOnce(impl: StreamChatImpl) {
  return vi.mocked(streamChat).mockImplementationOnce((client, opts) => {
    messageSnapshots.push(opts.messages.map((m) => ({ role: m.role, content: m.content })));
    explainSnapshots.push(opts.explain);
    attachmentSnapshots.push(opts.attachments);
    return impl(client as never, opts);
  });
}

beforeEach(() => {
  __resetVscodeMock();
  messageSnapshots = [];
  explainSnapshots = [];
  attachmentSnapshots = [];
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

function fakeRequest(prompt: string, references: unknown[] = []): vscode.ChatRequest {
  return { prompt, references } as unknown as vscode.ChatRequest;
}

function fakeStream() {
  const chunks: string[] = [];
  const references: unknown[] = [];
  return {
    stream: {
      markdown: (text: string) => chunks.push(text),
      reference: (value: unknown) => references.push(value),
    } as unknown as vscode.ChatResponseStream,
    chunks,
    references,
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

  it('prefixes the prompt with file context and shows a reference when there is an active editor', async () => {
    const fakeUri = { path: 'src/index.ts' };
    __setActiveTextEditor({
      document: {
        getText: () => 'const x = 1;',
        languageId: 'typescript',
        uri: fakeUri,
      },
      selection: { isEmpty: true },
    });
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, references } = fakeStream();

    await handler(fakeRequest('what does this do?'), { history: [] }, stream, token);

    const content = lastMessages()[0].content;
    expect(content).toContain('Context — visible file from');
    expect(content).toContain('const x = 1;');
    expect(content.endsWith('what does this do?')).toBe(true);
    // review finding A13 — a visible reference chip, VS Code's own
    // built-in way of showing "this file informed the answer."
    expect(references).toEqual([fakeUri]);
  });

  it('sends no file context and no reference when hupi.fileContext.enabled is false (review finding A13)', async () => {
    __setActiveTextEditor({
      document: {
        getText: () => 'const x = 1;',
        languageId: 'typescript',
        uri: { path: 'src/index.ts' },
      },
      selection: { isEmpty: true },
    });
    __setConfig({ 'fileContext.enabled': false });
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, references } = fakeStream();

    await handler(fakeRequest('what does this do?'), { history: [] }, stream, token);

    expect(lastMessages()[0].content).toBe('what does this do?');
    expect(references).toEqual([]);
  });

  it('requests citations on by default (hupi.citations.enabled defaults to true)', async () => {
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(explainSnapshots).toEqual(['on']);
  });

  it('does not request citations when hupi.citations.enabled is false', async () => {
    __setConfig({ 'citations.enabled': false });
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(explainSnapshots).toEqual([undefined]);
  });

  it('requests deep citations when hupi.citations.deep is true', async () => {
    __setConfig({ 'citations.enabled': true, 'citations.deep': true });
    mockStreamChat(async () => 'reply');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(explainSnapshots).toEqual(['deep']);
  });

  it('renders citations as a Sources markdown block after the answer', async () => {
    mockStreamChat(async (_client, opts) => {
      opts.onDelta('the answer');
      opts.onCitations?.([
        { ref: { kind: 'summary', scope: { kind: 'private', owner: 'user:1' }, id: 'sum_1' }, snippet: 'source text', used: true },
      ]);
      return 'the answer';
    });
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(chunks[0]).toBe('the answer');
    expect(chunks[1]).toContain('Sources');
    expect(chunks[1]).toContain('summary');
    expect(chunks[1]).toContain('source text');
    expect(chunks[1]).toContain('✓ used');
  });
});

describe('attachments', () => {
  it('resolves a file reference into a document attachment', async () => {
    mockStreamChat(async () => 'ok');
    const uri = vscode.Uri.file('/workspace/resume.txt');
    __setFileContents(uri, Buffer.from('Senior Go engineer, 5 years experience.'));
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('check my resume', [{ id: 'file', value: uri }]), { history: [] }, stream, token);

    expect(lastAttachments()).toEqual([
      { type: 'document', filename: 'resume.txt', content_type: undefined, data: Buffer.from('Senior Go engineer, 5 years experience.').toString('base64') },
    ]);
  });

  it('resolves an image-extension file reference into an image attachment with the right content_type', async () => {
    mockStreamChat(async () => 'ok');
    const uri = vscode.Uri.file('/workspace/photo.png');
    __setFileContents(uri, Buffer.from('fake-png-bytes'));
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('what is this?', [{ id: 'file', value: uri }]), { history: [] }, stream, token);

    expect(lastAttachments()).toEqual([
      { type: 'image', filename: 'photo.png', content_type: 'image/png', data: Buffer.from('fake-png-bytes').toString('base64') },
    ]);
  });

  it('ignores references that are not file Uris (e.g. a code selection or bare string)', async () => {
    mockStreamChat(async () => 'ok');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(
      fakeRequest('hi', [
        { id: 'selection', value: 'some bare string value' },
        { id: 'symbol', value: { name: 'not a uri or location' } },
      ]),
      { history: [] },
      stream,
      token,
    );

    expect(lastAttachments()).toBeUndefined();
  });

  it('sends attachments: undefined (not an empty array) when there are no file references', async () => {
    mockStreamChat(async () => 'ok');
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream } = fakeStream();

    await handler(fakeRequest('hi'), { history: [] }, stream, token);

    expect(lastAttachments()).toBeUndefined();
  });

  it('skips an oversized file and surfaces a warning in the response, without including it as an attachment', async () => {
    mockStreamChat(async () => 'ok');
    const uri = vscode.Uri.file('/workspace/huge.pdf');
    __setFileContents(uri, new Uint8Array(8 * 1024 * 1024 + 1));
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('read this', [{ id: 'file', value: uri }]), { history: [] }, stream, token);

    expect(lastAttachments()).toBeUndefined();
    expect(chunks.some((c) => c.includes('huge.pdf') && c.includes('size limit'))).toBe(true);
  });

  it('renders attachment warnings returned by the server as markdown', async () => {
    mockStreamChat(async (_client, opts) => {
      opts.onDelta('here you go');
      opts.onAttachmentWarnings?.(['scan.pdf: no extractable text found — this PDF may be scanned/image-only']);
      return 'here you go';
    });
    const uri = vscode.Uri.file('/workspace/scan.pdf');
    __setFileContents(uri, Buffer.from('%PDF-fake'));
    const handler = registerAndGetHandler();
    const { token } = fakeCancellationToken();
    const { stream, chunks } = fakeStream();

    await handler(fakeRequest('read this', [{ id: 'file', value: uri }]), { history: [] }, stream, token);

    expect(chunks.some((c) => c.includes('no extractable text found'))).toBe(true);
  });
});
