import * as vscode from 'vscode';
import { getApiKey } from './secrets';
import { getValidAccessToken, readOidcSettings } from './oidcAuth';
import type { HupiConfig } from './hupiClient';

/**
 * Thrown by loadConfig when hupi.oidc.issuerUrl/clientAppId are set (OIDC
 * is authoritative for this workspace) but there's no valid session —
 * callers should catch this specifically and prompt sign-in
 * (oidcAuth.ts's promptSignInRequired) rather than showing a generic
 * config-error message.
 */
export class OidcSignInRequiredError extends Error {
  constructor() {
    super('Sign in to HUPI (HUPI: Sign In) to continue.');
    this.name = 'OidcSignInRequiredError';
  }
}

export async function loadConfig(context: vscode.ExtensionContext): Promise<HupiConfig> {
  const cfg = vscode.workspace.getConfiguration('hupi');
  const base = {
    baseUrl: cfg.get<string>('baseUrl', 'http://localhost:8787'),
    model: cfg.get<string>('model', ''),
    teamId: cfg.get<string>('teamId', ''),
  };

  const oidc = readOidcSettings();
  if (oidc.issuerUrl && oidc.clientAppId) {
    // OIDC is configured for this workspace — authoritative once set, so
    // this never silently falls back to a possibly-stale stored API key
    // that could belong to the wrong identity/team.
    const token = await getValidAccessToken(context, oidc);
    if (!token) {
      throw new OidcSignInRequiredError();
    }
    return { ...base, apiKey: token };
  }

  return { ...base, apiKey: await getApiKey(context) };
}
