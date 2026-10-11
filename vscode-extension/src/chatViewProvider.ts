import * as vscode from 'vscode';
import {
  createClient,
  streamChat,
  streamChatTurn,
  type AgentMessage,
  type ChatAttachment,
  type ChatMessage,
  type Citation,
  type HupiConfig,
} from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { runAgentTurn } from './agent/agentLoop';
import { buildRegistry, selectVscodeTools, startMcpServers } from './agent/toolRegistry';
import { runVscodeTool } from './agent/vscodeTools';

/** Overall cap on one agent turn, on top of each request's own timeout. */
export const AGENT_TURN_TIMEOUT_MS = 10 * 60_000;

/** The plain-chat view of a history that may hold agent turns: tool
 *  results and tool-call-only assistant messages are dropped, so the
 *  string-only streamChat path never sees them. */
export function textOnly(history: AgentMessage[]): ChatMessage[] {
  const out: ChatMessage[] = [];
  for (const m of history) {
    if (m.role === 'tool' || ('tool_calls' in m && m.tool_calls.length > 0)) {
      continue;
    }
    out.push({ role: m.role, content: m.content ?? '' });
  }
  return out;
}

// Message shapes crossing the extension-host <-> webview boundary
// (postMessage/onDidReceiveMessage). Kept as a tiny discriminated union
// rather than a shared module import, since the webview side (src/webview/chat.ts)
// is a separate esbuild bundle that must never import `vscode` — duplicating
// these few lines is cheaper than wiring a shared non-vscode types module.
// Citation/ChatAttachment themselves (hupiClient.ts) have no `vscode`
// dependency, so they're imported directly rather than re-duplicated
// field-by-field here — this file (unlike src/webview/chat.ts) is
// extension-host code, so importing hupiClient.ts is fine.
//
// The agent messages (toolCall/toolResult/approvalRequest, approve/deny)
// carry a tool call's id so the webview can update that call's card;
// "cancel" is the Stop button, answered with "stopped".
type FromWebview =
  | { type: 'send'; text: string; attachments?: ChatAttachment[] }
  | { type: 'clear' }
  | { type: 'cancel' }
  | { type: 'approve'; id: string }
  | { type: 'deny'; id: string };
type ToWebview =
  | { type: 'fileContext'; relativePath: string }
  | { type: 'delta'; text: string }
  | { type: 'citations'; items: Citation[] }
  | { type: 'attachmentWarnings'; warnings: string[] }
  | { type: 'toolCall'; id: string; name: string; argsPreview: string }
  | { type: 'approvalRequest'; id: string; name: string; argsPreview: string }
  | { type: 'toolResult'; id: string; ok: boolean; summary: string }
  | { type: 'done' }
  | { type: 'stopped' }
  | { type: 'error'; message: string };

/** Shortens text for display in a tool card (the full value still goes to
 *  the model). */
export function preview(text: string, max: number): string {
  const flat = text.replace(/\s+/g, ' ').trim();
  return flat.length > max ? flat.slice(0, max) + '…' : flat;
}

export interface FileContext {
  /** The block prepended to the user's message, sent to the gateway. */
  text: string;
  relativePath: string;
  uri: vscode.Uri;
}

/** Builds the active-file/selection context block prepended to every chat
 *  turn, unless disabled via hupi.fileContext.enabled (review finding
 *  A13 — this used to be silent and unconditional, with no UI indication
 *  anything beyond the user's typed text was sent, and no way to turn it
 *  off for a sensitive file). Returns the relativePath/uri alongside the
 *  text so callers can surface a visible "context attached" indicator
 *  without re-deriving them. */
export function currentFileContext(): FileContext | undefined {
  const editor = vscode.window.activeTextEditor;
  if (!editor) {
    return undefined;
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
  const relativePath = vscode.workspace.asRelativePath(doc.uri);
  return {
    text: `Context — ${label} from ${relativePath}:\n\`\`\`${doc.languageId}\n${capped}\n\`\`\``,
    relativePath,
    uri: doc.uri,
  };
}

export class HupiChatViewProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = 'hupi.chatView';

  private history: AgentMessage[] = [];
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
  // The turn the user pressed Stop on, so its abort is answered with a
  // "stopped" message rather than silence (a supersede or New Chat needs
  // no reply: the webview already moved on).
  private stopRequested?: AbortController;
  // Approvals waiting on the webview's Allow/Deny, by tool-call id.
  private pendingApprovals = new Map<string, (approved: boolean) => void>();

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
        this.denyPendingApprovals();
        this.history = [];
        return;
      }
      if (message.type === 'cancel') {
        this.stopRequested = this.inFlight;
        this.inFlight?.abort();
        this.denyPendingApprovals();
        return;
      }
      if (message.type === 'approve' || message.type === 'deny') {
        const resolve = this.pendingApprovals.get(message.id);
        this.pendingApprovals.delete(message.id);
        resolve?.(message.type === 'approve');
        return;
      }
      if (message.type === 'send') {
        this.inFlight?.abort();
        await this.handleSend(message.text, message.attachments, webviewView.webview);
      }
    });
  }

  private post(webview: vscode.Webview, message: ToWebview): void {
    void webview.postMessage(message);
  }

  private denyPendingApprovals(): void {
    for (const resolve of this.pendingApprovals.values()) {
      resolve(false);
    }
    this.pendingApprovals.clear();
  }

  /** Asks the user in the sidebar (hupi.agent.approval: "all"): the call's
   *  card shows Allow/Deny, and the turn waits for the answer. Stop and
   *  New Chat count as Deny. */
  private askInSidebar(webview: vscode.Webview, id: string, name: string, args: string, signal: AbortSignal): Promise<boolean> {
    if (signal.aborted) {
      return Promise.resolve(false);
    }
    return new Promise((resolve) => {
      this.pendingApprovals.set(id, resolve);
      this.post(webview, { type: 'approvalRequest', id, name, argsPreview: preview(args || '{}', 300) });
    });
  }

  private async handleSend(text: string, attachments: ChatAttachment[] | undefined, webview: vscode.Webview): Promise<void> {
    const trimmed = text.trim();
    if (trimmed === '') {
      return;
    }

    const settings = vscode.workspace.getConfiguration('hupi');
    const fileContextEnabled = settings.get<boolean>('fileContext.enabled', true);
    const context = fileContextEnabled ? currentFileContext() : undefined;
    const userContent = context ? `${context.text}\n\n${trimmed}` : trimmed;
    // Any failure or cancellation rolls the history back to here, so a
    // failed turn (including every message an agent turn added) never
    // poisons the conversation sent on the next try.
    const historyBefore = this.history.length;
    const rollback = () => {
      this.history.length = historyBefore;
    };
    this.history.push({ role: 'user', content: userContent });
    if (context) {
      this.post(webview, { type: 'fileContext', relativePath: context.relativePath });
    }

    let cfg;
    try {
      cfg = await loadConfig(this.context);
    } catch (err) {
      rollback();
      if (err instanceof OidcSignInRequiredError) {
        this.post(webview, { type: 'error', message: err.message });
        promptSignInRequired();
        return;
      }
      this.post(webview, { type: 'error', message: `Config error: ${(err as Error).message}` });
      return;
    }

    const citationsEnabled = settings.get<boolean>('citations.enabled', true);
    const deepCitations = settings.get<boolean>('citations.deep', false);

    const controller = new AbortController();
    this.inFlight = controller;
    const explain = citationsEnabled ? (deepCitations ? 'deep' : 'on') : undefined;
    const turnAttachments = attachments && attachments.length > 0 ? attachments : undefined;
    try {
      if (settings.get<boolean>('agent.enabled', false)) {
        this.history.push(...(await this.runAgent(cfg, settings, webview, controller.signal, explain, turnAttachments)));
      } else {
        const assistantText = await streamChat(createClient(cfg), {
          model: cfg.model,
          messages: textOnly(this.history),
          signal: controller.signal,
          onDelta: (delta) => this.post(webview, { type: 'delta', text: delta }),
          explain,
          onCitations: (items) => this.post(webview, { type: 'citations', items }),
          attachments: turnAttachments,
          onAttachmentWarnings: (warnings) => this.post(webview, { type: 'attachmentWarnings', warnings }),
        });
        this.history.push({ role: 'assistant', content: assistantText });
      }
      this.post(webview, { type: 'done' });
    } catch (err) {
      rollback();
      // A deliberate abort (superseded by a newer send, or "New Chat") is
      // not a failure worth surfacing — the webview already moved on. Stop
      // is answered so the webview can close off the reply.
      if (controller.signal.aborted) {
        if (this.stopRequested === controller) {
          this.post(webview, { type: 'stopped' });
        }
        return;
      }
      this.post(webview, {
        type: 'error',
        message: `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).`,
      });
      return;
    } finally {
      if (this.inFlight === controller) {
        this.inFlight = undefined;
      }
      if (this.stopRequested === controller) {
        this.stopRequested = undefined;
      }
    }
  }

  /**
   * One agent turn (hupi.agent.enabled): offers the user's MCP tools to
   * the model and runs what it asks for until it answers. Returns the
   * messages to append to history. Each tool call shows as a card in the
   * reply (toolCall, then toolResult).
   */
  private async runAgent(
    cfg: HupiConfig,
    settings: vscode.WorkspaceConfiguration,
    webview: vscode.Webview,
    cancel: AbortSignal,
    explain: 'on' | 'deep' | undefined,
    attachments: ChatAttachment[] | undefined,
  ): Promise<AgentMessage[]> {
    const useVscodeTools = settings.get<boolean>('agent.vscodeTools', true);
    if (useVscodeTools) {
      await startMcpServers();
    }
    const registry = buildRegistry(useVscodeTools ? selectVscodeTools(vscode.lm.tools) : [], runVscodeTool);
    const tools = [...registry.values()].map((t) => t.def);
    const signal = AbortSignal.any([cancel, AbortSignal.timeout(AGENT_TURN_TIMEOUT_MS)]);
    const client = createClient(cfg);
    const note = (text: string) => this.post(webview, { type: 'delta', text });

    const turn = await runAgentTurn(
      this.history,
      {
        send: (messages, offered, toolChoice) =>
          streamChatTurn(client, {
            model: cfg.model,
            messages,
            tools: offered,
            toolChoice,
            signal,
            onDelta: note,
            explain,
            attachments,
          }),
        runTool: (call, toolSignal) => {
          const tool = registry.get(call.function.name);
          if (!tool) {
            return Promise.reject(new Error(`unknown tool "${call.function.name}"`));
          }
          return tool.run(call.function.arguments, toolSignal);
        },
        // "changes" (default) leaves approval to VS Code, which asks before
        // any tool that isn't read-only; "all" also asks here first.
        approve: (call) =>
          settings.get<string>('agent.approval', 'changes') === 'all'
            ? this.askInSidebar(webview, call.id, call.function.name, call.function.arguments, signal)
            : Promise.resolve(true),
        onToolCall: (call) =>
          this.post(webview, { type: 'toolCall', id: call.id, name: call.function.name, argsPreview: preview(call.function.arguments || '{}', 300) }),
        onToolResult: (call, ok, text) => this.post(webview, { type: 'toolResult', id: call.id, ok, summary: preview(text, 300) }),
      },
      {
        tools,
        maxSteps: settings.get<number>('agent.maxSteps', 10),
        maxToolResultChars: settings.get<number>('agent.maxToolResultChars', 20_000),
        signal,
      },
    );
    if (turn.hitStepLimit) {
      note('\n\n_Stopped after the maximum number of tool steps (hupi.agent.maxSteps)._');
    }
    if (turn.final.citations.length > 0) {
      this.post(webview, { type: 'citations', items: turn.final.citations });
    }
    if (turn.final.attachmentWarnings.length > 0) {
      this.post(webview, { type: 'attachmentWarnings', warnings: turn.final.attachmentWarnings });
    }
    return turn.added;
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
    .fileContextBadge {
      margin-top: 6px; font-size: 11px;
      color: var(--vscode-descriptionForeground);
    }
    .citations {
      margin-top: 8px; padding-top: 6px;
      border-top: 1px solid var(--hupi-border);
      font-size: 11.5px; color: var(--vscode-descriptionForeground);
    }
    .citations .citationsTitle {
      font-weight: 600; font-size: 10.5px; letter-spacing: 0.06em;
      text-transform: uppercase; margin-bottom: 4px;
    }
    .citations ul { margin: 0; padding-left: 16px; }
    .citations li { margin-bottom: 3px; }
    .citations .used { color: var(--vscode-charts-green, var(--vscode-descriptionForeground)); }
    .citations .unused { opacity: 0.7; }
    .attachmentWarning {
      margin-top: 8px; padding-top: 6px;
      border-top: 1px solid var(--hupi-border);
      font-size: 11px; font-style: italic;
      color: var(--vscode-descriptionForeground);
    }
    .transientNote {
      margin: 0 8px 6px; padding: 4px 8px;
      font-size: 11.5px; color: var(--vscode-descriptionForeground);
      background: var(--hupi-card-bg); border: 1px solid var(--hupi-border);
      border-radius: 4px;
    }
    #attachmentChips {
      display: none; flex-wrap: wrap; gap: 6px;
      margin: 0 8px 6px; flex-shrink: 0;
    }
    .attachmentChip {
      display: flex; align-items: center; gap: 4px;
      background: var(--hupi-card-bg); border: 1px solid var(--hupi-border);
      border-radius: 12px; padding: 2px 4px 2px 8px; font-size: 11.5px;
      max-width: 100%; overflow: hidden; white-space: nowrap; text-overflow: ellipsis;
    }
    .attachmentChipRemove {
      background: transparent; border: none; color: var(--vscode-descriptionForeground);
      cursor: pointer; font-size: 13px; line-height: 1; padding: 2px 4px; border-radius: 50%;
    }
    .attachmentChipRemove:hover { background: var(--vscode-toolbar-hoverBackground, var(--hupi-border)); }
    #inputRow {
      display: flex; align-items: flex-end;
      border-top: 1px solid var(--hupi-border); padding: 8px; gap: 6px; flex-shrink: 0;
    }
    #inputRow.dragOver { outline: 2px dashed var(--vscode-focusBorder); outline-offset: -2px; }
    #attachBtn {
      display: flex; align-items: center; justify-content: center;
      background: transparent; color: var(--vscode-foreground);
      border: 1px solid var(--hupi-border); border-radius: 4px;
      width: 28px; height: 28px; flex-shrink: 0; cursor: pointer;
    }
    #attachBtn:hover { background: var(--vscode-toolbar-hoverBackground, var(--hupi-card-bg)); }
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
    #stop {
      background: var(--vscode-button-secondaryBackground, transparent); color: var(--vscode-button-secondaryForeground, var(--vscode-foreground));
      border: 1px solid var(--hupi-border); border-radius: 4px; padding: 0 12px; cursor: pointer; font-size: inherit;
    }
    #stop:hover { background: var(--vscode-button-secondaryHoverBackground, var(--hupi-card-bg)); }
    .segment + .segment, .toolCard + .segment { margin-top: 6px; }
    .toolCard {
      margin: 6px 0; padding: 5px 8px;
      border: 1px solid var(--hupi-border); border-radius: 4px;
      font-size: 11.5px; white-space: normal;
      background: var(--vscode-editor-background);
    }
    .toolHead { color: var(--vscode-descriptionForeground); }
    .toolName { font-family: var(--vscode-editor-font-family, monospace); color: var(--vscode-foreground); }
    .toolStatus { margin-left: 4px; font-style: italic; }
    .toolCard.ok .toolStatus { color: var(--vscode-charts-green, var(--vscode-descriptionForeground)); font-style: normal; }
    .toolCard.failed .toolStatus, .toolCard.denied .toolStatus { color: var(--vscode-errorForeground); font-style: normal; }
    .toolCard.waiting { border-color: var(--vscode-focusBorder); }
    .toolArgs {
      margin-top: 2px; font-family: var(--vscode-editor-font-family, monospace);
      color: var(--vscode-descriptionForeground); overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    }
    .toolActions { display: flex; gap: 6px; margin-top: 4px; }
    .toolActions:empty { display: none; }
    .toolButton {
      border: none; border-radius: 3px; padding: 2px 10px; cursor: pointer; font-size: 11.5px;
      background: var(--vscode-button-secondaryBackground, var(--hupi-card-bg)); color: var(--vscode-button-secondaryForeground, var(--vscode-foreground));
    }
    .toolButton.approve { background: var(--vscode-button-background); color: var(--vscode-button-foreground); }
    .toolButton:disabled { opacity: 0.5; cursor: default; }
    .toolCard details { margin-top: 3px; }
    .toolCard summary { cursor: pointer; color: var(--vscode-descriptionForeground); }
    .toolCard details pre { margin: 3px 0 0; white-space: pre-wrap; max-height: 160px; overflow-y: auto; }
    .stoppedNote { margin-top: 6px; font-size: 11px; font-style: italic; color: var(--vscode-descriptionForeground); }
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
    <div id="empty">Ask about your code, or anything HUPI already remembers from past conversations. Attach a file or image with the 📎 button or by dragging it in.</div>
  </div>
  <div id="attachmentChips"></div>
  <div id="inputRow">
    <button id="attachBtn" type="button" title="Attach a file or image">
      <svg width="14" height="14" viewBox="0 0 16 16" fill="none"><path d="M12.5 6.5 7 12a2.5 2.5 0 1 1-3.5-3.5l6-6a1.5 1.5 0 1 1 2 2.2l-5.6 5.6a.5.5 0 1 1-.7-.7l5-5" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" stroke-linejoin="round"/></svg>
    </button>
    <input id="fileInput" type="file" multiple style="display:none" />
    <textarea id="input" rows="2" placeholder="Ask HUPI... (Enter to send, Shift+Enter for newline)"></textarea>
    <button id="send">Send</button>
    <button id="stop" style="display:none" title="Stop this reply">Stop</button>
  </div>
  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
  }
}
