import * as vscode from 'vscode';
import { registerSetApiKeyCommand } from './secrets';
import { registerOidcCommands } from './oidcAuth';
import { registerInlineEdit } from './inlineEdit';
import { HupiChatViewProvider } from './chatViewProvider';

export function activate(context: vscode.ExtensionContext): void {
  context.subscriptions.push(registerSetApiKeyCommand(context));
  context.subscriptions.push(...registerOidcCommands(context));
  context.subscriptions.push(...registerInlineEdit(context));

  const chatProvider = new HupiChatViewProvider(context, context.extensionUri);
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(HupiChatViewProvider.viewType, chatProvider),
  );
}

export function deactivate(): void {}
