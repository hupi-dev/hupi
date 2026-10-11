import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setConfig, __setLmTools, invokeTool, LanguageModelTextPart, LanguageModelToolResult } from './test/vscode-mock';

vi.mock('./hupiClient', () => ({
  createClient: vi.fn(),
  streamChat: vi.fn(),
  streamChatTurn: vi.fn(),
}));
vi.mock('./config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./config')>();
  return { ...actual, loadConfig: vi.fn() };
});
vi.mock('./oidcAuth', () => ({ promptSignInRequired: vi.fn() }));

import { createClient, streamChat, streamChatTurn, type AgentMessage, type StreamChatTurnOptions } from './hupiClient';
import { loadConfig } from './config';
import { HupiChatViewProvider, textOnly } from './chatViewProvider';

type Posted = { type: string; text?: string; message?: string; items?: unknown[] };

function makeProvider() {
  const posted: Posted[] = [];
  let receive: ((m: unknown) => Promise<void>) | undefined;
  const webview = {
    options: undefined as unknown,
    html: '',
    cspSource: 'csp',
    postMessage: async (m: Posted) => {
      posted.push(m);
      return true;
    },
    onDidReceiveMessage: (cb: (m: unknown) => Promise<void>) => {
      receive = cb;
      return { dispose() {} };
    },
    asWebviewUri: (u: unknown) => u,
  };
  const context = { extensionUri: { path: '/fake' } } as unknown as vscode.ExtensionContext;
  const provider = new HupiChatViewProvider(context, context.extensionUri as unknown as vscode.Uri);
  provider.resolveWebviewView({ webview } as unknown as vscode.WebviewView);
  return { posted, send: (text: string) => receive!({ type: 'send', text }) };
}

let sent: { messages: AgentMessage[]; tools: string[]; toolChoice?: string }[];
function scriptTurns(...results: { text: string; calls?: { id: string; name: string; args: string }[]; citations?: unknown[] }[]) {
  vi.mocked(streamChatTurn).mockImplementation(async (_c, opts: StreamChatTurnOptions) => {
    sent.push({ messages: structuredClone(opts.messages), tools: opts.tools.map((t) => t.function.name), toolChoice: opts.toolChoice });
    const r = results.shift();
    if (!r) throw new Error('unscripted request');
    if (r.text) opts.onDelta(r.text);
    return {
      text: r.text,
      toolCalls: (r.calls ?? []).map((c) => ({ id: c.id, type: 'function' as const, function: { name: c.name, arguments: c.args } })),
      finishReason: r.calls?.length ? 'tool_calls' : 'stop',
      citations: (r.citations ?? []) as never,
      attachmentWarnings: [],
    };
  });
}

beforeEach(() => {
  __resetVscodeMock();
  sent = [];
  vi.mocked(loadConfig).mockReset().mockResolvedValue({ baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'k' });
  vi.mocked(createClient).mockReset().mockReturnValue({} as never);
  vi.mocked(streamChat).mockReset();
  vi.mocked(streamChatTurn).mockReset();
  __setConfig({ 'agent.enabled': true, 'fileContext.enabled': false });
  __setLmTools([
    { name: 'mcp_github_search_issues', description: 'Search issues', tags: ['mcp'], inputSchema: { type: 'object' } },
    { name: 'mcp_hupi_memory_search_memory', description: 'HUPI memory', tags: ['mcp'] },
    { name: 'run_in_terminal', description: 'core', tags: [] },
  ]);
});

describe('HUPI agent in the sidebar', () => {
  it('offers MCP tools, runs a call through VS Code, and keeps the whole turn in history', async () => {
    invokeTool.mockResolvedValue(new LanguageModelToolResult([new LanguageModelTextPart('3 open bugs')]));
    scriptTurns({ text: '', calls: [{ id: 'c1', name: 'mcp_github_search_issues', args: '{"q":"bug"}' }] }, { text: 'You have 3 open bugs.', citations: [{ ref: {}, snippet: 's' }] });
    const { posted, send } = makeProvider();
    await send('How many open bugs?');

    expect(sent[0].tools).toEqual(['mcp_github_search_issues']);
    expect(invokeTool).toHaveBeenCalledWith('mcp_github_search_issues', { input: { q: 'bug' }, toolInvocationToken: undefined }, expect.anything());
    expect(sent[1].messages.map((m) => m.role)).toEqual(['user', 'assistant', 'tool']);
    expect(posted.some((p) => p.type === 'delta' && p.text?.includes('Running tool `mcp_github_search_issues`'))).toBe(true);
    expect(posted.filter((p) => p.type === 'citations')).toHaveLength(1);
    expect(posted[posted.length - 1]).toEqual({ type: 'done' });

    // The next turn's first request carries the full previous turn.
    scriptTurns({ text: 'Sure.' });
    await send('Thanks');
    expect(sent[2].messages.map((m) => m.role)).toEqual(['user', 'assistant', 'tool', 'assistant', 'user']);
  });

  it('rolls back every message of a failed agent turn', async () => {
    invokeTool.mockResolvedValue(new LanguageModelToolResult([new LanguageModelTextPart('x')]));
    vi.mocked(streamChatTurn)
      .mockImplementationOnce(async (_c, opts) => {
        sent.push({ messages: structuredClone(opts.messages), tools: [], toolChoice: undefined });
        return { text: '', toolCalls: [{ id: 'c1', type: 'function', function: { name: 'mcp_github_search_issues', arguments: '{}' } }], finishReason: 'tool_calls', citations: [], attachmentWarnings: [] };
      })
      .mockRejectedValueOnce(new Error('gateway down'));
    const { posted, send } = makeProvider();
    await send('First question');
    expect(posted.some((p) => p.type === 'error' && p.message?.includes('gateway down'))).toBe(true);

    scriptTurns({ text: 'Answer.' });
    await send('Second question');
    expect(sent[sent.length - 1].messages).toEqual([{ role: 'user', content: 'Second question' }]);
  });

  it('uses plain chat with only text messages when the agent is off', async () => {
    invokeTool.mockResolvedValue(new LanguageModelToolResult([new LanguageModelTextPart('x')]));
    scriptTurns({ text: '', calls: [{ id: 'c1', name: 'mcp_github_search_issues', args: '{}' }] }, { text: 'Agent answer.' });
    const { send } = makeProvider();
    await send('Q1');

    __setConfig({ 'agent.enabled': false, 'fileContext.enabled': false });
    let plainMessages: unknown[] = [];
    vi.mocked(streamChat).mockImplementation(async (_c, opts) => {
      plainMessages = structuredClone(opts.messages);
      return 'Plain answer.';
    });
    await send('Q2');
    expect(plainMessages).toEqual([
      { role: 'user', content: 'Q1' },
      { role: 'assistant', content: 'Agent answer.' },
      { role: 'user', content: 'Q2' },
    ]);
  });

  it('offers no VS Code tools when hupi.agent.vscodeTools is off', async () => {
    __setConfig({ 'agent.enabled': true, 'agent.vscodeTools': false, 'fileContext.enabled': false });
    scriptTurns({ text: 'No tools.' });
    const { send } = makeProvider();
    await send('Q');
    expect(sent[0].tools).toEqual([]);
  });
});

describe('textOnly', () => {
  it('drops tool results and tool-call-only assistant messages', () => {
    const history: AgentMessage[] = [
      { role: 'user', content: 'q' },
      { role: 'assistant', content: null, tool_calls: [{ id: 'c', type: 'function', function: { name: 'f', arguments: '{}' } }] },
      { role: 'tool', tool_call_id: 'c', content: 'r' },
      { role: 'assistant', content: 'a' },
    ];
    expect(textOnly(history)).toEqual([
      { role: 'user', content: 'q' },
      { role: 'assistant', content: 'a' },
    ]);
  });
});
