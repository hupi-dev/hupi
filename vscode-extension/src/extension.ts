import * as vscode from 'vscode';
import { registerSetApiKeyCommand } from './secrets';
import { registerOidcCommands } from './oidcAuth';
import { registerInlineEdit } from './inlineEdit';
import { HupiChatViewProvider } from './chatViewProvider';
import { registerChatParticipant } from './chatParticipant';
import { registerInlineCompletions } from './inlineCompletionProvider';
import { registerMultiFileEdit } from './multiFileEdit';

export function activate(context: vscode.ExtensionContext): void {
  context.subscriptions.push(registerSetApiKeyCommand(context));
  context.subscriptions.push(...registerOidcCommands(context));
  context.subscriptions.push(...registerInlineEdit(context));
  context.subscriptions.push(...registerMultiFileEdit(context));
  context.subscriptions.push(registerInlineCompletions(context));

  const chatProvider = new HupiChatViewProvider(context, context.extensionUri);
  context.subscriptions.push(
    // retainContextWhenHidden (finding B27): without it, VS Code tears
    // down and recreates the webview's DOM/JS state every time the user
    // hides and reopens the sidebar — the extension host still holds the
    // conversation history (HupiChatViewProvider's own `history` field),
    // but the visible panel comes back blank, with no indication that the
    // next message will still carry that context. Keeping the webview's
    // page alive is a one-line fix that exactly matches live-chat
    // rendering (citations, the file-context badge) with no risk of a
    // separate replay mechanism drifting out of sync with it — the usual
    // memory-overhead tradeoff is reasonable for one always-present
    // single-instance sidebar view.
    vscode.window.registerWebviewViewProvider(HupiChatViewProvider.viewType, chatProvider, {
      webviewOptions: { retainContextWhenHidden: true },
    }),
  );

  context.subscriptions.push(registerChatParticipant(context));
}

export function deactivate(): void {}
