import * as fs from 'node:fs/promises';
import * as vscode from 'vscode';

// HUPI's own read-only workspace tools for the agent. They're internal to
// HUPI (not registered with vscode.lm.registerTool), so other agents in
// VS Code don't gain anything through HUPI. Read-only, so with the default
// approval mode they run without asking.
//
// Everything stays inside the open workspace folders: relative paths
// resolve against them, absolute paths must be inside one, ".." can't
// climb out, and on a local disk a symlink pointing outside is refused.
// Files that commonly hold secrets are refused too: their contents would
// be sent to the model provider.

export interface HupiTool {
  name: string;
  description: string;
  parameters: object;
  run(args: Record<string, unknown>, signal: AbortSignal): Promise<string>;
}

const EXCLUDE = '**/{node_modules,.git,dist,out,build,.venv,__pycache__}/**';
const MAX_READ_BYTES = 512 * 1024;
const MAX_SCAN_FILE_BYTES = 1024 * 1024;
const MAX_SCAN_FILES = 2000;

// ---- paths ------------------------------------------------------------------

export interface FolderLike {
  name: string;
  uri: vscode.Uri;
}

export interface ResolvedPath {
  folder: FolderLike;
  uri: vscode.Uri;
  /** Workspace-relative path with "/" separators ("" for the folder itself). */
  relative: string;
}

/** Resolves a path the model gave into one of the workspace folders, or
 *  throws if it would land outside them. */
export function resolveInWorkspace(folders: readonly FolderLike[], input: string): ResolvedPath {
  if (folders.length === 0) {
    throw new Error('no workspace folder is open');
  }
  let p = input.trim().replace(/\\/g, '/');
  let folder = folders[0];
  const absolute = p.startsWith('/') || /^[a-zA-Z]:\//.test(p);
  if (absolute) {
    const match = folders.find((f) => {
      const root = f.uri.path.replace(/\/+$/, '');
      return p === root || p.startsWith(root + '/');
    });
    if (!match) {
      throw new Error(`"${input}" is outside the open workspace`);
    }
    folder = match;
    p = p.slice(match.uri.path.replace(/\/+$/, '').length);
  } else if (folders.length > 1) {
    const first = p.split('/')[0];
    const named = folders.find((f) => f.name === first);
    if (named) {
      folder = named;
      p = p.slice(first.length);
    }
  }
  const segments: string[] = [];
  for (const seg of p.split('/')) {
    if (seg === '' || seg === '.') {
      continue;
    }
    if (seg === '..') {
      if (segments.length === 0) {
        throw new Error(`"${input}" is outside the open workspace`);
      }
      segments.pop();
      continue;
    }
    segments.push(seg);
  }
  return { folder, uri: segments.length ? vscode.Uri.joinPath(folder.uri, ...segments) : folder.uri, relative: segments.join('/') };
}

/** On a local disk, refuses a path whose real location (after symlinks)
 *  is outside its workspace folder. A path that doesn't exist yet passes;
 *  reading it will fail anyway. */
export async function ensureNoSymlinkEscape(folderPath: string, targetPath: string): Promise<void> {
  let real: string;
  let root: string;
  try {
    [real, root] = await Promise.all([fs.realpath(targetPath), fs.realpath(folderPath)]);
  } catch {
    return;
  }
  if (real !== root && !real.startsWith(root.endsWith('/') ? root : root + '/')) {
    throw new Error('that path links to a location outside the open workspace');
  }
}

const SECRET_NAME = /^(\.env(\..*)?|\.netrc|\.npmrc|\.pypirc|\.git-credentials|id_(rsa|dsa|ecdsa|ed25519)|.*\.(pem|key|p12|pfx|jks|keystore))$/i;

/** True for files that commonly hold secrets (keys, tokens, .env). */
export function looksSecret(relative: string): boolean {
  const parts = relative.split('/');
  const name = parts[parts.length - 1];
  if (SECRET_NAME.test(name) && !/^\.env\.(example|sample|template)$/i.test(name)) {
    return true;
  }
  return parts.includes('.ssh') || (name === 'credentials' && parts.includes('.aws'));
}

function folders(): FolderLike[] {
  return (vscode.workspace.workspaceFolders ?? []).map((f) => ({ name: f.name, uri: f.uri }));
}

function relativePath(uri: vscode.Uri): string {
  return vscode.workspace.asRelativePath(uri, (vscode.workspace.workspaceFolders?.length ?? 0) > 1);
}

async function readText(r: ResolvedPath): Promise<string> {
  if (looksSecret(r.relative)) {
    throw new Error(`HUPI's agent doesn't read files that commonly hold secrets (${r.relative})`);
  }
  if (r.uri.scheme === 'file') {
    await ensureNoSymlinkEscape(r.folder.uri.fsPath, r.uri.fsPath);
  }
  const bytes = await vscode.workspace.fs.readFile(r.uri);
  if (bytes.byteLength > MAX_READ_BYTES) {
    throw new Error(`${r.relative} is ${bytes.byteLength} bytes; only files up to ${MAX_READ_BYTES} bytes can be read`);
  }
  if (bytes.includes(0)) {
    throw new Error(`${r.relative} looks like a binary file`);
  }
  return new TextDecoder().decode(bytes);
}

function str(args: Record<string, unknown>, key: string): string | undefined {
  const v = args[key];
  return typeof v === 'string' && v.trim() !== '' ? v : undefined;
}

function num(args: Record<string, unknown>, key: string, def: number, max: number): number {
  const v = args[key];
  return typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.min(Math.floor(v), max) : def;
}

function checkAbort(signal: AbortSignal): void {
  if (signal.aborted) {
    throw new Error('cancelled');
  }
}

// ---- the tools ----------------------------------------------------------------

const readFile: HupiTool = {
  name: 'read_file',
  description:
    'Read a text file from the open workspace. Path is relative to the workspace (or absolute inside it). Optionally read only lines start_line..end_line (1-based). Returns the text with line numbers.',
  parameters: {
    type: 'object',
    properties: {
      path: { type: 'string', description: 'File path, relative to the workspace' },
      start_line: { type: 'number', description: 'First line to read (1-based, optional)' },
      end_line: { type: 'number', description: 'Last line to read (optional)' },
    },
    required: ['path'],
  },
  async run(args) {
    const path = str(args, 'path');
    if (!path) {
      throw new Error('path is required');
    }
    const r = resolveInWorkspace(folders(), path);
    const lines = (await readText(r)).split(/\r?\n/);
    const start = num(args, 'start_line', 1, lines.length);
    const end = Math.max(start, num(args, 'end_line', lines.length, lines.length));
    const width = String(end).length;
    const body = lines
      .slice(start - 1, end)
      .map((l, i) => `${String(start + i).padStart(width)}  ${l}`)
      .join('\n');
    return `${r.relative} (lines ${start}-${end} of ${lines.length}):\n${body}`;
  },
};

const listFiles: HupiTool = {
  name: 'list_files',
  description:
    'List files in the open workspace matching a glob pattern (default "**/*"), e.g. "src/**/*.ts". Skips node_modules, .git and build output.',
  parameters: {
    type: 'object',
    properties: {
      glob: { type: 'string', description: 'Glob pattern relative to the workspace, e.g. "src/**/*.go"' },
      max_results: { type: 'number', description: 'Maximum files to list (default 200, max 1000)' },
    },
  },
  async run(args) {
    if (folders().length === 0) {
      throw new Error('no workspace folder is open');
    }
    const max = num(args, 'max_results', 200, 1000);
    const found = await vscode.workspace.findFiles(str(args, 'glob') ?? '**/*', EXCLUDE, max + 1);
    const paths = found.map(relativePath).sort();
    const shown = paths.slice(0, max);
    if (shown.length === 0) {
      return 'No files match.';
    }
    return shown.join('\n') + (paths.length > max ? `\n… (more files match; narrow the glob)` : '');
  },
};

const searchText: HupiTool = {
  name: 'search_text',
  description:
    'Search the text of files in the open workspace. Returns matching lines as "path:line: text". Case-insensitive; set is_regex to use a regular expression. Optionally limit to files matching a glob.',
  parameters: {
    type: 'object',
    properties: {
      query: { type: 'string', description: 'Text (or regular expression) to search for' },
      glob: { type: 'string', description: 'Only search files matching this glob, e.g. "**/*.ts"' },
      is_regex: { type: 'boolean', description: 'Treat query as a regular expression' },
      max_results: { type: 'number', description: 'Maximum matching lines (default 50, max 200)' },
    },
    required: ['query'],
  },
  async run(args, signal) {
    const query = str(args, 'query');
    if (!query) {
      throw new Error('query is required');
    }
    if (folders().length === 0) {
      throw new Error('no workspace folder is open');
    }
    let re: RegExp;
    try {
      re = args.is_regex === true ? new RegExp(query, 'i') : new RegExp(query.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i');
    } catch (err) {
      throw new Error(`invalid regular expression: ${(err as Error).message}`);
    }
    const max = num(args, 'max_results', 50, 200);
    const files = await vscode.workspace.findFiles(str(args, 'glob') ?? '**/*', EXCLUDE, MAX_SCAN_FILES);
    const hits: string[] = [];
    let scanned = 0;
    for (const uri of files) {
      checkAbort(signal);
      const rel = relativePath(uri);
      if (looksSecret(rel)) {
        continue;
      }
      // Same symlink rule as read_file: never read through a link that
      // points outside the workspace.
      const folder = vscode.workspace.getWorkspaceFolder(uri);
      if (uri.scheme === 'file' && folder) {
        try {
          await ensureNoSymlinkEscape(folder.uri.fsPath, uri.fsPath);
        } catch {
          continue;
        }
      }
      let bytes: Uint8Array;
      try {
        bytes = await vscode.workspace.fs.readFile(uri);
      } catch {
        continue;
      }
      if (bytes.byteLength > MAX_SCAN_FILE_BYTES || bytes.includes(0)) {
        continue;
      }
      scanned++;
      const lines = new TextDecoder().decode(bytes).split(/\r?\n/);
      for (let i = 0; i < lines.length && hits.length < max; i++) {
        if (re.test(lines[i])) {
          const text = lines[i].trim();
          hits.push(`${rel}:${i + 1}: ${text.length > 200 ? text.slice(0, 200) + '…' : text}`);
        }
      }
      if (hits.length >= max) {
        break;
      }
    }
    if (hits.length === 0) {
      return `No matches in ${scanned} files.`;
    }
    return hits.join('\n') + (hits.length >= max ? `\n… (stopped at ${max} matches)` : '');
  },
};

const SEVERITY = ['error', 'warning', 'info', 'hint'];

const getDiagnostics: HupiTool = {
  name: 'get_diagnostics',
  description:
    'Get the problems (errors, warnings) VS Code currently reports, for one file or the whole workspace. Returns "path:line:column severity: message".',
  parameters: {
    type: 'object',
    properties: { path: { type: 'string', description: 'Only this file (optional; default: all files)' } },
  },
  async run(args) {
    const path = str(args, 'path');
    let entries: [vscode.Uri, readonly vscode.Diagnostic[]][];
    if (path) {
      const r = resolveInWorkspace(folders(), path);
      entries = [[r.uri, vscode.languages.getDiagnostics(r.uri)]];
    } else {
      entries = vscode.languages.getDiagnostics();
    }
    const out: string[] = [];
    for (const [uri, diags] of entries) {
      for (const d of diags) {
        const pos = `${d.range.start.line + 1}:${d.range.start.character + 1}`;
        const source = d.source ? ` (${d.source})` : '';
        out.push(`${relativePath(uri)}:${pos} ${SEVERITY[d.severity] ?? 'info'}: ${d.message}${source}`);
        if (out.length >= 200) {
          return out.join('\n') + '\n… (more problems not shown)';
        }
      }
    }
    return out.length ? out.join('\n') : 'No problems reported.';
  },
};

const getSelection: HupiTool = {
  name: 'get_selection',
  description: "Get the file open in the user's active editor, the selected line range, and the selected text.",
  parameters: { type: 'object', properties: {} },
  async run() {
    const editor = vscode.window.activeTextEditor;
    if (!editor) {
      return 'No file is open in the editor.';
    }
    const rel = relativePath(editor.document.uri);
    const sel = editor.selection;
    if (sel.isEmpty) {
      return `${rel} (${editor.document.languageId}) is open; nothing is selected (cursor at line ${sel.active.line + 1}).`;
    }
    if (looksSecret(rel)) {
      return `${rel} is open with a selection, but HUPI's agent doesn't read files that commonly hold secrets.`;
    }
    const text = editor.document.getText(sel);
    return `${rel} (${editor.document.languageId}), lines ${sel.start.line + 1}-${sel.end.line + 1} selected:\n${text}`;
  },
};

/** HUPI's read-only workspace tools. */
export const WORKSPACE_TOOLS: readonly HupiTool[] = [readFile, listFiles, searchText, getDiagnostics, getSelection];
