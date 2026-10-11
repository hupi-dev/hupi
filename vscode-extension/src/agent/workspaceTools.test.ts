import * as fs from 'node:fs/promises';
import * as os from 'node:os';
import * as path from 'node:path';
import { beforeEach, describe, expect, it } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setActiveTextEditor, __setFileContents, __setWorkspaceFolders, findFiles, getDiagnostics, Position, Range, Uri } from '../test/vscode-mock';
import { buildRegistry } from './toolRegistry';
import { ensureNoSymlinkEscape, isInsideFolder, looksSecret, resolveInWorkspace, WORKSPACE_TOOLS, type FolderLike } from './workspaceTools';

const root = Uri.file('/work/app');
const lib = Uri.file('/work/lib');
const file = (base: Uri, rel: string) => Uri.joinPath(base, ...rel.split('/'));
const put = (base: Uri, rel: string, text: string) => __setFileContents(file(base, rel), new TextEncoder().encode(text));
const tool = (name: string) => WORKSPACE_TOOLS.find((t) => t.name === name)!;
const run = (name: string, args: Record<string, unknown> = {}) => tool(name).run(args, new AbortController().signal);

beforeEach(() => {
  __resetVscodeMock();
  __setWorkspaceFolders([{ name: 'app', uri: root }]);
});

describe('resolveInWorkspace', () => {
  const folders = [
    { name: 'app', uri: root },
    { name: 'lib', uri: lib },
  ] as unknown as FolderLike[];
  it('resolves relative, named-folder and absolute paths inside the workspace', () => {
    expect(resolveInWorkspace(folders, 'src/main.ts')).toMatchObject({ relative: 'src/main.ts', folder: { name: 'app' } });
    expect(resolveInWorkspace(folders, 'lib/util.ts')).toMatchObject({ relative: 'util.ts', folder: { name: 'lib' } });
    expect(resolveInWorkspace(folders, '/work/lib/a/b.ts')).toMatchObject({ relative: 'a/b.ts', folder: { name: 'lib' } });
    expect(resolveInWorkspace(folders, './src/../README.md').relative).toBe('README.md');
    expect(resolveInWorkspace(folders, 'src\\win\\path.ts').relative).toBe('src/win/path.ts');
  });
  it('refuses anything outside the workspace', () => {
    for (const p of ['../secret.txt', 'src/../../etc/passwd', '/etc/passwd', '/work/application/x', 'C:/Windows/system.ini']) {
      expect(() => resolveInWorkspace(folders, p), p).toThrow(/outside the open workspace/);
    }
    expect(() => resolveInWorkspace([], 'a')).toThrow(/no workspace folder/);
  });
});

describe('resolveInWorkspace on Windows', () => {
  // vscode.Uri's path for C:\Users\Public\hupi proj's (fsPath gives "c:\…").
  const win = [{ name: "hupi proj's", uri: Uri.file("/C:/Users/Public/hupi proj's") }] as unknown as FolderLike[];
  it('accepts absolute drive paths inside the folder, in any case and with either separator', () => {
    for (const p of ["c:\\Users\\Public\\hupi proj's\\src\\config.ts", "C:/Users/Public/hupi proj's/src/config.ts", "/c:/users/public/HUPI PROJ'S/src/config.ts"]) {
      expect(resolveInWorkspace(win, p).relative, p).toBe('src/config.ts');
    }
    expect(resolveInWorkspace(win, "c:\\Users\\Public\\hupi proj's").relative).toBe('');
  });
  it('refuses drive paths outside the folder', () => {
    for (const p of ['C:\\Windows\\win.ini', "c:\\Users\\Public\\hupi proj's2\\x", 'D:/Users/Public/x', "c:\\Users\\Public\\hupi proj's\\..\\..\\x"]) {
      expect(() => resolveInWorkspace(win, p), p).toThrow(/outside the open workspace/);
    }
  });
});

describe('isInsideFolder', () => {
  it('follows Windows rules: either case, either separator, other drives outside', () => {
    const root = "C:\\Users\\Public\\hupi proj's";
    expect(isInsideFolder(root, "C:\\Users\\Public\\hupi proj's\\src\\config.ts", path.win32)).toBe(true);
    expect(isInsideFolder(root, "c:\\users\\public\\HUPI PROJ'S\\src", path.win32)).toBe(true);
    expect(isInsideFolder(root, root, path.win32)).toBe(true);
    expect(isInsideFolder(root, "C:\\Users\\Public\\hupi proj's2\\x", path.win32)).toBe(false);
    expect(isInsideFolder(root, 'C:\\Users\\secret.txt', path.win32)).toBe(false);
    expect(isInsideFolder(root, 'D:\\x', path.win32)).toBe(false);
  });
  it('follows POSIX rules', () => {
    expect(isInsideFolder('/work/app', '/work/app/..config/x', path.posix)).toBe(true);
    expect(isInsideFolder('/work/app', '/work/application', path.posix)).toBe(false);
    expect(isInsideFolder('/work/app', '/work/App/x', path.posix)).toBe(false);
  });
});

describe('ensureNoSymlinkEscape', () => {
  it('refuses a symlink that points outside the folder, allows ordinary files', async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'hupi-ws-'));
    const outside = await fs.mkdtemp(path.join(os.tmpdir(), 'hupi-out-'));
    try {
      await fs.writeFile(path.join(outside, 'secret.txt'), 'x');
      await fs.writeFile(path.join(dir, 'ok.txt'), 'x');
      await fs.symlink(path.join(outside, 'secret.txt'), path.join(dir, 'link.txt'));
      await expect(ensureNoSymlinkEscape(dir, path.join(dir, 'ok.txt'))).resolves.toBeUndefined();
      await expect(ensureNoSymlinkEscape(dir, path.join(dir, 'link.txt'))).rejects.toThrow(/outside the open workspace/);
      await expect(ensureNoSymlinkEscape(dir, path.join(dir, 'missing.txt'))).resolves.toBeUndefined();
    } finally {
      await fs.rm(dir, { recursive: true, force: true });
      await fs.rm(outside, { recursive: true, force: true });
    }
  });
});

describe('search_text and symlinks', () => {
  it('never searches through a symlink that points outside the workspace', async () => {
    const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'hupi-ws-'));
    const outside = await fs.mkdtemp(path.join(os.tmpdir(), 'hupi-out-'));
    try {
      await fs.writeFile(path.join(outside, 'secret.txt'), 'refresh outside secret');
      await fs.writeFile(path.join(dir, 'inside.txt'), 'refresh inside');
      await fs.symlink(path.join(outside, 'secret.txt'), path.join(dir, 'link.txt'));
      const folder = Uri.file(dir);
      __setWorkspaceFolders([{ name: 'ws', uri: folder }]);
      put(folder, 'inside.txt', 'refresh inside');
      put(folder, 'link.txt', 'refresh outside secret');
      findFiles.mockResolvedValue([file(folder, 'inside.txt'), file(folder, 'link.txt')]);
      const out = await run('search_text', { query: 'refresh' });
      expect(out).toBe('inside.txt:1: refresh inside');
    } finally {
      await fs.rm(dir, { recursive: true, force: true });
      await fs.rm(outside, { recursive: true, force: true });
    }
  });
});

describe('looksSecret', () => {
  it('flags common secret files but not ordinary or example files', () => {
    for (const p of ['.env', 'config/.env.production', 'certs/server.pem', 'deploy/tls.key', '.ssh/config', 'id_ed25519', '.aws/credentials', '.npmrc', '.git-credentials']) {
      expect(looksSecret(p), p).toBe(true);
    }
    for (const p of ['src/env.ts', '.env.example', 'README.md', 'keys.go', 'id_ed25519.pub', 'docs/credentials.md']) {
      expect(looksSecret(p), p).toBe(false);
    }
  });
});

describe('read_file', () => {
  it('returns numbered lines, optionally a range', async () => {
    put(root, 'src/main.ts', 'line one\nline two\nline three\n');
    expect(await run('read_file', { path: 'src/main.ts' })).toBe('src/main.ts (lines 1-4 of 4):\n1  line one\n2  line two\n3  line three\n4  ');
    expect(await run('read_file', { path: 'src/main.ts', start_line: 2, end_line: 2 })).toBe('src/main.ts (lines 2-2 of 4):\n2  line two');
  });
  it('refuses secrets, binaries and escapes', async () => {
    put(root, '.env', 'API_KEY=x');
    __setFileContents(file(root, 'logo.png'), new Uint8Array([137, 80, 0, 1]));
    await expect(run('read_file', { path: '.env' })).rejects.toThrow(/commonly hold secrets/);
    await expect(run('read_file', { path: 'logo.png' })).rejects.toThrow(/binary/);
    await expect(run('read_file', { path: '../other/x' })).rejects.toThrow(/outside/);
    await expect(run('read_file', {})).rejects.toThrow(/path is required/);
  });
});

describe('list_files and search_text', () => {
  beforeEach(() => {
    put(root, 'src/auth/login.ts', 'export function login() {\n  // TODO: refresh token\n}\n');
    put(root, 'src/auth/token.ts', 'const refreshToken = 1;\n');
    put(root, '.env', 'REFRESH_TOKEN=supersecret\n');
    findFiles.mockResolvedValue([file(root, 'src/auth/login.ts'), file(root, 'src/auth/token.ts'), file(root, '.env')]);
  });
  it('lists workspace-relative paths, excluding dependency and build folders', async () => {
    expect(await run('list_files', { glob: 'src/**' })).toBe('.env\nsrc/auth/login.ts\nsrc/auth/token.ts');
    expect(findFiles).toHaveBeenCalledWith('src/**', expect.stringContaining('node_modules'), 201);
  });
  it('finds matching lines case-insensitively, skipping secret files', async () => {
    const out = await run('search_text', { query: 'refresh' });
    expect(out).toBe('src/auth/login.ts:2: // TODO: refresh token\nsrc/auth/token.ts:1: const refreshToken = 1;');
    expect(out).not.toContain('supersecret');
  });
  it('supports regular expressions, escapes plain queries, and caps results', async () => {
    expect(await run('search_text', { query: 'refresh\\w+', is_regex: true })).toBe('src/auth/token.ts:1: const refreshToken = 1;');
    expect(await run('search_text', { query: 'login()' })).toContain('src/auth/login.ts:1:');
    expect(await run('search_text', { query: 'e', max_results: 1 })).toMatch(/stopped at 1 matches/);
    await expect(run('search_text', { query: '(', is_regex: true })).rejects.toThrow(/invalid regular expression/);
  });
});

describe('get_diagnostics and get_selection', () => {
  it('formats problems for the workspace or one file', async () => {
    const d = { range: new Range(new Position(4, 2), new Position(4, 9)), severity: 0, message: "Cannot find name 'tokn'.", source: 'ts' };
    getDiagnostics.mockImplementation((uri?: unknown) => (uri ? [d] : [[file(root, 'src/a.ts'), [d]]]));
    expect(await run('get_diagnostics')).toBe("src/a.ts:5:3 error: Cannot find name 'tokn'. (ts)");
    expect(await run('get_diagnostics', { path: 'src/a.ts' })).toBe("src/a.ts:5:3 error: Cannot find name 'tokn'. (ts)");
  });
  it('reports the active selection', async () => {
    expect(await run('get_selection')).toBe('No file is open in the editor.');
    const sel = { isEmpty: false, start: new Position(1, 0), end: new Position(2, 5), active: new Position(2, 5) };
    __setActiveTextEditor({ selection: sel, document: { uri: file(root, 'src/a.ts'), languageId: 'typescript', getText: () => 'const a = 1;\nlet b' } });
    expect(await run('get_selection')).toBe('src/a.ts (typescript), lines 2-3 selected:\nconst a = 1;\nlet b');
  });
});

describe('registration', () => {
  it('registers HUPI tools first with their own names and parses arguments', async () => {
    put(root, 'a.txt', 'hello');
    const reg = buildRegistry([{ name: 'read_file', description: 'mcp', tags: ['mcp'], inputSchema: {} } as unknown as vscode.LanguageModelToolInformation], async () => 'mcp', WORKSPACE_TOOLS);
    expect([...reg.keys()]).toEqual(['read_file', 'list_files', 'search_text', 'get_diagnostics', 'get_selection', 'read_file_2']);
    expect(await reg.get('read_file')!.run('{"path":"a.txt"}', new AbortController().signal)).toContain('hello');
    await expect(reg.get('read_file')!.run('[1]', new AbortController().signal)).rejects.toThrow(/JSON object/);
  });
});
