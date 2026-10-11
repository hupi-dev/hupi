// The HUPI agent loop: send the conversation with tools, run whatever the
// model asks for, send the results back, repeat until a text answer. Pure
// (no `vscode` import) so it's unit-testable; everything with side
// effects comes in through AgentDeps.
//
// The gateway (internal/gateway/agentturn.go) treats all of a turn's
// requests as one turn: memory is searched once, and only the question
// and final answer are remembered — tool arguments and results never are.
import type { AgentMessage, AgentToolCall, AgentToolDef, ChatTurnResult } from '../hupiClient';

export interface AgentDeps {
  /** One request of the turn (hupiClient.streamChatTurn, wired up). */
  send(messages: AgentMessage[], tools: AgentToolDef[], toolChoice?: 'none'): Promise<ChatTurnResult>;
  /** Runs one tool call; returns its result text, throws on failure. */
  runTool(call: AgentToolCall, signal: AbortSignal): Promise<string>;
  /** Asks the user before running a call; false means declined. */
  approve(call: AgentToolCall): Promise<boolean>;
  /** Progress callbacks for the UI. */
  onToolCall?(call: AgentToolCall): void;
  onToolResult?(call: AgentToolCall, ok: boolean, text: string): void;
}

export interface AgentOptions {
  tools: AgentToolDef[];
  maxSteps: number;
  maxToolResultChars: number;
  signal: AbortSignal;
}

export interface AgentTurn {
  /** The final text answer. */
  answer: string;
  /** Messages this turn added after the user's message: assistant tool
   *  calls, tool results, and the final assistant answer. */
  added: AgentMessage[];
  /** Model requests made (tool steps plus the final answer). */
  requests: number;
  /** True if the step limit forced a final answer. */
  hitStepLimit: boolean;
  /** From the final request. */
  final: ChatTurnResult;
}

export const DECLINED_BY_USER = 'The user declined to run this tool.';

export class AgentAbortedError extends Error {
  constructor() {
    super('agent turn cancelled');
    this.name = 'AgentAbortedError';
  }
}

export function truncateResult(text: string, max: number): string {
  if (text.length <= max) {
    return text;
  }
  return `${text.slice(0, max)}\n… (truncated: ${text.length - max} more characters)`;
}

/**
 * Runs one agent turn over `history` (which must end with the user's
 * message). Tool calls run one at a time, in order. A tool error goes back
 * to the model as the result (`error: …`) rather than ending the turn.
 * After maxSteps tool rounds the model is asked for a final answer with
 * tools turned off.
 */
export async function runAgentTurn(history: AgentMessage[], deps: AgentDeps, opts: AgentOptions): Promise<AgentTurn> {
  const added: AgentMessage[] = [];
  const conversation = () => [...history, ...added];
  const checkAbort = () => {
    if (opts.signal.aborted) {
      throw new AgentAbortedError();
    }
  };

  for (let step = 0; ; step++) {
    checkAbort();
    const atLimit = step >= opts.maxSteps;
    const res = await deps.send(conversation(), opts.tools, atLimit ? 'none' : undefined);
    checkAbort();
    if (res.toolCalls.length === 0 || atLimit) {
      added.push({ role: 'assistant', content: res.text });
      return { answer: res.text, added, requests: step + 1, hitStepLimit: atLimit && opts.maxSteps > 0, final: res };
    }

    added.push({ role: 'assistant', content: res.text || null, tool_calls: res.toolCalls });
    for (const call of res.toolCalls) {
      checkAbort();
      deps.onToolCall?.(call);
      let text: string;
      let ok = true;
      if (!(await deps.approve(call))) {
        text = DECLINED_BY_USER;
        ok = false;
      } else {
        try {
          text = await deps.runTool(call, opts.signal);
        } catch (err) {
          checkAbort();
          text = `error: ${(err as Error).message}`;
          ok = false;
        }
      }
      text = truncateResult(text, opts.maxToolResultChars);
      deps.onToolResult?.(call, ok, text);
      added.push({ role: 'tool', tool_call_id: call.id, content: text });
    }
  }
}
