import * as vscode from 'vscode';

// The API key is a bearer credential with the same blast radius as any
// other HUPI API key (docs/ADMIN_UI.md: "treat every API key with the same
// care as a password") — it never touches plain settings.json/settings
// sync, only VS Code's SecretStorage (OS keychain-backed).
const SECRET_KEY = 'hupi.apiKey';

export async function getApiKey(context: vscode.ExtensionContext): Promise<string> {
  return (await context.secrets.get(SECRET_KEY)) ?? '';
}

export async function setApiKey(context: vscode.ExtensionContext, value: string): Promise<void> {
  await context.secrets.store(SECRET_KEY, value);
}

export function registerSetApiKeyCommand(context: vscode.ExtensionContext): vscode.Disposable {
  return vscode.commands.registerCommand('hupi.setApiKey', async () => {
    const value = await vscode.window.showInputBox({
      prompt: 'HUPI API key (from `hupi-admin create-key` or the admin UI — shown once, paste it now)',
      password: true,
      ignoreFocusOut: true,
    });
    if (value === undefined) {
      return; // user cancelled
    }
    await setApiKey(context, value.trim());
    vscode.window.showInformationMessage('HUPI API key saved.');
  });
}
