import * as vscode from 'vscode';
import { createClient, streamChat, type ChatMessage } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';

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

export function currentFileContext(): string {
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
  // Only one response should ever be in flight at a time — without this,
  // sending a second message (or hitting "New Chat") before the first
  // finishes streaming leaves two concurrent streamChat loops both posting
  // 'delta'/'done' messages at the same webview, both racing to mutate its
  // single currentAssistantContent/currentAssistantRaw state. The webview
  // now also disables input while streaming, so this is belt-and-suspenders
  // against any message that slips through anyway (a stale click, a
  // 'clear' arriving mid-stream).
  private inFlight?: AbortController;

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
        this.inFlight?.abort();
        this.history = [];
        return;
      }
      if (message.type === 'send') {
        this.inFlight?.abort();
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
      if (err instanceof OidcSignInRequiredError) {
        this.post(webview, { type: 'error', message: err.message });
        promptSignInRequired();
        return;
      }
      this.post(webview, { type: 'error', message: `Config error: ${(err as Error).message}` });
      return;
    }

    const controller = new AbortController();
    this.inFlight = controller;
    const client = createClient(cfg);
    let assistantText = '';
    try {
      assistantText = await streamChat(client, {
        model: cfg.model,
        messages: this.history,
        signal: controller.signal,
        onDelta: (delta) => this.post(webview, { type: 'delta', text: delta }),
      });
      this.post(webview, { type: 'done' });
    } catch (err) {
      // A deliberate abort (superseded by a newer send, or "New Chat") is
      // not a failure worth surfacing — the webview already moved on.
      if (controller.signal.aborted) {
        return;
      }
      this.post(webview, {
        type: 'error',
        message: `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).`,
      });
      // Roll back the just-added user turn so a failed request doesn't
      // silently poison the conversation history sent on the next try.
      this.history.pop();
      return;
    } finally {
      if (this.inFlight === controller) {
        this.inFlight = undefined;
      }
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
    :root {
      --hupi-border: color-mix(in srgb, var(--vscode-foreground) 12%, transparent);
      --hupi-card-bg: color-mix(in srgb, var(--vscode-editorWidget-background) 60%, var(--vscode-editor-background));
    }
    * { box-sizing: border-box; }
    body {
      font-family: var(--vscode-font-family);
      font-size: var(--vscode-font-size, 13px);
      color: var(--vscode-foreground);
      background: var(--vscode-editor-background);
      margin: 0; padding: 0;
      display: flex; flex-direction: column; height: 100vh;
    }
    #header {
      display: flex; align-items: center; justify-content: space-between;
      padding: 8px 12px;
      border-bottom: 1px solid var(--hupi-border);
      flex-shrink: 0;
    }
    #header .title {
      font-weight: 600; font-size: 11px; letter-spacing: 0.08em;
      text-transform: uppercase; color: var(--vscode-descriptionForeground);
    }
    #newChat {
      display: flex; align-items: center; gap: 5px;
      background: transparent; color: var(--vscode-foreground);
      border: 1px solid var(--hupi-border); border-radius: 4px;
      padding: 3px 9px; font-size: 12px; cursor: pointer;
    }
    #newChat:hover { background: var(--vscode-toolbar-hoverBackground, var(--hupi-card-bg)); }
    #log { flex: 1; overflow-y: auto; padding: 10px 12px; display: flex; flex-direction: column; gap: 10px; }
    #empty {
      color: var(--vscode-descriptionForeground); font-size: 12.5px;
      line-height: 1.5; padding: 12px 2px;
    }
    .msg {
      border: 1px solid var(--hupi-border);
      border-radius: 6px;
      padding: 8px 10px;
      background: var(--hupi-card-bg);
      white-space: pre-wrap; word-break: break-word;
      line-height: 1.45;
    }
    .msg .role {
      display: block;
      font-size: 10.5px; font-weight: 600; letter-spacing: 0.06em;
      text-transform: uppercase; margin-bottom: 4px;
      color: var(--vscode-descriptionForeground);
    }
    .msg.user { border-left: 2px solid var(--vscode-textLink-foreground); }
    .msg.user .role { color: var(--vscode-textLink-foreground); }
    .msg.assistant { border-left: 2px solid var(--vscode-charts-purple, var(--vscode-textLink-foreground)); }
    .msg.error { border-left: 2px solid var(--vscode-errorForeground); color: var(--vscode-errorForeground); }
    .msg.error .role { color: var(--vscode-errorForeground); }
    .msg p { margin: 0 0 6px; }
    .msg p:last-child { margin-bottom: 0; }
    .msg pre { background: var(--vscode-textCodeBlock-background); padding: 8px; overflow-x: auto; border-radius: 4px; }
    .msg code { font-family: var(--vscode-editor-font-family, monospace); }
    #inputRow { display: flex; border-top: 1px solid var(--hupi-border); padding: 8px; gap: 6px; flex-shrink: 0; }
    #input {
      flex: 1; resize: none;
      background: var(--vscode-input-background); color: var(--vscode-input-foreground);
      border: 1px solid var(--vscode-input-border); border-radius: 4px;
      font-family: inherit; font-size: inherit; padding: 6px 8px;
    }
    #input:focus { outline: 1px solid var(--vscode-focusBorder); outline-offset: -1px; }
    #send {
      background: var(--vscode-button-background); color: var(--vscode-button-foreground);
      border: none; border-radius: 4px; padding: 0 14px; cursor: pointer; font-size: inherit;
    }
    #send:hover { background: var(--vscode-button-hoverBackground); }
  </style>
</head>
<body>
  <div id="header">
    <span class="title">HUPI</span>
    <button id="newChat" title="Start a new conversation">
      <svg width="12" height="12" viewBox="0 0 16 16" fill="none"><path d="M8 3v10M3 8h10" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>
      New Chat
    </button>
  </div>
  <div id="log">
    <div id="empty">Ask about your code, or anything HUPI already remembers from past conversations.</div>
  </div>
  <div id="inputRow">
    <textarea id="input" rows="2" placeholder="Ask HUPI... (Enter to send, Shift+Enter for newline)"></textarea>
    <button id="send">Send</button>
  </div>
  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
  }
}
