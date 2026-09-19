import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import {
  __getRegisteredCommand,
  __resetVscodeMock,
  __setTextDocuments,
  applyEdit,
  createWebviewPanel,
  executeCommand,
  registerTextDocumentContentProvider,
  showErrorMessage,
  showInformationMessage,
  showInputBox,
  showQuickPick,
  showWarningMessage,
} from './test/vscode-mock';

vi.mock('./hupiClient', () => ({ createClient: vi.fn(), chat: vi.fn() }));
vi.mock('./config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./config')>();
  return { ...actual, loadConfig: vi.fn() };
});
vi.mock('./oidcAuth', () => ({ promptSignInRequired: vi.fn() }));

import { createClient, chat, type ChatOptions } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { parseFileBlocks, registerMultiFileEdit } from './multiFileEdit';

const fakeCfg = { baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'hupi_sk_test' };

function fakeDoc(path: string, text: string) {
  return {
    getText: () => text,
    positionAt: (n: number) => ({ offset: n }),
    uri: { scheme: 'file', path, toString: () => `file://${path}` },
  } as unknown as vscode.TextDocument;
}

type ToWebview = { type: 'render'; files: { path: string; checked: boolean }[] };

function fakeWebviewPanel() {
  const posted: ToWebview[] = [];
  let receiveHandler: ((message: unknown) => unknown) | undefined;
  const dispose = vi.fn();
  const webview = {
    html: '',
    cspSource: 'fake-csp',
    postMessage: async (msg: ToWebview) => {
      posted.push(msg);
      return true;
    },
    onDidReceiveMessage: (cb: (message: unknown) => unknown) => {
      receiveHandler = cb;
      return { dispose() {} };
    },
    asWebviewUri: (uri: unknown) => uri,
  };
  return {
    panel: { webview, dispose } as unknown as vscode.WebviewPanel,
    posted,
    dispose,
    send: (message: unknown) => receiveHandler!(message),
  };
}

function setUpCommand(docs: vscode.TextDocument[]) {
  __setTextDocuments(docs);
  const context = { extensionUri: vscode.Uri.file('/fake') } as unknown as vscode.ExtensionContext;
  registerMultiFileEdit(context);
  return __getRegisteredCommand('hupi.multiFileEdit')!;
}

beforeEach(() => {
  __resetVscodeMock();
  vi.mocked(loadConfig).mockReset().mockResolvedValue(fakeCfg);
  vi.mocked(createClient).mockReset().mockReturnValue({} as never);
  vi.mocked(chat).mockReset();
  vi.mocked(promptSignInRequired).mockReset();
});

describe('parseFileBlocks', () => {
  it('parses a single file block', () => {
    expect(parseFileBlocks('---FILE: a.ts---\nconst a = 1;\n---END---')).toEqual([
      { path: 'a.ts', content: 'const a = 1;' },
    ]);
  });

  it('parses multiple blocks and ignores surrounding text', () => {
    const text = [
      'Sure, here are the changes:',
      '---FILE: a.ts---',
      'const a = 1;',
      '---END---',
      '---FILE: b.ts---',
      'const b = 2;',
      '---END---',
    ].join('\n');
    expect(parseFileBlocks(text)).toEqual([
      { path: 'a.ts', content: 'const a = 1;' },
      { path: 'b.ts', content: 'const b = 2;' },
    ]);
  });

  it('returns an empty array when nothing matches', () => {
    expect(parseFileBlocks('no changes needed')).toEqual([]);
  });
});

describe('registerMultiFileEdit', () => {
  it('registers hupi.multiFileEdit and a text document content provider', () => {
    const command = setUpCommand([]);
    expect(command).toBeDefined();
    expect(registerTextDocumentContentProvider).toHaveBeenCalledWith(
      'hupi-multifile-diff',
      expect.anything(),
    );
  });

  it('warns and stops when no files are open', async () => {
    const command = setUpCommand([]);
    await command();
    expect(showWarningMessage).toHaveBeenCalledWith(expect.stringContaining('needs at least one open file'));
    expect(showQuickPick).not.toHaveBeenCalled();
  });

  it('does nothing when the file picker is cancelled', async () => {
    const command = setUpCommand([fakeDoc('a.ts', 'const a = 1;')]);
    showQuickPick.mockResolvedValue(undefined);

    await command();

    expect(showInputBox).not.toHaveBeenCalled();
  });

  it('does nothing when no files are picked', async () => {
    const command = setUpCommand([fakeDoc('a.ts', 'const a = 1;')]);
    showQuickPick.mockResolvedValue([]);

    await command();

    expect(showInputBox).not.toHaveBeenCalled();
  });

  it('does nothing when the instruction is cancelled or blank', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('   ');

    await command();

    expect(loadConfig).not.toHaveBeenCalled();
  });

  it('prompts sign-in when loadConfig throws OidcSignInRequiredError', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('add a comment');
    vi.mocked(loadConfig).mockRejectedValue(new OidcSignInRequiredError());

    await command();

    expect(promptSignInRequired).toHaveBeenCalledOnce();
    expect(chat).not.toHaveBeenCalled();
  });

  it('shows a generic config error without prompting sign-in', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('add a comment');
    vi.mocked(loadConfig).mockRejectedValue(new Error('providers.yaml missing'));

    await command();

    expect(showErrorMessage).toHaveBeenCalledWith('HUPI config error: providers.yaml missing');
    expect(promptSignInRequired).not.toHaveBeenCalled();
  });

  it('shows a request-failed error when the model call fails', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('add a comment');
    vi.mocked(chat).mockRejectedValue(new Error('upstream 502'));

    await command();

    expect(showErrorMessage).toHaveBeenCalledWith(
      'HUPI request failed: upstream 502. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).',
    );
    expect(createWebviewPanel).not.toHaveBeenCalled();
  });

  it('reports no changes proposed when the response has no usable blocks', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('leave everything alone');
    vi.mocked(chat).mockResolvedValue('No changes are needed.');

    await command();

    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: no changes proposed.');
    expect(createWebviewPanel).not.toHaveBeenCalled();
  });

  it('drops a block whose content is unchanged and one for a file outside the selection', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('add logging to b.ts too');
    vi.mocked(chat).mockResolvedValue(
      '---FILE: a.ts---\nconst a = 1;\n---END---\n---FILE: b.ts---\nconst b = 2;\n---END---',
    );

    await command();

    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: no changes proposed.');
    expect(createWebviewPanel).not.toHaveBeenCalled();
  });

  it('sends the instruction and full contents of every selected file', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const docB = fakeDoc('b.ts', 'const b = 2;');
    const command = setUpCommand([docA, docB]);
    showQuickPick.mockResolvedValue([
      { label: 'a.ts', doc: docA },
      { label: 'b.ts', doc: docB },
    ]);
    showInputBox.mockResolvedValue('rename a and b to x and y');
    vi.mocked(chat).mockResolvedValue('---FILE: a.ts---\nconst x = 1;\n---END---');
    createWebviewPanel.mockReturnValue(fakeWebviewPanel().panel);

    await command();

    const opts = vi.mocked(chat).mock.calls[0][1] as ChatOptions;
    expect(opts.messages[1].content).toContain('rename a and b to x and y');
    expect(opts.messages[1].content).toContain('---FILE: a.ts---\nconst a = 1;\n---END---');
    expect(opts.messages[1].content).toContain('---FILE: b.ts---\nconst b = 2;\n---END---');
  });

  it('opens a review panel listing the proposed files', async () => {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('rename a to x');
    vi.mocked(chat).mockResolvedValue('---FILE: a.ts---\nconst x = 1;\n---END---');
    const { panel, posted } = fakeWebviewPanel();
    createWebviewPanel.mockReturnValue(panel);

    await command();

    expect(createWebviewPanel).toHaveBeenCalledWith(
      'hupi.multiFileReview',
      expect.stringContaining('1 file'),
      vscode.ViewColumn.Beside,
      expect.anything(),
    );
    expect(posted).toEqual([{ type: 'render', files: [{ path: 'a.ts', checked: true }] }]);
  });

  async function openPanelWithOneProposal() {
    const docA = fakeDoc('a.ts', 'const a = 1;');
    const command = setUpCommand([docA]);
    showQuickPick.mockResolvedValue([{ label: 'a.ts', doc: docA }]);
    showInputBox.mockResolvedValue('rename a to x');
    vi.mocked(chat).mockResolvedValue('---FILE: a.ts---\nconst x = 1;\n---END---');
    const { panel, send, dispose } = fakeWebviewPanel();
    createWebviewPanel.mockReturnValue(panel);

    await command();
    return { send, dispose, docA };
  }

  it('opens a native diff for the requested file on viewDiff', async () => {
    const { send } = await openPanelWithOneProposal();

    await send({ type: 'viewDiff', path: 'a.ts' });

    expect(executeCommand).toHaveBeenCalledWith(
      'vscode.diff',
      expect.anything(),
      expect.anything(),
      expect.stringContaining('a.ts'),
    );
  });

  it('applies only the checked files and disposes the panel on apply', async () => {
    const { send, dispose } = await openPanelWithOneProposal();

    await send({ type: 'apply', paths: ['a.ts'] });

    expect(applyEdit).toHaveBeenCalledOnce();
    const edit = vi.mocked(applyEdit).mock.calls[0][0] as { edits: { newText: string }[] };
    expect(edit.edits).toEqual([expect.objectContaining({ newText: 'const x = 1;' })]);
    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: applied changes to 1 file.');
    expect(dispose).toHaveBeenCalledOnce();
  });

  it('applies nothing when apply is sent with no paths', async () => {
    const { send, dispose } = await openPanelWithOneProposal();

    await send({ type: 'apply', paths: [] });

    expect(applyEdit).toHaveBeenCalledOnce();
    const edit = vi.mocked(applyEdit).mock.calls[0][0] as { edits: unknown[] };
    expect(edit.edits).toEqual([]);
    expect(dispose).toHaveBeenCalledOnce();
  });

  it('discards without applying any edit', async () => {
    const { send, dispose } = await openPanelWithOneProposal();

    await send({ type: 'discard' });

    expect(applyEdit).not.toHaveBeenCalled();
    expect(dispose).toHaveBeenCalledOnce();
  });
});
