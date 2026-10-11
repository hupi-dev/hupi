import * as vscode from 'vscode';
import { DECLINED_BY_USER } from './agentLoop';

/**
 * Runs one of VS Code's registered tools (an MCP tool, typically) with the
 * model's JSON arguments and returns its result as text.
 *
 * There's no chat session token here, so VS Code shows its own modal
 * confirmation for any tool that isn't read-only (verified in Phase 0 of
 * the agent plan), and throws a CancellationError if the user declines.
 */
export async function runVscodeTool(name: string, args: string, signal: AbortSignal): Promise<string> {
  let input: object;
  try {
    input = args.trim() === '' ? {} : (JSON.parse(args) as object);
  } catch {
    throw new Error(`the tool arguments are not valid JSON: ${args.slice(0, 200)}`);
  }
  const cts = new vscode.CancellationTokenSource();
  const onAbort = () => cts.cancel();
  signal.addEventListener('abort', onAbort);
  try {
    const result = await vscode.lm.invokeTool(name, { input, toolInvocationToken: undefined }, cts.token);
    return resultText(result);
  } catch (err) {
    if (signal.aborted) {
      throw err;
    }
    // The user declined VS Code's confirmation: a normal result, so the
    // model can carry on without the tool.
    if (err instanceof vscode.CancellationError) {
      return DECLINED_BY_USER;
    }
    throw err;
  } finally {
    signal.removeEventListener('abort', onAbort);
    cts.dispose();
  }
}

/** Flattens a tool result to text for the model. */
export function resultText(result: vscode.LanguageModelToolResult): string {
  const out: string[] = [];
  for (const part of result.content) {
    if (part instanceof vscode.LanguageModelTextPart) {
      out.push(part.value);
    } else if (part instanceof vscode.LanguageModelDataPart) {
      const mime = part.mimeType;
      if (mime.startsWith('text/') || mime.includes('json')) {
        out.push(new TextDecoder().decode(part.data));
      } else {
        out.push(`[${mime} data, ${part.data.byteLength} bytes]`);
      }
    } else if (part instanceof vscode.LanguageModelPromptTsxPart) {
      out.push(JSON.stringify(part.value));
    }
  }
  return out.join('\n');
}
