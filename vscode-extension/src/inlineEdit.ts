import * as vscode from 'vscode';
import { createClient, streamChat, type ChatMessage } from './hupiClient';
import { loadConfig } from './config';

const DIFF_SCHEME = 'hupi-inline-diff';

/** Backing store for the virtual before/after documents the diff view
 *  reads from — keyed by the virtual URI's path, so concurrent inline-edit
 *  invocations (unlikely, but cheap to get right) don't clobber each other. */
const virtualDocs = new Map<string, string>();

class InlineDiffContentProvider implements vscode.TextDocumentContentProvider {
  provideTextDocumentContent(uri: vscode.Uri): string {
    return virtualDocs.get(uri.path) ?? '';
  }
}

function stripCodeFences(text: string): string {
  const trimmed = text.trim();
  const fenced = trimmed.match(/^```[^\n]*\n([\s\S]*?)\n?```$/);
  return fenced ? fenced[1] : trimmed;
}

export function registerInlineEdit(context: vscode.ExtensionContext): vscode.Disposable[] {
  const provider = new InlineDiffContentProvider();
  const providerRegistration = vscode.workspace.registerTextDocumentContentProvider(
    DIFF_SCHEME,
    provider,
  );

  const command = vscode.commands.registerCommand('hupi.inlineEdit', async () => {
    const editor = vscode.window.activeTextEditor;
    if (!editor) {
      return;
    }
    const selection = editor.selection;
    if (selection.isEmpty) {
      vscode.window.showWarningMessage('HUPI: Inline Edit needs a selection — select some code first.');
      return;
    }

    const instruction = await vscode.window.showInputBox({
      prompt: 'HUPI: describe the change to make to the selected code',
      ignoreFocusOut: true,
    });
    if (!instruction || instruction.trim() === '') {
      return;
    }

    const originalText = editor.document.getText(selection);
    const languageId = editor.document.languageId;

    let cfg;
    try {
      cfg = await loadConfig(context);
    } catch (err) {
      vscode.window.showErrorMessage(`HUPI config error: ${(err as Error).message}`);
      return;
    }
    const client = createClient(cfg);

    const messages: ChatMessage[] = [
      {
        role: 'system',
        content:
          'You are a code editing assistant embedded in an editor. Rewrite the given code selection according to the instruction. Respond with ONLY the rewritten code — no explanation, no markdown code fences, no commentary.',
      },
      {
        role: 'user',
        content: `File language: ${languageId}\n\nInstruction: ${instruction}\n\nCode to rewrite:\n${originalText}`,
      },
    ];

    let rewritten = '';
    try {
      await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Notification, title: 'HUPI: generating edit...' },
        async () => {
          rewritten = await streamChat(client, { model: cfg.model, messages, onDelta: () => {} });
        },
      );
    } catch (err) {
      vscode.window.showErrorMessage(
        `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key).`,
      );
      return;
    }
    rewritten = stripCodeFences(rewritten);

    if (rewritten.trim() === originalText.trim()) {
      vscode.window.showInformationMessage('HUPI: no change proposed.');
      return;
    }

    // Show a before/after diff scoped to just the selection (not the whole
    // file — clearer for a selection-scoped edit, and avoids needing a
    // full second copy of the document). vscode.diff only compares whole
    // documents, so both sides are small virtual documents backed by
    // InlineDiffContentProvider, not the real file.
    const stamp = Date.now();
    const beforeUri = vscode.Uri.parse(`${DIFF_SCHEME}:/before-${stamp}${extFor(languageId)}`);
    const afterUri = vscode.Uri.parse(`${DIFF_SCHEME}:/after-${stamp}${extFor(languageId)}`);
    virtualDocs.set(beforeUri.path, originalText);
    virtualDocs.set(afterUri.path, rewritten);

    await vscode.commands.executeCommand(
      'vscode.diff',
      beforeUri,
      afterUri,
      'HUPI: proposed edit (left: current, right: proposed)',
    );

    const choice = await vscode.window.showInformationMessage(
      'Apply this edit to your file?',
      { modal: false },
      'Accept',
      'Reject',
    );

    virtualDocs.delete(beforeUri.path);
    virtualDocs.delete(afterUri.path);
    await closeDiffTab(beforeUri, afterUri);

    if (choice === 'Accept') {
      const edit = new vscode.WorkspaceEdit();
      edit.replace(editor.document.uri, selection, rewritten);
      await vscode.workspace.applyEdit(edit);
    }
  });

  return [providerRegistration, command];
}

function extFor(languageId: string): string {
  // Purely cosmetic — gives the virtual diff documents a plausible
  // extension so VS Code picks reasonable syntax highlighting for them.
  const known: Record<string, string> = {
    typescript: '.ts',
    typescriptreact: '.tsx',
    javascript: '.js',
    javascriptreact: '.jsx',
    python: '.py',
    go: '.go',
    rust: '.rs',
    java: '.java',
    json: '.json',
  };
  return known[languageId] ?? '.txt';
}

async function closeDiffTab(beforeUri: vscode.Uri, afterUri: vscode.Uri): Promise<void> {
  for (const group of vscode.window.tabGroups.all) {
    for (const tab of group.tabs) {
      const input = tab.input;
      if (
        input instanceof vscode.TabInputTextDiff &&
        (input.original.toString() === beforeUri.toString() ||
          input.modified.toString() === afterUri.toString())
      ) {
        await vscode.window.tabGroups.close(tab);
      }
    }
  }
}
