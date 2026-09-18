import * as vscode from 'vscode';
import type { OidcSettings, TokenSet } from './oidc';

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

// An OIDC session (access + refresh token) is at least as sensitive as an
// API key — same SecretStorage-only rule applies. The hupi.oidc.* settings
// it was issued under are stored alongside the tokens so a later settings
// change can be detected and invalidate the old session (see oidcAuth.ts's
// getValidAccessToken / settingsMatch) rather than silently keep using
// tokens scoped to a since-changed IdP config.
const OIDC_SESSION_KEY = 'hupi.oidcSession';

export type StoredOidcSession = OidcSettings & TokenSet & { obtainedAt: number };

export async function getOidcSession(context: vscode.ExtensionContext): Promise<StoredOidcSession | undefined> {
  const raw = await context.secrets.get(OIDC_SESSION_KEY);
  if (!raw) {
    return undefined;
  }
  try {
    return JSON.parse(raw) as StoredOidcSession;
  } catch {
    // Corrupt/unparseable — treat as "no session" rather than throwing;
    // the next sign-in overwrites it cleanly.
    return undefined;
  }
}

export async function storeOidcSession(context: vscode.ExtensionContext, session: StoredOidcSession): Promise<void> {
  await context.secrets.store(OIDC_SESSION_KEY, JSON.stringify(session));
}

export async function clearOidcSession(context: vscode.ExtensionContext): Promise<void> {
  await context.secrets.delete(OIDC_SESSION_KEY);
}
