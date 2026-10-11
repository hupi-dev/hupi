import * as vscode from 'vscode';
import type { AgentToolDef } from '../hupiClient';
import type { HupiTool, ToolContext } from './workspaceTools';

// Which of VS Code's registered tools (vscode.lm.tools) the HUPI agent
// offers the model. Verified in a HUPI Code build (Phase 0 of the agent
// plan): MCP servers' tools appear as `mcp_<server>_<tool>` tagged "mcp",
// alongside VS Code's own core tools.

/** HUPI memory's own MCP tools: never offered, since HUPI chat already
 *  injects memory into every turn. */
const HUPI_MEMORY_TOOLS = ['search_memory', 'get_entity', 'list_entities', 'recent_memory', 'remember', 'log_exchange'];

/** VS Code core tools the agent may offer. Empty for now: several core
 *  tools only work inside VS Code's own chat session, and each needs
 *  checking before it's allowed (plan Phase 5/6). VS Code's internal agent
 *  tools (task_complete, runSubagent, manage_todo_list, ...) are never
 *  offered. */
export const CORE_TOOL_ALLOWLIST: readonly string[] = [];

const NAME_PATTERN = /^[a-zA-Z0-9_-]{1,64}$/;

export function isHupiMemoryTool(name: string): boolean {
  return name.startsWith('mcp_hupi') && HUPI_MEMORY_TOOLS.some((t) => name.endsWith('_' + t));
}

/** The VS Code tools the agent should offer, in registry order. */
export function selectVscodeTools(tools: readonly vscode.LanguageModelToolInformation[]): vscode.LanguageModelToolInformation[] {
  return tools.filter((t) => {
    if (t.tags.includes('mcp')) {
      return !isHupiMemoryTool(t.name);
    }
    return CORE_TOOL_ALLOWLIST.includes(t.name);
  });
}

export interface RegisteredTool {
  /** The name sent to the model (OpenAI's pattern, unique). */
  wireName: string;
  def: AgentToolDef;
  /** The tool asks the user itself (HupiTool.selfApproving), so the
   *  loop's general approval step is skipped for it. */
  selfApproving?: boolean;
  /** Runs the tool with the model's JSON arguments, returning its result
   *  as text. Throws on failure; the loop reports errors to the model. */
  run: (args: string, signal: AbortSignal, ctx?: ToolContext) => Promise<string>;
}

/** Sanitizes a tool name to OpenAI's pattern and makes it unique among
 *  names already taken. */
export function wireName(name: string, taken: Set<string>): string {
  let base = name.replace(/[^a-zA-Z0-9_-]/g, '_').slice(0, 64) || 'tool';
  if (!NAME_PATTERN.test(base)) {
    base = 'tool';
  }
  let candidate = base;
  for (let i = 2; taken.has(candidate); i++) {
    const suffix = `_${i}`;
    candidate = base.slice(0, 64 - suffix.length) + suffix;
  }
  taken.add(candidate);
  return candidate;
}

/** The tool definition sent to the model for a VS Code tool. */
export function toolDef(name: string, info: vscode.LanguageModelToolInformation): AgentToolDef {
  return {
    type: 'function',
    function: {
      name,
      description: info.description,
      parameters: (info.inputSchema as object | undefined) ?? { type: 'object', properties: {} },
    },
  };
}

/** Builds the tool set for one agent turn: HUPI's own tools first (so
 *  they keep their names), then VS Code's. Names that collide or don't
 *  fit the model's pattern are renamed, and runs map back by wire name. */
export function buildRegistry(
  vscodeTools: readonly vscode.LanguageModelToolInformation[],
  runVscodeTool: (name: string, args: string, signal: AbortSignal) => Promise<string>,
  hupiTools: readonly HupiTool[] = [],
): Map<string, RegisteredTool> {
  const taken = new Set<string>();
  const out = new Map<string, RegisteredTool>();
  for (const tool of hupiTools) {
    const name = wireName(tool.name, taken);
    out.set(name, {
      wireName: name,
      def: { type: 'function', function: { name, description: tool.description, parameters: tool.parameters } },
      selfApproving: tool.selfApproving,
      run: async (args, signal, ctx) => {
        let parsed: unknown;
        try {
          parsed = args.trim() === '' ? {} : JSON.parse(args);
        } catch {
          throw new Error(`the tool arguments are not valid JSON: ${args.slice(0, 200)}`);
        }
        if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
          throw new Error('the tool arguments must be a JSON object');
        }
        return tool.run(parsed as Record<string, unknown>, signal, ctx);
      },
    });
  }
  for (const info of vscodeTools) {
    const name = wireName(info.name, taken);
    out.set(name, { wireName: name, def: toolDef(name, info), run: (args, signal) => runVscodeTool(info.name, args, signal) });
  }
  return out;
}

/** Starts the user's configured MCP servers so their tools are listed.
 *  Nothing auto-starts them in HUPI Code (no native chat view), so the
 *  agent does it at the start of each turn. Bounded and best-effort. */
export async function startMcpServers(timeoutMs = 15_000): Promise<void> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([
      vscode.commands.executeCommand('workbench.mcp.startServer', '*', { waitForLiveTools: true }),
      new Promise((resolve) => {
        timer = setTimeout(resolve, timeoutMs);
      }),
    ]);
  } catch {
    // No MCP support, or a server failed to start: the agent just has
    // fewer tools this turn.
  } finally {
    clearTimeout(timer);
  }
}
