import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import {
  __getRegisteredCommand,
  __resetVscodeMock,
  __setActiveTextEditor,
  __setTabGroups,
  applyEdit,
  executeCommand,
  registerTextDocumentContentProvider,
  showErrorMessage,
  showInformationMessage,
  showInputBox,
  showWarningMessage,
  tabGroupsClose,
} from './test/vscode-mock';

vi.mock('./hupiClient', () => ({
  createClient: vi.fn(),
  streamChat: vi.fn(),
}));
vi.mock('./config', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./config')>();
  return { ...actual, loadConfig: vi.fn() };
});
vi.mock('./oidcAuth', () => ({
  promptSignInRequired: vi.fn(),
}));

import { createClient, streamChat } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { registerInlineEdit } from './inlineEdit';

const fakeCfg = { baseUrl: 'http://localhost:8787', model: '', teamId: '', apiKey: 'hupi_sk_test' };

function fakeEditor(text: string, opts: { isEmpty?: boolean; languageId?: string } = {}) {
  const { isEmpty = false, languageId = 'typescript' } = opts;
  return {
    selection: { isEmpty },
    document: {
      getText: () => text,
      languageId,
      uri: { path: '/fake/file.ts' },
    },
  };
}

function setUpCommand() {
  const context = {} as unknown as vscode.ExtensionContext;
  registerInlineEdit(context);
  return __getRegisteredCommand('hupi.inlineEdit')!;
}

/** Reads back what registerTextDocumentContentProvider's registered
 *  provider actually has stored for a given URI — the only way to see
 *  what registerInlineEdit's private virtualDocs map holds. */
function virtualDocContent(uri: vscode.Uri): string {
  const provider = registerTextDocumentContentProvider.mock.calls.at(-1)?.[1] as {
    provideTextDocumentContent(u: vscode.Uri): string;
  };
  return provider.provideTextDocumentContent(uri);
}

beforeEach(() => {
  __resetVscodeMock();
  vi.mocked(loadConfig).mockReset().mockResolvedValue(fakeCfg);
  vi.mocked(createClient).mockReset().mockReturnValue({} as never);
  vi.mocked(streamChat).mockReset();
  vi.mocked(promptSignInRequired).mockReset();
});

describe('registerInlineEdit', () => {
  it('registers hupi.inlineEdit and a text document content provider', () => {
    const context = {} as unknown as vscode.ExtensionContext;
    const disposables = registerInlineEdit(context);
    expect(disposables).toHaveLength(2);
    expect(__getRegisteredCommand('hupi.inlineEdit')).toBeDefined();
    expect(registerTextDocumentContentProvider).toHaveBeenCalledWith('hupi-inline-diff', expect.anything());
  });

  it('does nothing when there is no active editor', async () => {
    const command = setUpCommand();
    await command();
    expect(showInputBox).not.toHaveBeenCalled();
    expect(loadConfig).not.toHaveBeenCalled();
  });

  it('warns and stops when the selection is empty', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;', { isEmpty: true }));
    const command = setUpCommand();

    await command();

    expect(showWarningMessage).toHaveBeenCalledWith(expect.stringContaining('needs a selection'));
    expect(showInputBox).not.toHaveBeenCalled();
  });

  it('does nothing when the instruction box is cancelled or left blank', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue(undefined);
    const command = setUpCommand();

    await command();

    expect(loadConfig).not.toHaveBeenCalled();
  });

  it('prompts sign-in and does not call the provider when loadConfig throws OidcSignInRequiredError', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(loadConfig).mockRejectedValue(new OidcSignInRequiredError());
    const command = setUpCommand();

    await command();

    expect(promptSignInRequired).toHaveBeenCalledOnce();
    expect(showErrorMessage).not.toHaveBeenCalled();
    expect(streamChat).not.toHaveBeenCalled();
  });

  it('shows a generic config error without prompting sign-in', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(loadConfig).mockRejectedValue(new Error('providers.yaml missing'));
    const command = setUpCommand();

    await command();

    expect(showErrorMessage).toHaveBeenCalledWith('HUPI config error: providers.yaml missing');
    expect(promptSignInRequired).not.toHaveBeenCalled();
  });

  it('shows a request-failed error when the provider call fails', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockRejectedValue(new Error('upstream 502'));
    const command = setUpCommand();

    await command();

    expect(showErrorMessage).toHaveBeenCalledWith(
      'HUPI request failed: upstream 502. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).',
    );
    expect(executeCommand).not.toHaveBeenCalledWith('vscode.diff', expect.anything(), expect.anything(), expect.anything());
  });

  it('reports "no change proposed" and shows no diff when the rewrite matches the original', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('leave it alone');
    vi.mocked(streamChat).mockResolvedValue('const x = 1;');
    const command = setUpCommand();

    await command();

    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: no change proposed.');
    expect(executeCommand).not.toHaveBeenCalledWith('vscode.diff', expect.anything(), expect.anything(), expect.anything());
  });

  it('strips markdown code fences from the rewritten code before diffing', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockResolvedValue('```typescript\nlet x = 1;\n```');
    // registerInlineEdit deletes the virtual docs as soon as the
    // accept/reject prompt resolves — so the only place to observe them
    // is from inside that same prompt's mock, before it resolves.
    let beforeContent = '';
    let afterContent = '';
    showInformationMessage.mockImplementation(async () => {
      const [, beforeUri, afterUri] = vi.mocked(executeCommand).mock.calls.find((c) => c[0] === 'vscode.diff')!;
      beforeContent = virtualDocContent(beforeUri as vscode.Uri);
      afterContent = virtualDocContent(afterUri as vscode.Uri);
      return 'Reject';
    });
    const command = setUpCommand();

    await command();

    expect(beforeContent).toBe('const x = 1;');
    expect(afterContent).toBe('let x = 1;');
  });

  it('applies the edit when the user accepts', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockResolvedValue('let x = 1;');
    showInformationMessage.mockResolvedValue('Accept');
    const command = setUpCommand();

    await command();

    expect(applyEdit).toHaveBeenCalledOnce();
    const edit = vi.mocked(applyEdit).mock.calls[0][0] as { edits: { newText: string }[] };
    expect(edit.edits).toEqual([expect.objectContaining({ newText: 'let x = 1;' })]);
  });

  it('does not apply any edit when the user rejects', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockResolvedValue('let x = 1;');
    showInformationMessage.mockResolvedValue('Reject');
    const command = setUpCommand();

    await command();

    expect(applyEdit).not.toHaveBeenCalled();
  });

  it('does not apply any edit when the accept/reject prompt is dismissed', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockResolvedValue('let x = 1;');
    showInformationMessage.mockResolvedValue(undefined);
    const command = setUpCommand();

    await command();

    expect(applyEdit).not.toHaveBeenCalled();
  });

  it('closes a matching diff tab after the choice is made', async () => {
    __setActiveTextEditor(fakeEditor('const x = 1;'));
    showInputBox.mockResolvedValue('make it a let');
    vi.mocked(streamChat).mockResolvedValue('let x = 1;');
    showInformationMessage.mockResolvedValue('Reject');

    let matchingTab: unknown;
    const { TabInputTextDiff } = vscode;
    __setTabGroups([
      {
        tabs: [
          {
            get input() {
              // Built lazily so it can reuse the exact URIs the command
              // generates — captured via executeCommand below.
              return matchingTab;
            },
          },
        ],
      },
    ]);

    const command = setUpCommand();
    // Pre-seed a callback that builds the matching tab input once we know
    // the real before/after URIs the command picked.
    vi.mocked(executeCommand).mockImplementation(async (id: string, ...args: unknown[]) => {
      if (id === 'vscode.diff') {
        matchingTab = new TabInputTextDiff(args[0] as vscode.Uri, args[1] as vscode.Uri);
      }
      return undefined;
    });

    await command();

    expect(tabGroupsClose).toHaveBeenCalledOnce();
  });
});
