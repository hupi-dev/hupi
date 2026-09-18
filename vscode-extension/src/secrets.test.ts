import { beforeEach, describe, expect, it } from 'vitest';
import * as vscode from 'vscode';
import { __getRegisteredCommand, __resetVscodeMock, showInformationMessage, showInputBox } from './test/vscode-mock';
import {
  clearOidcSession,
  getApiKey,
  getOidcSession,
  registerSetApiKeyCommand,
  setApiKey,
  storeOidcSession,
  type StoredOidcSession,
} from './secrets';

// A minimal in-memory SecretStorage — real enough for these tests, cast
// through `as unknown as vscode.ExtensionContext` since it doesn't (and
// doesn't need to) implement the full real interface.
function fakeContext(): vscode.ExtensionContext {
  const store = new Map<string, string>();
  return {
    secrets: {
      get: async (key: string) => store.get(key),
      store: async (key: string, value: string) => {
        store.set(key, value);
      },
      delete: async (key: string) => {
        store.delete(key);
      },
    },
  } as unknown as vscode.ExtensionContext;
}

beforeEach(() => {
  __resetVscodeMock();
});

describe('getApiKey/setApiKey', () => {
  it('returns an empty string when nothing is stored', async () => {
    const context = fakeContext();
    expect(await getApiKey(context)).toBe('');
  });

  it('round-trips a stored key', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_abc123');
    expect(await getApiKey(context)).toBe('hupi_sk_abc123');
  });
});

describe('registerSetApiKeyCommand', () => {
  it('registers hupi.setApiKey', () => {
    const context = fakeContext();
    registerSetApiKeyCommand(context);
    expect(__getRegisteredCommand('hupi.setApiKey')).toBeDefined();
  });

  it('trims and stores the entered key, then confirms', async () => {
    const context = fakeContext();
    showInputBox.mockResolvedValue('  hupi_sk_from_input  ');
    registerSetApiKeyCommand(context);

    await __getRegisteredCommand('hupi.setApiKey')!();

    expect(await getApiKey(context)).toBe('hupi_sk_from_input');
    expect(showInformationMessage).toHaveBeenCalledWith('HUPI API key saved.');
  });

  it('does nothing when the input box is cancelled', async () => {
    const context = fakeContext();
    showInputBox.mockResolvedValue(undefined);
    registerSetApiKeyCommand(context);

    await __getRegisteredCommand('hupi.setApiKey')!();

    expect(await getApiKey(context)).toBe('');
    expect(showInformationMessage).not.toHaveBeenCalled();
  });
});

describe('OIDC session storage', () => {
  const session: StoredOidcSession = {
    issuerUrl: 'https://idp.example.com',
    clientAppId: 'client-1',
    scope: 'openid',
    accessToken: 'at-1',
    refreshToken: 'rt-1',
    expiresAt: Date.now() + 3600_000,
    obtainedAt: Date.now(),
  };

  it('returns undefined when nothing is stored', async () => {
    const context = fakeContext();
    expect(await getOidcSession(context)).toBeUndefined();
  });

  it('round-trips a stored session', async () => {
    const context = fakeContext();
    await storeOidcSession(context, session);
    expect(await getOidcSession(context)).toEqual(session);
  });

  it('clearOidcSession removes it', async () => {
    const context = fakeContext();
    await storeOidcSession(context, session);
    await clearOidcSession(context);
    expect(await getOidcSession(context)).toBeUndefined();
  });

  it('treats corrupt stored JSON as no session rather than throwing', async () => {
    const context = fakeContext();
    await context.secrets.store('hupi.oidcSession', 'not valid json{');
    await expect(getOidcSession(context)).resolves.toBeUndefined();
  });
});
