// A hand-written stand-in for the real `vscode` module — there's no real
// installed package to import at all outside a real VS Code process (the
// extension host injects it at runtime; esbuild leaves it external,
// see esbuild.mjs). Wired in via vitest.config.ts's resolve.alias, so
// `import * as vscode from 'vscode'` in every test file (and in the
// source files under test) resolves here automatically.
//
// TypeScript still type-checks `import ... from 'vscode'` against the
// real @types/vscode ambient declarations regardless of this alias
// (module resolution for typechecking and Vite's runtime alias are
// unrelated mechanisms) — so this file only needs to behave correctly at
// runtime for what the source under test actually calls, not to
// structurally satisfy the real, much larger vscode.* interfaces. Test
// helpers that build fake ExtensionContext/TextEditor/etc. objects
// intentionally cast through `as unknown as vscode.X`, same pattern used
// throughout the VS Code extension testing community.
import { vi } from 'vitest';

// ---- workspace configuration -----------------------------------------

let configValues: Record<string, unknown> = {};

/** Test helper: set the values `workspace.getConfiguration('hupi').get(key, ...)`
 *  returns. Keys match what the source passes to `.get(...)` — e.g.
 *  `{'baseUrl': '...', 'oidc.issuerUrl': '...'}` — not the full
 *  `hupi.oidc.issuerUrl` path, since every real call site here always
 *  uses section `'hupi'`. */
export function __setConfig(values: Record<string, unknown>): void {
  configValues = { ...values };
}

function getConfiguration() {
  return {
    get<T>(key: string, defaultValue?: T): T {
      return (key in configValues ? configValues[key] : defaultValue) as T;
    },
  };
}

// ---- window.* spies -----------------------------------------------------

export const showInputBox = vi.fn<(...args: any[]) => Promise<string | undefined>>();
export const showInformationMessage = vi.fn<(...args: any[]) => Promise<string | undefined>>();
export const showErrorMessage = vi.fn<(...args: any[]) => Promise<string | undefined>>();
export const showWarningMessage = vi.fn<(...args: any[]) => Promise<string | undefined>>();
export const showQuickPick = vi.fn<(...args: any[]) => Promise<unknown>>();
export const withProgress = vi.fn<(...args: any[]) => Promise<unknown>>();
export const registerWebviewViewProvider = vi.fn();
export const createWebviewPanel = vi.fn();
export const openExternal = vi.fn<(...args: any[]) => Promise<boolean>>();
export const applyEdit = vi.fn<(...args: any[]) => Promise<boolean>>();
export const registerTextDocumentContentProvider = vi.fn();
export const registerInlineCompletionItemProvider = vi.fn();
export const tabGroupsClose = vi.fn<(...args: any[]) => Promise<void>>();

let activeTextEditorValue: unknown = undefined;
export function __setActiveTextEditor(editor: unknown): void {
  activeTextEditorValue = editor;
}

let tabGroupsAllValue: { tabs: unknown[] }[] = [];
export function __setTabGroups(groups: { tabs: unknown[] }[]): void {
  tabGroupsAllValue = groups;
}

export const window = {
  get activeTextEditor() {
    return activeTextEditorValue;
  },
  showInputBox,
  showInformationMessage,
  showErrorMessage,
  showWarningMessage,
  showQuickPick,
  withProgress,
  registerWebviewViewProvider,
  createWebviewPanel,
  tabGroups: {
    get all() {
      return tabGroupsAllValue;
    },
    close: tabGroupsClose,
  },
};

export const languages = { registerInlineCompletionItemProvider };

export const ProgressLocation = { Notification: 15 };
export const ViewColumn = { Beside: -2 };
export const InlineCompletionTriggerKind = { Invoke: 0, Automatic: 1 };

// ---- commands.* — registerCommand also records handlers so tests can
// invoke them directly, and executeCommand's default implementation
// dispatches to whatever's registered, same as real VS Code, so a test
// like "the Sign In prompt's action really runs hupi.signIn" doesn't need
// its own separate wiring. ----------------------------------------------

const registeredCommands = new Map<string, (...args: any[]) => unknown>();

export const registerCommand = vi.fn((id: string, handler: (...args: any[]) => unknown) => {
  registeredCommands.set(id, handler);
  return new Disposable(() => registeredCommands.delete(id));
});

export const executeCommand = vi.fn(async (id: string, ...args: unknown[]) => {
  const handler = registeredCommands.get(id);
  return handler ? handler(...args) : undefined;
});

export function __getRegisteredCommand(id: string): ((...args: any[]) => unknown) | undefined {
  return registeredCommands.get(id);
}

export const commands = { registerCommand, executeCommand };

// ---- workspace.* ---------------------------------------------------------

function asRelativePath(uri: { path?: string } | string): string {
  return typeof uri === 'string' ? uri : (uri.path ?? String(uri));
}

let textDocumentsValue: unknown[] = [];
export function __setTextDocuments(docs: unknown[]): void {
  textDocumentsValue = docs;
}

export const workspace = {
  getConfiguration,
  asRelativePath,
  registerTextDocumentContentProvider,
  applyEdit,
  get textDocuments() {
    return textDocumentsValue;
  },
};

// ---- env.* ---------------------------------------------------------------

export const env = { openExternal };

// ---- Uri ------------------------------------------------------------------

export class Uri {
  private constructor(
    public readonly scheme: string,
    public readonly path: string,
    private readonly raw: string,
  ) {}

  static parse(value: string): Uri {
    const schemeMatch = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(value);
    const scheme = schemeMatch?.[1] ?? '';
    const afterScheme = scheme ? value.slice(scheme.length + 1) : value;
    const path = afterScheme.replace(/^\/\/[^/]*/, '').split(/[?#]/)[0];
    return new Uri(scheme, path, value);
  }

  static file(path: string): Uri {
    return new Uri('file', path, `file://${path}`);
  }

  static joinPath(base: Uri, ...segments: string[]): Uri {
    const joined = [base.path.replace(/\/+$/, ''), ...segments].join('/');
    return new Uri(base.scheme, joined, `${base.scheme}:${joined}`);
  }

  toString(): string {
    return this.raw;
  }
}

// ---- Position/Range/InlineCompletionItem — minimal, structural only;
// nothing in this extension's source inspects their internals, it only
// passes them through to real VS Code APIs (or, in tests, back out
// through mocked ones for assertions). ------------------------------

export class Position {
  constructor(
    public readonly line: number,
    public readonly character: number,
  ) {}
}

export class Range {
  constructor(
    public readonly start: unknown,
    public readonly end: unknown,
  ) {}
}

export class InlineCompletionItem {
  constructor(
    public readonly insertText: string,
    public readonly range?: unknown,
  ) {}
}

// ---- Disposable -------------------------------------------------------

export class Disposable {
  constructor(private readonly onDispose?: () => void) {}
  dispose(): void {
    this.onDispose?.();
  }
}

// ---- WorkspaceEdit — records .replace() calls for assertions ----------

export interface RecordedEdit {
  uri: unknown;
  range: unknown;
  newText: string;
}

export class WorkspaceEdit {
  public readonly edits: RecordedEdit[] = [];
  replace(uri: unknown, range: unknown, newText: string): void {
    this.edits.push({ uri, range, newText });
  }
}

// ---- TabInputTextDiff — needs to be a real class for `instanceof` -----

export class TabInputTextDiff {
  constructor(
    public readonly original: Uri,
    public readonly modified: Uri,
  ) {}
}

// ---- chat.* — real classes for ChatRequestTurn/ChatResponseTurn/
// ChatResponseMarkdownPart since chatParticipant.ts's historyToMessages
// tells them apart with `instanceof`, same reasoning as TabInputTextDiff
// above. Real VS Code marks these classes' constructors private (TS-only,
// not a runtime restriction) since application code is only ever meant to
// *receive* them from context.history, never construct one — tests still
// need to build fake history turns, so this mock's constructors are
// deliberately public. -----------------------------------------------

export class ChatRequestTurn {
  constructor(
    public readonly prompt: string,
    public readonly command: string | undefined,
    public readonly references: unknown[],
    public readonly participant: string,
    public readonly toolReferences: unknown[],
  ) {}
}

export class ChatResponseMarkdownPart {
  public readonly value: { value: string };
  constructor(value: string) {
    this.value = { value };
  }
}

export class ChatResponseTurn {
  constructor(
    public readonly response: ChatResponseMarkdownPart[],
    public readonly result: unknown,
    public readonly participant: string,
  ) {}
}

export const createChatParticipant = vi.fn((id: string, handler: (...args: any[]) => unknown) => {
  return {
    id,
    requestHandler: handler,
    iconPath: undefined,
    followupProvider: undefined,
    onDidReceiveFeedback: () => new Disposable(),
    dispose: vi.fn(),
  };
});

export const chat = { createChatParticipant };

// ---- reset ----------------------------------------------------------------

/** Call from beforeEach — resets every spy and every piece of mutable mock
 *  state, so tests never leak into each other. */
export function __resetVscodeMock(): void {
  configValues = {};
  activeTextEditorValue = undefined;
  tabGroupsAllValue = [];
  textDocumentsValue = [];
  registeredCommands.clear();

  showInputBox.mockReset().mockResolvedValue(undefined);
  showInformationMessage.mockReset().mockResolvedValue(undefined);
  showErrorMessage.mockReset().mockResolvedValue(undefined);
  showWarningMessage.mockReset().mockResolvedValue(undefined);
  showQuickPick.mockReset().mockResolvedValue(undefined);
  registerWebviewViewProvider.mockReset().mockReturnValue(new Disposable());
  createWebviewPanel.mockReset();
  openExternal.mockReset().mockResolvedValue(true);
  applyEdit.mockReset().mockResolvedValue(true);
  registerTextDocumentContentProvider.mockReset().mockReturnValue(new Disposable());
  registerInlineCompletionItemProvider.mockReset().mockReturnValue(new Disposable());
  tabGroupsClose.mockReset().mockResolvedValue(undefined);
  registerCommand.mockClear();
  executeCommand.mockClear();
  createChatParticipant.mockClear();

  withProgress.mockReset().mockImplementation(async (_options: unknown, task: (...args: any[]) => unknown) => {
    const fakeProgress = { report() {} };
    const fakeCancellationToken = { onCancellationRequested: () => new Disposable() };
    return task(fakeProgress, fakeCancellationToken);
  });
}

__resetVscodeMock();
