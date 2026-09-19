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
    vscode.window.registerWebviewViewProvider(HupiChatViewProvider.viewType, chatProvider),
  );

  context.subscriptions.push(registerChatParticipant(context));
}

export function deactivate(): void {}
