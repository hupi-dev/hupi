import * as vscode from 'vscode';
import { closeDiffTab, createVirtualDiffProvider, type VirtualDiffProvider } from '../diffContentProvider';
import { DECLINED_BY_USER } from './agentLoop';
import { runInTerminal } from './terminalRunner';
import { folders, looksSecret, num, readText, resolveInWorkspace, str, type HupiTool, type ToolContext } from './workspaceTools';

// HUPI's agent tools that change things: edit or create a file, run a
// command. Every call asks the user first (ToolContext.ask, shown on the
// call's card in the sidebar), whatever hupi.agent.approval says; an edit
// opens VS Code's diff view of the exact change while it asks. They're
// only offered in a trusted workspace (see writeToolsFor).

const DIFF_SCHEME = 'hupi-agent-diff';
const MAX_FILE_CHARS = 512 * 1024;
const DEFAULT_TIMEOUT_S = 60;
const MAX_TIMEOUT_S = 600;
const MAX_OUTPUT_CHARS = 20_000;

let diffDocs: VirtualDiffProvider | undefined;
function diffProvider(): VirtualDiffProvider {
  diffDocs ??= createVirtualDiffProvider(DIFF_SCHEME);
  return diffDocs;
}

/** Shows before/after in VS Code's diff view while asking; closes it after. */
async function askWithDiff(ctx: ToolContext | undefined, rel: string, before: string, after: string, detail: string): Promise<boolean> {
  if (!ctx) {
    throw new Error('this tool needs the user to approve it, and no approval channel is available');
  }
  const stamp = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  const name = rel.split('/').pop() || 'file';
  const beforeUri = vscode.Uri.parse(`${DIFF_SCHEME}:/before-${stamp}/${name}`);
  const afterUri = vscode.Uri.parse(`${DIFF_SCHEME}:/after-${stamp}/${name}`);
  const docs = diffProvider();
  docs.set(beforeUri, before);
  docs.set(afterUri, after);
  try {
    await vscode.commands.executeCommand('vscode.diff', beforeUri, afterUri, `HUPI agent: ${rel} (left: current, right: proposed)`, { preview: true });
    return await ctx.ask(detail);
  } finally {
    docs.delete(beforeUri);
    docs.delete(afterUri);
    await closeDiffTab(beforeUri, afterUri);
  }
}

function lineCount(s: string): number {
  return s === '' ? 0 : s.split('\n').length;
}

function countOccurrences(haystack: string, needle: string): number {
  let n = 0;
  for (let i = haystack.indexOf(needle); i !== -1; i = haystack.indexOf(needle, i + needle.length)) {
    n++;
  }
  return n;
}

/** Writes the whole document through VS Code (so an open editor, and its
 *  undo history, see the change) and saves it. */
async function replaceDocument(uri: vscode.Uri, next: string): Promise<void> {
  const doc = await vscode.workspace.openTextDocument(uri);
  const edit = new vscode.WorkspaceEdit();
  edit.replace(uri, new vscode.Range(doc.positionAt(0), doc.positionAt(doc.getText().length)), next);
  if (!(await vscode.workspace.applyEdit(edit))) {
    throw new Error('VS Code did not apply the edit');
  }
  await doc.save();
}

const editFile: HupiTool = {
  name: 'edit_file',
  selfApproving: true,
  description:
    'Edit a file in the open workspace by replacing one exact piece of text. old_text must appear exactly once in the file (include enough surrounding lines to make it unique); new_text replaces it. The user sees a diff and must approve. Read the file first so old_text matches exactly, including indentation.',
  parameters: {
    type: 'object',
    properties: {
      path: { type: 'string', description: 'File path, relative to the workspace' },
      old_text: { type: 'string', description: 'The exact existing text to replace (must occur exactly once)' },
      new_text: { type: 'string', description: 'The replacement text' },
    },
    required: ['path', 'old_text', 'new_text'],
  },
  async run(args, _signal, ctx) {
    const path = str(args, 'path');
    const oldText = typeof args.old_text === 'string' ? args.old_text : undefined;
    const newText = typeof args.new_text === 'string' ? args.new_text : undefined;
    if (!path || !oldText || newText === undefined) {
      throw new Error('path, old_text and new_text are required');
    }
    const r = resolveInWorkspace(folders(), path);
    if (looksSecret(r.relative)) {
      throw new Error(`HUPI's agent doesn't edit files that commonly hold secrets (${r.relative})`);
    }
    // readText enforces the workspace, symlink, size and binary rules; an
    // open editor's text (with any unsaved changes) wins over the disk.
    const onDisk = await readText(r);
    const open = vscode.workspace.textDocuments.find((d) => d.uri.toString() === r.uri.toString());
    const current = open ? open.getText() : onDisk;
    const found = countOccurrences(current, oldText);
    if (found === 0) {
      throw new Error(`old_text was not found in ${r.relative}; read the file again and copy the text exactly`);
    }
    if (found > 1) {
      throw new Error(`old_text occurs ${found} times in ${r.relative}; include more surrounding lines so it is unique`);
    }
    const next = current.replace(oldText, () => newText);
    if (next.length > MAX_FILE_CHARS) {
      throw new Error(`the edited file would be larger than ${MAX_FILE_CHARS} characters`);
    }
    const detail = `Edit ${r.relative}: replace ${lineCount(oldText)} line(s) with ${lineCount(newText)} line(s). The diff is open in the editor.`;
    if (!(await askWithDiff(ctx, r.relative, current, next, detail))) {
      return DECLINED_BY_USER;
    }
    await replaceDocument(r.uri, next);
    return `Edited ${r.relative}.`;
  },
};

const createFile: HupiTool = {
  name: 'create_file',
  selfApproving: true,
  description:
    'Create a new file in the open workspace with the given content (parent folders are created). Fails if the file already exists — use edit_file to change an existing file. The user sees the content and must approve.',
  parameters: {
    type: 'object',
    properties: {
      path: { type: 'string', description: 'New file path, relative to the workspace' },
      content: { type: 'string', description: 'The full file content' },
    },
    required: ['path', 'content'],
  },
  async run(args, _signal, ctx) {
    const path = str(args, 'path');
    const content = typeof args.content === 'string' ? args.content : undefined;
    if (!path || content === undefined) {
      throw new Error('path and content are required');
    }
    const r = resolveInWorkspace(folders(), path);
    if (r.relative === '') {
      throw new Error('a file path is required, not the workspace folder');
    }
    if (looksSecret(r.relative)) {
      throw new Error(`HUPI's agent doesn't create files that commonly hold secrets (${r.relative})`);
    }
    if (content.length > MAX_FILE_CHARS) {
      throw new Error(`content is larger than ${MAX_FILE_CHARS} characters`);
    }
    const exists = await vscode.workspace.fs.stat(r.uri).then(
      () => true,
      () => false,
    );
    if (exists) {
      throw new Error(`${r.relative} already exists; use edit_file to change it`);
    }
    const detail = `Create ${r.relative} (${lineCount(content)} line(s)). The content is open in the editor.`;
    if (!(await askWithDiff(ctx, r.relative, '', content, detail))) {
      return DECLINED_BY_USER;
    }
    await vscode.workspace.fs.writeFile(r.uri, new TextEncoder().encode(content));
    return `Created ${r.relative}.`;
  },
};

const runCommand: HupiTool = {
  name: 'run_command',
  selfApproving: true,
  description:
    'Run a shell command in the open workspace (in a visible "HUPI agent" terminal) and return its exit code and output — e.g. to run tests or a build. The user sees the exact command and must approve. Avoid commands that wait for input or never exit.',
  parameters: {
    type: 'object',
    properties: {
      command: { type: 'string', description: 'The shell command line to run' },
      cwd: { type: 'string', description: 'Directory to run in, relative to the workspace (default: the workspace root)' },
      timeout_seconds: { type: 'number', description: `Stop the command after this long (default ${DEFAULT_TIMEOUT_S}, max ${MAX_TIMEOUT_S})` },
    },
    required: ['command'],
  },
  async run(args, signal, ctx) {
    const command = str(args, 'command');
    if (!command) {
      throw new Error('command is required');
    }
    if (!ctx) {
      throw new Error('this tool needs the user to approve it, and no approval channel is available');
    }
    const dir = resolveInWorkspace(folders(), str(args, 'cwd') ?? '.');
    const where = dir.relative === '' ? 'the workspace root' : dir.relative;
    if (!(await ctx.ask(`Run in ${where}:\n${command}`))) {
      return DECLINED_BY_USER;
    }
    const timeout = num(args, 'timeout_seconds', DEFAULT_TIMEOUT_S, MAX_TIMEOUT_S);
    const res = await runInTerminal(command, dir.uri, timeout * 1000, MAX_OUTPUT_CHARS, signal);
    const status = res.timedOut ? `timed out after ${timeout}s and was stopped` : `exit code ${res.exitCode ?? 'unknown'}`;
    return `${status}\n${res.output.trim() || '(no output)'}`;
  },
};

export const WRITE_TOOLS: readonly HupiTool[] = [editFile, createFile, runCommand];

/** The write/run tools to offer: none in an untrusted workspace (VS
 *  Code's Restricted Mode) or when hupi.agent.writeTools is off. */
export function writeToolsFor(enabled: boolean): readonly HupiTool[] {
  return enabled && vscode.workspace.isTrusted ? WRITE_TOOLS : [];
}

/** For tests: forget the cached diff provider. */
export function __resetWriteTools(): void {
  diffDocs = undefined;
}

