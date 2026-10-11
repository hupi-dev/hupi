import { describe, expect, it, vi } from 'vitest';
import type { AgentMessage, AgentToolCall, ChatTurnResult } from '../hupiClient';
import { AgentAbortedError, DECLINED_BY_USER, runAgentTurn, truncateResult, type AgentDeps } from './agentLoop';

const call = (id: string, name: string, args = '{}'): AgentToolCall => ({ id, type: 'function', function: { name, arguments: args } });
const reply = (text: string, toolCalls: AgentToolCall[] = []): ChatTurnResult => ({
  text,
  toolCalls,
  finishReason: toolCalls.length ? 'tool_calls' : 'stop',
  citations: [],
  attachmentWarnings: [],
});
const tools = [{ type: 'function' as const, function: { name: 'get_weather', parameters: {} } }];
const opts = (signal = new AbortController().signal) => ({ tools, maxSteps: 10, maxToolResultChars: 1000, signal });

function deps(replies: ChatTurnResult[], overrides: Partial<AgentDeps> = {}): AgentDeps & { sent: AgentMessage[][]; choices: (string | undefined)[] } {
  const sent: AgentMessage[][] = [];
  const choices: (string | undefined)[] = [];
  return {
    sent,
    choices,
    send: vi.fn(async (messages: AgentMessage[], _tools: unknown, toolChoice?: 'none') => {
      sent.push(messages.map((m) => ({ ...m })));
      choices.push(toolChoice);
      const r = replies.shift();
      if (!r) throw new Error('no more replies scripted');
      return r;
    }),
    runTool: vi.fn(async (c: AgentToolCall) => `result of ${c.function.name}`),
    approve: vi.fn(async () => true),
    ...overrides,
  };
}

const history: AgentMessage[] = [{ role: 'user', content: 'Weather in Oslo and Rome?' }];

describe('runAgentTurn', () => {
  it('returns a direct answer in one request', async () => {
    const d = deps([reply('Sunny.')]);
    const turn = await runAgentTurn(history, d, opts());
    expect(turn).toMatchObject({ answer: 'Sunny.', requests: 1, hitStepLimit: false });
    expect(turn.added).toEqual([{ role: 'assistant', content: 'Sunny.' }]);
    expect(d.runTool).not.toHaveBeenCalled();
  });

  it('runs tool calls in order, sends results back, and returns the answer', async () => {
    const d = deps([reply('Checking.', [call('c1', 'get_weather', '{"city":"Oslo"}'), call('c2', 'get_weather', '{"city":"Rome"}')]), reply('Oslo 5C, Rome 20C.')]);
    const turn = await runAgentTurn(history, d, opts());

    expect(vi.mocked(d.runTool).mock.calls.map((c) => c[0].id)).toEqual(['c1', 'c2']);
    expect(turn.answer).toBe('Oslo 5C, Rome 20C.');
    expect(turn.requests).toBe(2);
    expect(turn.added).toEqual([
      { role: 'assistant', content: 'Checking.', tool_calls: [call('c1', 'get_weather', '{"city":"Oslo"}'), call('c2', 'get_weather', '{"city":"Rome"}')] },
      { role: 'tool', tool_call_id: 'c1', content: 'result of get_weather' },
      { role: 'tool', tool_call_id: 'c2', content: 'result of get_weather' },
      { role: 'assistant', content: 'Oslo 5C, Rome 20C.' },
    ]);
    // The second request carries the question, the calls and both results.
    expect(d.sent[1]).toHaveLength(4);
    expect(d.sent[1][0]).toEqual(history[0]);
  });

  it('sends a tool-call-only step with null content', async () => {
    const d = deps([reply('', [call('c1', 'get_weather')]), reply('Done.')]);
    const turn = await runAgentTurn(history, d, opts());
    expect(turn.added[0]).toMatchObject({ role: 'assistant', content: null });
  });

  it('reports a tool error to the model and carries on', async () => {
    const onToolResult = vi.fn();
    const d = deps([reply('', [call('c1', 'get_weather')]), reply('Could not get the weather.')], {
      runTool: vi.fn(async () => {
        throw new Error('server unreachable');
      }),
      onToolResult,
    });
    const turn = await runAgentTurn(history, d, opts());
    expect(turn.added[1]).toEqual({ role: 'tool', tool_call_id: 'c1', content: 'error: server unreachable' });
    expect(onToolResult).toHaveBeenCalledWith(expect.objectContaining({ id: 'c1' }), false, 'error: server unreachable');
    expect(turn.answer).toBe('Could not get the weather.');
  });

  it('tells the model when the user declines, without running the tool', async () => {
    const d = deps([reply('', [call('c1', 'get_weather')]), reply('OK, skipped.')], { approve: vi.fn(async () => false) });
    const turn = await runAgentTurn(history, d, opts());
    expect(d.runTool).not.toHaveBeenCalled();
    expect(turn.added[1]).toEqual({ role: 'tool', tool_call_id: 'c1', content: DECLINED_BY_USER });
  });

  it('truncates long tool results', async () => {
    const d = deps([reply('', [call('c1', 'get_weather')]), reply('ok')], { runTool: vi.fn(async () => 'x'.repeat(5000)) });
    const turn = await runAgentTurn(history, d, { ...opts(), maxToolResultChars: 100 });
    const content = (turn.added[1] as { content: string }).content;
    expect(content.startsWith('x'.repeat(100) + '\n… (truncated: 4900 more')).toBe(true);
    expect(truncateResult('short', 100)).toBe('short');
  });

  it('forces a final answer without tools after maxSteps rounds', async () => {
    const d = deps([reply('', [call('c1', 'get_weather')]), reply('', [call('c2', 'get_weather')]), reply('Best I can do.')]);
    const turn = await runAgentTurn(history, d, { ...opts(), maxSteps: 2 });
    expect(d.choices).toEqual([undefined, undefined, 'none']);
    expect(turn).toMatchObject({ answer: 'Best I can do.', requests: 3, hitStepLimit: true });
  });

  it('stops with AgentAbortedError when cancelled mid-turn', async () => {
    const controller = new AbortController();
    const d = deps([reply('', [call('c1', 'get_weather'), call('c2', 'get_weather')]), reply('never')], {
      runTool: vi.fn(async () => {
        controller.abort();
        throw new Error('aborted');
      }),
    });
    await expect(runAgentTurn(history, d, opts(controller.signal))).rejects.toBeInstanceOf(AgentAbortedError);
    expect(d.runTool).toHaveBeenCalledTimes(1);
    expect(d.send).toHaveBeenCalledTimes(1);
  });

  it('does nothing if already cancelled', async () => {
    const controller = new AbortController();
    controller.abort();
    const d = deps([reply('x')]);
    await expect(runAgentTurn(history, d, opts(controller.signal))).rejects.toBeInstanceOf(AgentAbortedError);
    expect(d.send).not.toHaveBeenCalled();
  });
});
