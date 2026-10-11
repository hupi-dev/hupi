import { beforeEach, describe, expect, it } from 'vitest';
import * as vscode from 'vscode';
import {
  __resetVscodeMock,
  CancellationError,
  invokeTool,
  LanguageModelDataPart,
  LanguageModelPromptTsxPart,
  LanguageModelTextPart,
  LanguageModelToolResult,
} from '../test/vscode-mock';
import { DECLINED_BY_USER } from './agentLoop';
import { resultText, runVscodeTool } from './vscodeTools';

beforeEach(() => __resetVscodeMock());

describe('runVscodeTool', () => {
  it('invokes the tool with parsed arguments and no chat token', async () => {
    invokeTool.mockResolvedValue(new LanguageModelToolResult([new LanguageModelTextPart('42 issues')]));
    const text = await runVscodeTool('mcp_github_search', '{"q":"bug"}', new AbortController().signal);
    expect(text).toBe('42 issues');
    expect(invokeTool).toHaveBeenCalledWith('mcp_github_search', { input: { q: 'bug' }, toolInvocationToken: undefined }, expect.anything());
  });

  it('treats empty arguments as {} and rejects invalid JSON', async () => {
    invokeTool.mockResolvedValue(new LanguageModelToolResult([]));
    await runVscodeTool('t', '  ', new AbortController().signal);
    expect(invokeTool.mock.calls[0][1]).toEqual({ input: {}, toolInvocationToken: undefined });
    await expect(runVscodeTool('t', '{not json', new AbortController().signal)).rejects.toThrow(/not valid JSON/);
  });

  it("reports the user declining VS Code's confirmation as a normal result", async () => {
    invokeTool.mockRejectedValue(new CancellationError());
    await expect(runVscodeTool('t', '{}', new AbortController().signal)).resolves.toBe(DECLINED_BY_USER);
  });

  it('cancels the tool when the turn is aborted, and rethrows', async () => {
    const controller = new AbortController();
    let cancelled = false;
    invokeTool.mockImplementation((_n, _o, token) => {
      (token as vscode.CancellationToken).onCancellationRequested(() => {
        cancelled = true;
      });
      controller.abort();
      return Promise.reject(new CancellationError());
    });
    await expect(runVscodeTool('t', '{}', controller.signal)).rejects.toBeInstanceOf(CancellationError);
    expect(cancelled).toBe(true);
  });

  it('propagates other tool failures', async () => {
    invokeTool.mockRejectedValue(new Error('server crashed'));
    await expect(runVscodeTool('t', '{}', new AbortController().signal)).rejects.toThrow('server crashed');
  });
});

describe('resultText', () => {
  it('flattens text, text-like data, binary data and prompt parts', () => {
    const r = new LanguageModelToolResult([
      new LanguageModelTextPart('line one'),
      new LanguageModelDataPart(new TextEncoder().encode('{"a":1}'), 'application/json'),
      new LanguageModelDataPart(new Uint8Array(10), 'image/png'),
      new LanguageModelPromptTsxPart({ node: 'x' }),
    ]);
    expect(resultText(r as unknown as vscode.LanguageModelToolResult)).toBe('line one\n{"a":1}\n[image/png data, 10 bytes]\n{"node":"x"}');
  });
});
