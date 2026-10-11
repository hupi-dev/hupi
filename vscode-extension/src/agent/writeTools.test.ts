import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  __resetVscodeMock,
  __setFileContents,
  __setShellBehavior,
  __setShellIntegrationAvailable,
  __setTrusted,
  __setWorkspaceFolders,
  applyEdit,
  executeCommand,
  executedCommands,
  openedDocuments,
  sentText,
  Uri,
  writeFile,
} from '../test/vscode-mock';
import { DECLINED_BY_USER } from './agentLoop';
import { __resetTerminal, cleanTerminalOutput, inDirectory } from './terminalRunner';
import { WRITE_TOOLS, writeToolsFor, __resetWriteTools } from './writeTools';

const root = Uri.file('/work/app');
const file = (rel: string) => Uri.joinPath(root, ...rel.split('/'));
const put = (rel: string, text: string) => __setFileContents(file(rel), new TextEncoder().encode(text));
const tool = (name: string) => WRITE_TOOLS.find((t) => t.name === name)!;

function ctx(answer: boolean) {
  const asked: string[] = [];
  return { asked, ctx: { ask: vi.fn(async (detail: string) => (asked.push(detail), answer)) } };
}
const run = (name: string, args: Record<string, unknown>, c = ctx(true).ctx, signal = new AbortController().signal) => tool(name).run(args, signal, c);

beforeEach(() => {
  __resetVscodeMock();
  __resetWriteTools();
  __resetTerminal();
  __setWorkspaceFolders([{ name: 'app', uri: root }]);
  applyEdit.mockImplementation(async (edit: { edits: { uri: { toString(): string }; newText: string }[] }) => {
    for (const e of edit.edits) {
      const doc = openedDocuments.get(e.uri.toString());
      if (doc) doc.text = e.newText;
    }
    return true;
  });
});

describe('edit_file', () => {
  it('shows a diff, asks, then applies and saves exactly one replacement', async () => {
    put('src/a.ts', 'const a = 1;\nconst b = 2;\n');
    const { asked, ctx: c } = ctx(true);
    const out = await run('edit_file', { path: 'src/a.ts', old_text: 'const b = 2;', new_text: 'const b = 3;\nconst c = 4;' }, c);
    expect(out).toBe('Edited src/a.ts.');
    expect(asked[0]).toMatch(/^Edit src\/a\.ts: replace 1 line\(s\) with 2 line\(s\)/);
    expect(executeCommand).toHaveBeenCalledWith('vscode.diff', expect.anything(), expect.anything(), expect.stringContaining('src/a.ts'), { preview: true });
    const doc = openedDocuments.get(file('src/a.ts').toString())!;
    expect(doc.text).toBe('const a = 1;\nconst b = 3;\nconst c = 4;\n');
    expect(doc.save).toHaveBeenCalled();
  });

  it('changes nothing when the user denies', async () => {
    put('src/a.ts', 'x = 1');
    const out = await run('edit_file', { path: 'src/a.ts', old_text: 'x = 1', new_text: 'x = 2' }, ctx(false).ctx);
    expect(out).toBe(DECLINED_BY_USER);
    expect(applyEdit).not.toHaveBeenCalled();
  });

  it('refuses missing, ambiguous, secret and outside edits without asking', async () => {
    put('src/a.ts', 'dup\ndup\n');
    put('.env', 'KEY=1');
    const { asked, ctx: c } = ctx(true);
    await expect(run('edit_file', { path: 'src/a.ts', old_text: 'nope', new_text: 'x' }, c)).rejects.toThrow(/not found/);
    await expect(run('edit_file', { path: 'src/a.ts', old_text: 'dup', new_text: 'x' }, c)).rejects.toThrow(/occurs 2 times/);
    await expect(run('edit_file', { path: '.env', old_text: 'KEY', new_text: 'X' }, c)).rejects.toThrow(/secrets/);
    await expect(run('edit_file', { path: '../x.ts', old_text: 'a', new_text: 'b' }, c)).rejects.toThrow(/outside/);
    await expect(run('edit_file', { path: 'src/a.ts', old_text: '', new_text: 'b' }, c)).rejects.toThrow(/required/);
    expect(asked).toEqual([]);
  });

  it('keeps $-patterns in new_text literal', async () => {
    put('a.ts', 'price');
    await run('edit_file', { path: 'a.ts', old_text: 'price', new_text: "'$&' costs $1" });
    expect(openedDocuments.get(file('a.ts').toString())!.text).toBe("'$&' costs $1");
  });
});

describe('create_file', () => {
  it('previews, asks, and writes a new file', async () => {
    const { asked, ctx: c } = ctx(true);
    expect(await run('create_file', { path: 'src/new.ts', content: 'export {};\n' }, c)).toBe('Created src/new.ts.');
    expect(asked[0]).toMatch(/^Create src\/new\.ts \(2 line\(s\)\)/);
    expect(new TextDecoder().decode(writeFile.mock.calls[0][1])).toBe('export {};\n');
  });
  it('refuses to overwrite, to write secrets, or to target the folder', async () => {
    put('exists.ts', 'x');
    await expect(run('create_file', { path: 'exists.ts', content: 'y' })).rejects.toThrow(/already exists/);
    await expect(run('create_file', { path: 'config/.env.local', content: 'K=1' })).rejects.toThrow(/secrets/);
    await expect(run('create_file', { path: '.', content: 'y' })).rejects.toThrow(/file path is required/);
    expect(writeFile).not.toHaveBeenCalled();
  });
  it('writes nothing when denied', async () => {
    expect(await run('create_file', { path: 'n.ts', content: 'x' }, ctx(false).ctx)).toBe(DECLINED_BY_USER);
    expect(writeFile).not.toHaveBeenCalled();
  });
});

describe('run_command', () => {
  it('asks with the exact command, runs it in the workspace, returns exit code and clean output', async () => {
    __setShellBehavior(() => ({ chunks: ['\x1b]633;C\x07ok 3 tests\r\n', '\x1b[32mPASS\x1b[0m\r\n'], exitCode: 0 }));
    const { asked, ctx: c } = ctx(true);
    const out = await run('run_command', { command: 'npm test', cwd: 'pkg' }, c);
    expect(asked[0]).toBe('Run in pkg:\nnpm test');
    expect(executedCommands[0]).toBe(inDirectory('/work/app/pkg', 'npm test', process.platform));
    expect(out).toBe('exit code 0\nok 3 tests\nPASS');
  });
  it('reports a failing exit code and runs nothing when denied', async () => {
    __setShellBehavior(() => ({ chunks: ['boom'], exitCode: 2 }));
    expect(await run('run_command', { command: 'make' })).toBe('exit code 2\nboom');
    expect(await run('run_command', { command: 'rm -rf build' }, ctx(false).ctx)).toBe(DECLINED_BY_USER);
    expect(executedCommands).toEqual([inDirectory('/work/app', 'make', process.platform)]);
  });
  it("notes that PowerShell's exit code 1 means any failure", async () => {
    const platform = Object.getOwnPropertyDescriptor(process, 'platform')!;
    Object.defineProperty(process, 'platform', { value: 'win32' });
    try {
      __setShellBehavior(() => ({ chunks: [], exitCode: 1 }));
      expect(await run('run_command', { command: 'cmd /c exit 3' })).toMatch(/^exit code 1 \(PowerShell reports any failure as 1;.*\n\(no output\)$/);
    } finally {
      Object.defineProperty(process, 'platform', platform);
    }
    __setShellBehavior(() => ({ chunks: [], exitCode: 1 }));
    expect(await run('run_command', { command: 'false' })).toBe('exit code 1\n(no output)');
  });
  it('stops a command that runs past its timeout', async () => {
    __setShellBehavior(() => 'hang');
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const pending = run('run_command', { command: 'sleep 999', timeout_seconds: 1 });
      await vi.advanceTimersByTimeAsync(2500);
      expect(await pending).toMatch(/^timed out after 1s and was stopped/);
      expect(sentText).toContain('\x03');
    } finally {
      vi.useRealTimers();
    }
  });
  it('interrupts the command when the turn is stopped', async () => {
    __setShellBehavior(() => 'hang');
    const controller = new AbortController();
    const pending = run('run_command', { command: 'sleep 999' }, ctx(true).ctx, controller.signal);
    await new Promise((r) => setTimeout(r, 20));
    controller.abort();
    await expect(pending).rejects.toThrow(/cancelled/);
    expect(sentText).toContain('\x03');
  });
  it('explains when shell integration is unavailable', async () => {
    __setShellIntegrationAvailable(false);
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const pending = run('run_command', { command: 'ls' });
      const assertion = expect(pending).rejects.toThrow(/shell integration/);
      await vi.advanceTimersByTimeAsync(11_000);
      await assertion;
    } finally {
      vi.useRealTimers();
    }
  });
  it('refuses a working directory outside the workspace', async () => {
    await expect(run('run_command', { command: 'ls', cwd: '../..' })).rejects.toThrow(/outside/);
  });
});

describe('helpers and availability', () => {
  it('strips terminal escape sequences', () => {
    expect(cleanTerminalOutput('\x1b]633;E;ls\x07\x1b[1;31mred\x1b[0m\r\nline\x1b]0;title\x1b\\')).toBe('red\nline');
  });
  it('quotes the directory for POSIX shells and PowerShell', () => {
    expect(inDirectory("/w/it's", 'ls', 'linux')).toBe("cd '/w/it'\\''s' && ls");
    expect(inDirectory("C:\\w\\it's", 'dir', 'win32')).toBe("Set-Location -LiteralPath 'C:\\w\\it''s'; dir");
  });
  it('offers the write tools only when enabled and the workspace is trusted', () => {
    expect(writeToolsFor(true).map((t) => t.name)).toEqual(['edit_file', 'create_file', 'run_command']);
    expect(writeToolsFor(false)).toEqual([]);
    __setTrusted(false);
    expect(writeToolsFor(true)).toEqual([]);
    expect(WRITE_TOOLS.every((t) => t.selfApproving)).toBe(true);
  });
});
