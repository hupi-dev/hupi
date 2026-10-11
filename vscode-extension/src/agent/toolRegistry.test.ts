import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, executeCommand } from '../test/vscode-mock';
import { buildRegistry, isHupiMemoryTool, selectVscodeTools, startMcpServers, toolDef, wireName } from './toolRegistry';

const info = (name: string, tags: string[] = [], inputSchema?: object) =>
  ({ name, description: `${name} tool`, tags, inputSchema }) as unknown as vscode.LanguageModelToolInformation;

beforeEach(() => __resetVscodeMock());

describe('selectVscodeTools', () => {
  it('offers MCP tools only, never HUPI memory or VS Code core/internal tools', () => {
    const tools = [
      info('mcp_github_search_issues', ['mcp']),
      info('mcp_hupi_memory_search_memory', ['mcp']),
      info('mcp_hupi_remember', ['mcp']),
      info('run_in_terminal'),
      info('task_complete'),
      info('mcp_files_read', ['mcp']),
    ];
    expect(selectVscodeTools(tools).map((t) => t.name)).toEqual(['mcp_github_search_issues', 'mcp_files_read']);
  });

  it('only treats HUPI-named servers with HUPI memory tool names as HUPI memory', () => {
    expect(isHupiMemoryTool('mcp_hupi_memory_search_memory')).toBe(true);
    expect(isHupiMemoryTool('mcp_other_search_memory')).toBe(false);
    expect(isHupiMemoryTool('mcp_hupi_memory_custom')).toBe(false);
  });
});

describe('wireName', () => {
  it('sanitizes to the model name pattern and de-duplicates', () => {
    const taken = new Set<string>();
    expect(wireName('mcp_files_read', taken)).toBe('mcp_files_read');
    expect(wireName('mcp_files_read', taken)).toBe('mcp_files_read_2');
    expect(wireName('weird name/with.dots', taken)).toBe('weird_name_with_dots');
    const long = 'a'.repeat(80);
    expect(wireName(long, taken)).toBe('a'.repeat(64));
    expect(wireName(long, taken)).toBe('a'.repeat(62) + '_2');
  });
});

describe('buildRegistry', () => {
  it('maps wire names back to the original tool and builds definitions', async () => {
    const run = vi.fn(async () => 'ok');
    const reg = buildRegistry([info('mcp.files.read', ['mcp'], { type: 'object', properties: { path: { type: 'string' } } }), info('mcp_x', ['mcp'])], run);
    expect([...reg.keys()]).toEqual(['mcp_files_read', 'mcp_x']);
    const t = reg.get('mcp_files_read')!;
    expect(t.def).toEqual({
      type: 'function',
      function: { name: 'mcp_files_read', description: 'mcp.files.read tool', parameters: { type: 'object', properties: { path: { type: 'string' } } } },
    });
    await t.run('{"path":"a"}', new AbortController().signal);
    expect(run).toHaveBeenCalledWith('mcp.files.read', '{"path":"a"}', expect.anything());
    expect(toolDef('x', info('x')).function.parameters).toEqual({ type: 'object', properties: {} });
  });
});

describe('startMcpServers', () => {
  it('starts every configured server and waits for their tools', async () => {
    await startMcpServers(1000);
    expect(executeCommand).toHaveBeenCalledWith('workbench.mcp.startServer', '*', { waitForLiveTools: true });
  });

  it('gives up after its timeout and never throws', async () => {
    executeCommand.mockImplementationOnce(() => new Promise(() => {}));
    const t0 = Date.now();
    await startMcpServers(50);
    expect(Date.now() - t0).toBeLessThan(1000);
    executeCommand.mockImplementationOnce(async () => {
      throw new Error('no MCP');
    });
    await expect(startMcpServers(50)).resolves.toBeUndefined();
  });
});
