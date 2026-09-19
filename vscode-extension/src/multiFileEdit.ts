import * as vscode from 'vscode';
import { createClient, chat, type ChatMessage } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { createVirtualDiffProvider } from './diffContentProvider';

const DIFF_SCHEME = 'hupi-multifile-diff';

const SYSTEM_PROMPT =
  'You are a multi-file code editing assistant embedded in an editor. You ' +
  'will be given an instruction and the full contents of several files. For ' +
  'each file that genuinely needs a change, respond with a block in exactly ' +
  'this format:\n' +
  '---FILE: <path>---\n' +
  '<the entire new content of that file>\n' +
  '---END---\n' +
  'Use the exact path given for each file. Omit any file that needs no ' +
  'change entirely — do not include it at all. Output nothing else: no ' +
  'explanation, no commentary, no markdown fences around the blocks ' +
  'themselves.';

interface SelectedFile {
  path: string;
  doc: vscode.TextDocument;
}

interface FileProposal {
  path: string;
  doc: vscode.TextDocument;
  original: string;
  proposed: string;
}

type FromWebview =
  | { type: 'toggle'; path: string; checked: boolean }
  | { type: 'viewDiff'; path: string }
  | { type: 'apply'; paths: string[] }
  | { type: 'discard' };

type ToWebview = { type: 'render'; files: { path: string; checked: boolean }[] };

/** Parses the model's `---FILE: <path>---\n...\n---END---` blocks. Anything
 *  outside a well-formed block (stray commentary, a path the model made up)
 *  is silently dropped rather than surfaced as a parse error — callers
 *  additionally filter to only the paths that were actually offered, so a
 *  malformed or hallucinated block just means that file's changes are lost,
 *  not a crash. */
export function parseFileBlocks(text: string): { path: string; content: string }[] {
  const blocks: { path: string; content: string }[] = [];
  const re = /---FILE: (.+?)---\n([\s\S]*?)\n---END---/g;
  let match: RegExpExecArray | null;
  while ((match = re.exec(text)) !== null) {
    blocks.push({ path: match[1].trim(), content: match[2] });
  }
  return blocks;
}

export function registerMultiFileEdit(context: vscode.ExtensionContext): vscode.Disposable[] {
  const diffProvider = createVirtualDiffProvider(DIFF_SCHEME);

  const command = vscode.commands.registerCommand('hupi.multiFileEdit', async () => {
    const openDocs = (vscode.workspace.textDocuments as vscode.TextDocument[]).filter(
      (d) => d.uri.scheme === 'file',
    );
    if (openDocs.length === 0) {
      vscode.window.showWarningMessage(
        'HUPI: Multi-File Edit needs at least one open file — open the files you want to include first.',
      );
      return;
    }

    const picks = (await vscode.window.showQuickPick(
      openDocs.map((doc) => ({
        label: vscode.workspace.asRelativePath(doc.uri),
        picked: true,
        doc,
      })),
      {
        canPickMany: true,
        ignoreFocusOut: true,
        placeHolder: 'HUPI: select the open files to include in this edit',
      },
    )) as { label: string; doc: vscode.TextDocument }[] | undefined;
    if (!picks || picks.length === 0) {
      return;
    }
    const selected: SelectedFile[] = picks.map((p) => ({ path: p.label, doc: p.doc }));

    const instruction = await vscode.window.showInputBox({
      prompt: 'HUPI: describe the change to make across the selected files',
      ignoreFocusOut: true,
    });
    if (!instruction || instruction.trim() === '') {
      return;
    }

    let cfg;
    try {
      cfg = await loadConfig(context);
    } catch (err) {
      if (err instanceof OidcSignInRequiredError) {
        promptSignInRequired();
        return;
      }
      vscode.window.showErrorMessage(`HUPI config error: ${(err as Error).message}`);
      return;
    }
    const client = createClient(cfg);

    const fileBlocks = selected
      .map((s) => `---FILE: ${s.path}---\n${s.doc.getText()}\n---END---`)
      .join('\n\n');
    const messages: ChatMessage[] = [
      { role: 'system', content: SYSTEM_PROMPT },
      { role: 'user', content: `Instruction: ${instruction}\n\nFiles:\n\n${fileBlocks}` },
    ];

    let responseText = '';
    try {
      await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Notification, title: 'HUPI: generating multi-file edit...' },
        async () => {
          responseText = await chat(client, { model: cfg.model, messages });
        },
      );
    } catch (err) {
      vscode.window.showErrorMessage(
        `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).`,
      );
      return;
    }

    const selectedByPath = new Map(selected.map((s) => [s.path, s.doc]));
    const proposals: FileProposal[] = [];
    for (const { path, content } of parseFileBlocks(responseText)) {
      const doc = selectedByPath.get(path);
      if (!doc) {
        // Out of scope — the model touched a file that wasn't offered.
        continue;
      }
      const original = doc.getText();
      if (content.trim() === original.trim()) {
        continue;
      }
      proposals.push({ path, doc, original, proposed: content });
    }

    if (proposals.length === 0) {
      vscode.window.showInformationMessage('HUPI: no changes proposed.');
      return;
    }

    showReviewPanel(context, proposals, diffProvider);
  });

  return [diffProvider.registration, command];
}

function uriFor(scheme: string, stamp: number, side: 'before' | 'after', path: string): vscode.Uri {
  return vscode.Uri.parse(`${scheme}:/${side}-${stamp}-${encodeURIComponent(path)}`);
}

function showReviewPanel(
  context: vscode.ExtensionContext,
  proposals: FileProposal[],
  diffProvider: ReturnType<typeof createVirtualDiffProvider>,
): void {
  const panel = vscode.window.createWebviewPanel(
    'hupi.multiFileReview',
    `HUPI: Review Changes (${proposals.length} file${proposals.length === 1 ? '' : 's'})`,
    vscode.ViewColumn.Beside,
    { enableScripts: true, localResourceRoots: [vscode.Uri.joinPath(context.extensionUri, 'dist')] },
  );

  const checked = new Map(proposals.map((p) => [p.path, true]));
  const stamp = Date.now();
  const beforeUri = (p: FileProposal) => uriFor(DIFF_SCHEME, stamp, 'before', p.path);
  const afterUri = (p: FileProposal) => uriFor(DIFF_SCHEME, stamp, 'after', p.path);

  for (const p of proposals) {
    diffProvider.set(beforeUri(p), p.original);
    diffProvider.set(afterUri(p), p.proposed);
  }

  function post(message: ToWebview): void {
    void panel.webview.postMessage(message);
  }

  function renderState(): void {
    post({
      type: 'render',
      files: proposals.map((p) => ({ path: p.path, checked: checked.get(p.path) ?? true })),
    });
  }

  function cleanup(): void {
    for (const p of proposals) {
      diffProvider.delete(beforeUri(p));
      diffProvider.delete(afterUri(p));
    }
    panel.dispose();
  }

  panel.webview.html = renderHtml(panel.webview, context.extensionUri);
  panel.webview.onDidReceiveMessage(async (message: FromWebview) => {
    switch (message.type) {
      case 'toggle':
        checked.set(message.path, message.checked);
        return;
      case 'viewDiff': {
        const p = proposals.find((x) => x.path === message.path);
        if (!p) {
          return;
        }
        await vscode.commands.executeCommand(
          'vscode.diff',
          beforeUri(p),
          afterUri(p),
          `HUPI: ${p.path} (left: current, right: proposed)`,
        );
        return;
      }
      case 'apply': {
        const toApply = proposals.filter((p) => message.paths.includes(p.path));
        const edit = new vscode.WorkspaceEdit();
        for (const p of toApply) {
          const fullRange = new vscode.Range(p.doc.positionAt(0), p.doc.positionAt(p.original.length));
          edit.replace(p.doc.uri, fullRange, p.proposed);
        }
        await vscode.workspace.applyEdit(edit);
        vscode.window.showInformationMessage(
          `HUPI: applied changes to ${toApply.length} file${toApply.length === 1 ? '' : 's'}.`,
        );
        cleanup();
        return;
      }
      case 'discard':
        cleanup();
        return;
    }
  });
  renderState();
}

function renderHtml(webview: vscode.Webview, extensionUri: vscode.Uri): string {
  const scriptUri = webview.asWebviewUri(
    vscode.Uri.joinPath(extensionUri, 'dist', 'webview', 'multiFileReview.js'),
  );
  const nonce = String(Math.random()).slice(2);
  return /* html */ `<!doctype html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${webview.cspSource}; script-src 'nonce-${nonce}';" />
  <style>
    * { box-sizing: border-box; }
    body {
      font-family: var(--vscode-font-family);
      font-size: var(--vscode-font-size, 13px);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      margin: 0; padding: 12px;
      display: flex; flex-direction: column; gap: 10px; height: 100vh;
    }
    #hint { color: var(--vscode-descriptionForeground); font-size: 12.5px; }
    #files { flex: 1; overflow-y: auto; display: flex; flex-direction: column; gap: 6px; }
    .file-row {
      display: flex; align-items: center; gap: 8px;
      border: 1px solid color-mix(in srgb, var(--vscode-foreground) 12%, transparent);
      border-radius: 6px; padding: 8px 10px;
    }
    .file-row .path { flex: 1; font-family: var(--vscode-editor-font-family, monospace); font-size: 12.5px; }
    .file-row button {
      background: transparent; color: var(--vscode-textLink-foreground);
      border: none; cursor: pointer; font-size: 12px; padding: 2px 6px;
    }
    #footer { display: flex; justify-content: flex-end; gap: 8px; flex-shrink: 0; }
    #footer button {
      border: none; border-radius: 4px; padding: 6px 14px; cursor: pointer; font-size: inherit;
    }
    #apply { background: var(--vscode-button-background); color: var(--vscode-button-foreground); }
    #apply:hover { background: var(--vscode-button-hoverBackground); }
    #discard { background: transparent; color: var(--vscode-foreground); border: 1px solid color-mix(in srgb, var(--vscode-foreground) 25%, transparent) !important; }
  </style>
</head>
<body>
  <div id="hint">Review the proposed changes below, uncheck any file to exclude it, then apply.</div>
  <div id="files"></div>
  <div id="footer">
    <button id="discard">Discard All</button>
    <button id="apply">Apply Selected</button>
  </div>
  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
}
