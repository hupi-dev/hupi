import * as vscode from 'vscode';
import { createClient, streamChat, type ChatMessage } from './hupiClient';
import { loadConfig } from './config';

// Message shapes crossing the extension-host <-> webview boundary
// (postMessage/onDidReceiveMessage). Kept as a tiny discriminated union
// rather than a shared module import, since the webview side (src/webview/chat.ts)
// is a separate esbuild bundle that must never import `vscode` — duplicating
// these few lines is cheaper than wiring a shared non-vscode types module.
type FromWebview = { type: 'send'; text: string } | { type: 'clear' };
type ToWebview =
  | { type: 'userEcho'; text: string }
  | { type: 'delta'; text: string }
  | { type: 'done' }
  | { type: 'error'; message: string };

function currentFileContext(): string {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    return '';
  }
  const doc = editor.document;
  const selection = editor.selection;
  const hasSelection = !selection.isEmpty;
  const text = hasSelection ? doc.getText(selection) : doc.getText();
  const label = hasSelection ? 'selected code' : 'visible file';
  // A generous but bounded slice — the whole point of HUPI's own retrieval
  // is long-term memory; this context block is only meant to ground the
  // model in *what you're looking at right now*, not replace that.
  const capped = text.length > 8000 ? text.slice(0, 8000) + '\n... (truncated)' : text;
  return `Context — ${label} from ${vscode.workspace.asRelativePath(doc.uri)}:\n\`\`\`${doc.languageId}\n${capped}\n\`\`\``;
}

export class HupiChatViewProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = 'hupi.chatView';

  private history: ChatMessage[] = [];
  private webviewView?: vscode.WebviewView;

  constructor(
    private readonly context: vscode.ExtensionContext,
    private readonly extensionUri: vscode.Uri,
  ) {}

  resolveWebviewView(webviewView: vscode.WebviewView): void {
    this.webviewView = webviewView;
    webviewView.webview.options = {
      enableScripts: true,
      localResourceRoots: [vscode.Uri.joinPath(this.extensionUri, 'dist')],
    };
    webviewView.webview.html = this.renderHtml(webviewView.webview);

    webviewView.webview.onDidReceiveMessage(async (message: FromWebview) => {
      if (message.type === 'clear') {
        this.history = [];
        return;
      }
      if (message.type === 'send') {
        await this.handleSend(message.text, webviewView.webview);
      }
    });
  }

  private post(webview: vscode.Webview, message: ToWebview): void {
    void webview.postMessage(message);
  }

  private async handleSend(text: string, webview: vscode.Webview): Promise<void> {
    const trimmed = text.trim();
    if (trimmed === '') {
      return;
    }

    const context = currentFileContext();
    const userContent = context ? `${context}\n\n${trimmed}` : trimmed;
    this.history.push({ role: 'user', content: userContent });

    let cfg;
    try {
      cfg = await loadConfig(this.context);
    } catch (err) {
      this.post(webview, { type: 'error', message: `Config error: ${(err as Error).message}` });
      return;
    }

    const client = createClient(cfg);
    let assistantText = '';
    try {
      assistantText = await streamChat(client, {
        model: cfg.model,
        messages: this.history,
        onDelta: (delta) => this.post(webview, { type: 'delta', text: delta }),
      });
      this.post(webview, { type: 'done' });
    } catch (err) {
      this.post(webview, {
        type: 'error',
        message: `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key).`,
      });
      // Roll back the just-added user turn so a failed request doesn't
      // silently poison the conversation history sent on the next try.
      this.history.pop();
      return;
    }
    this.history.push({ role: 'assistant', content: assistantText });
  }

  private renderHtml(webview: vscode.Webview): string {
    const scriptUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, 'dist', 'webview', 'chat.js'),
    );
    const nonce = String(Math.random()).slice(2);
    return /* html */ `<!doctype html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${webview.cspSource}; script-src 'nonce-${nonce}';" />
  <style>
    body { font-family: var(--vscode-font-family); color: var(--vscode-foreground); background: var(--vscode-editor-background); margin: 0; padding: 0; display: flex; flex-direction: column; height: 100vh; }
    #log { flex: 1; overflow-y: auto; padding: 8px; }
    .msg { margin-bottom: 12px; white-space: pre-wrap; word-break: break-word; }
    .msg.user { color: var(--vscode-textLink-foreground); }
    .msg.error { color: var(--vscode-errorForeground); }
    .msg pre { background: var(--vscode-textCodeBlock-background); padding: 6px; overflow-x: auto; border-radius: 4px; }
    #inputRow { display: flex; border-top: 1px solid var(--vscode-panel-border); padding: 6px; gap: 6px; }
    #input { flex: 1; resize: none; background: var(--vscode-input-background); color: var(--vscode-input-foreground); border: 1px solid var(--vscode-input-border); font-family: inherit; padding: 4px; }
    button { background: var(--vscode-button-background); color: var(--vscode-button-foreground); border: none; padding: 4px 10px; cursor: pointer; }
    button:hover { background: var(--vscode-button-hoverBackground); }
  </style>
</head>
<body>
  <div id="log"></div>
  <div id="inputRow">
    <textarea id="input" rows="2" placeholder="Ask HUPI... (Enter to send, Shift+Enter for newline)"></textarea>
    <button id="send">Send</button>
  </div>
  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
  }
}
