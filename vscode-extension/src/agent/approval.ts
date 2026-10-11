import * as vscode from 'vscode';
import type { AgentToolCall } from '../hupiClient';

/**
 * hupi.agent.approval: "changes" (default) leaves approval to VS Code,
 * which already asks before any tool that isn't read-only. "all" also asks
 * before read-only tools, in a modal showing the call's arguments.
 */
export function makeApprover(mode: string): (call: AgentToolCall) => Promise<boolean> {
  if (mode !== 'all') {
    return async () => true;
  }
  return async (call) => {
    const args = call.function.arguments.length > 500 ? call.function.arguments.slice(0, 500) + '…' : call.function.arguments;
    const choice = await vscode.window.showWarningMessage(
      `HUPI wants to run the tool "${call.function.name}".`,
      { modal: true, detail: `Arguments: ${args || '{}'}` },
      'Run',
    );
    return choice === 'Run';
  };
}
