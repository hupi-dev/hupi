import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import {
  __getRegisteredCommand,
  __resetVscodeMock,
  __setConfig,
  executeCommand,
  openExternal,
  showErrorMessage,
  showInformationMessage,
  withProgress,
} from './test/vscode-mock';
import { getOidcSession, storeOidcSession, type StoredOidcSession } from './secrets';

vi.mock('./oidc', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./oidc')>();
  return {
    ...actual,
    fetchDiscoveryDocument: vi.fn(),
    exchangeCodeForToken: vi.fn(),
    refreshAccessToken: vi.fn(),
  };
});

import { RefreshFailedError, exchangeCodeForToken, fetchDiscoveryDocument, refreshAccessToken } from './oidc';
import {
  getValidAccessToken,
  promptSignInRequired,
  readOidcSettings,
  registerOidcCommands,
  signIn,
  signOut,
} from './oidcAuth';

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

const settings = { issuerUrl: 'https://idp.example.com', clientAppId: 'client-1', scope: 'openid profile offline_access' };
const discovery = { authorization_endpoint: 'https://idp.example.com/authorize', token_endpoint: 'https://idp.example.com/token' };

function baseSession(overrides: Partial<StoredOidcSession> = {}): StoredOidcSession {
  return {
    ...settings,
    accessToken: 'at-original',
    refreshToken: 'rt-original',
    expiresAt: Date.now() + 3600_000,
    obtainedAt: Date.now(),
    ...overrides,
  };
}

beforeEach(() => {
  __resetVscodeMock();
  vi.mocked(fetchDiscoveryDocument).mockReset().mockResolvedValue(discovery);
  vi.mocked(exchangeCodeForToken).mockReset();
  vi.mocked(refreshAccessToken).mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('readOidcSettings', () => {
  it('reads and trims hupi.oidc.* settings', () => {
    __setConfig({ 'oidc.issuerUrl': '  https://idp.example.com  ', 'oidc.clientAppId': ' client-1 ', 'oidc.scope': ' openid ' });
    expect(readOidcSettings()).toEqual({
      issuerUrl: 'https://idp.example.com',
      clientAppId: 'client-1',
      scope: 'openid',
    });
  });

  it('defaults to empty strings when unset', () => {
    __setConfig({});
    expect(readOidcSettings()).toEqual({ issuerUrl: '', clientAppId: '', scope: '' });
  });
});

describe('getValidAccessToken', () => {
  it('returns undefined when there is no stored session', async () => {
    const context = fakeContext();
    expect(await getValidAccessToken(context, settings)).toBeUndefined();
  });

  it('returns the access token unchanged when not expiring soon', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession());
    expect(await getValidAccessToken(context, settings)).toBe('at-original');
    expect(refreshAccessToken).not.toHaveBeenCalled();
  });

  it('returns undefined without touching the stored session when settings no longer match', async () => {
    const context = fakeContext();
    // Built once and reused for both the store and the expectation below —
    // baseSession() calls Date.now() internally, so calling it a second
    // time just for the comparison was a real, if rare, source of flakiness
    // (the two Date.now() calls can land a millisecond apart under load).
    const session = baseSession();
    await storeOidcSession(context, session);
    const differentSettings = { ...settings, clientAppId: 'a-different-client' };

    expect(await getValidAccessToken(context, differentSettings)).toBeUndefined();
    // The mismatched session is left alone, not cleared — confirmed by
    // still being readable under the *original* settings.
    expect(await getOidcSession(context)).toEqual(session);
  });

  it('refreshes and stores a new session when expiring soon and a refresh token exists', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession({ expiresAt: Date.now() + 1000 }));
    vi.mocked(refreshAccessToken).mockResolvedValue({ accessToken: 'at-refreshed', refreshToken: 'rt-refreshed', expiresAt: Date.now() + 3600_000 });

    const token = await getValidAccessToken(context, settings);

    expect(token).toBe('at-refreshed');
    expect(refreshAccessToken).toHaveBeenCalledWith(discovery.token_endpoint, { clientAppId: settings.clientAppId, refreshToken: 'rt-original' });
    const stored = await getOidcSession(context);
    expect(stored?.accessToken).toBe('at-refreshed');
  });

  it('clears the session and returns undefined when expiring soon with no refresh token', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession({ expiresAt: Date.now() + 1000, refreshToken: undefined }));

    expect(await getValidAccessToken(context, settings)).toBeUndefined();
    expect(await getOidcSession(context)).toBeUndefined();
    expect(fetchDiscoveryDocument).not.toHaveBeenCalled();
  });

  it('clears the session and returns undefined when the refresh token is dead (RefreshFailedError)', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession({ expiresAt: Date.now() + 1000 }));
    vi.mocked(refreshAccessToken).mockRejectedValue(new RefreshFailedError('revoked'));

    expect(await getValidAccessToken(context, settings)).toBeUndefined();
    expect(await getOidcSession(context)).toBeUndefined();
  });

  it('propagates a transient refresh error instead of clearing the session', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession({ expiresAt: Date.now() + 1000 }));
    vi.mocked(refreshAccessToken).mockRejectedValue(new Error('network blip'));

    await expect(getValidAccessToken(context, settings)).rejects.toThrow('network blip');
    // Not cleared — a transient error shouldn't force a fresh interactive
    // sign-in for what might just be a blip.
    expect(await getOidcSession(context)).toBeDefined();
  });
});

describe('signIn', () => {
  it('shows an error and never touches the network when settings are incomplete', async () => {
    __setConfig({ 'oidc.issuerUrl': 'https://idp.example.com', 'oidc.clientAppId': '', 'oidc.scope': '' });
    const context = fakeContext();

    await signIn(context);

    expect(showErrorMessage).toHaveBeenCalledWith(expect.stringContaining('hupi.oidc.issuerUrl'));
    expect(fetchDiscoveryDocument).not.toHaveBeenCalled();
  });

  it('completes a full loopback round trip and stores the resulting session', async () => {
    __setConfig({ 'oidc.issuerUrl': settings.issuerUrl, 'oidc.clientAppId': settings.clientAppId, 'oidc.scope': settings.scope });
    const context = fakeContext();
    const tokenSet = { accessToken: 'at-new', refreshToken: 'rt-new', expiresAt: Date.now() + 3600_000 };
    vi.mocked(exchangeCodeForToken).mockResolvedValue(tokenSet);

    // Stands in for a real browser: parses the real authorization URL
    // built by the real buildAuthorizationUrl, then fires a real HTTP
    // request at the real loopback server runLoopbackAuth started —
    // exercising the actual (unexported) waitForCallback/listenOnEphemeralPort
    // code, not a mock of it.
    openExternal.mockImplementation(async (uri: { toString(): string }) => {
      const authUrl = new URL(uri.toString());
      expect(authUrl.origin + authUrl.pathname).toBe(discovery.authorization_endpoint);
      const redirectUri = authUrl.searchParams.get('redirect_uri')!;
      const state = authUrl.searchParams.get('state')!;
      expect(redirectUri).toMatch(/^http:\/\/localhost:\d+$/);

      const callbackUrl = new URL(redirectUri);
      callbackUrl.searchParams.set('state', state);
      callbackUrl.searchParams.set('code', 'fake-auth-code');
      const res = await fetch(callbackUrl.toString());
      expect(res.status).toBe(200);
      return true;
    });

    await signIn(context);

    expect(exchangeCodeForToken).toHaveBeenCalledWith(
      discovery.token_endpoint,
      expect.objectContaining({ clientAppId: settings.clientAppId, code: 'fake-auth-code' }),
    );
    const stored = await getOidcSession(context);
    expect(stored?.accessToken).toBe('at-new');
    expect(stored?.refreshToken).toBe('rt-new');
    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: signed in.');
  });

  it('shows an error when the IdP redirects back with an error (user denied consent)', async () => {
    __setConfig({ 'oidc.issuerUrl': settings.issuerUrl, 'oidc.clientAppId': settings.clientAppId, 'oidc.scope': settings.scope });
    const context = fakeContext();

    openExternal.mockImplementation(async (uri: { toString(): string }) => {
      const authUrl = new URL(uri.toString());
      const redirectUri = authUrl.searchParams.get('redirect_uri')!;
      const callbackUrl = new URL(redirectUri);
      callbackUrl.searchParams.set('error', 'access_denied');
      callbackUrl.searchParams.set('error_description', 'user declined');
      await fetch(callbackUrl.toString());
      return true;
    });

    await signIn(context);

    expect(showErrorMessage).toHaveBeenCalledWith(expect.stringContaining('user declined'));
    expect(exchangeCodeForToken).not.toHaveBeenCalled();
    expect(await getOidcSession(context)).toBeUndefined();
  });

  it('shows an error on a state mismatch', async () => {
    __setConfig({ 'oidc.issuerUrl': settings.issuerUrl, 'oidc.clientAppId': settings.clientAppId, 'oidc.scope': settings.scope });
    const context = fakeContext();

    openExternal.mockImplementation(async (uri: { toString(): string }) => {
      const authUrl = new URL(uri.toString());
      const redirectUri = authUrl.searchParams.get('redirect_uri')!;
      const callbackUrl = new URL(redirectUri);
      callbackUrl.searchParams.set('state', 'not-the-real-state');
      callbackUrl.searchParams.set('code', 'irrelevant');
      await fetch(callbackUrl.toString());
      return true;
    });

    await signIn(context);

    expect(showErrorMessage).toHaveBeenCalledWith(expect.stringContaining('sign-in failed'));
    expect(exchangeCodeForToken).not.toHaveBeenCalled();
  });

  it('shows an error when the browser fails to open', async () => {
    __setConfig({ 'oidc.issuerUrl': settings.issuerUrl, 'oidc.clientAppId': settings.clientAppId, 'oidc.scope': settings.scope });
    const context = fakeContext();
    openExternal.mockResolvedValue(false);

    await signIn(context);

    expect(showErrorMessage).toHaveBeenCalledWith(expect.stringContaining('could not open the system browser'));
  });

  it('does not show an error when cancelled', async () => {
    __setConfig({ 'oidc.issuerUrl': settings.issuerUrl, 'oidc.clientAppId': settings.clientAppId, 'oidc.scope': settings.scope });
    const context = fakeContext();

    // withProgress's real signature hands the task a CancellationToken —
    // simulate the user cancelling by immediately invoking whatever
    // handler runLoopbackAuth registered via onCancellationRequested.
    withProgress.mockImplementation(async (_options: unknown, task: (...args: any[]) => unknown) => {
      const fakeProgress = { report() {} };
      const fakeCancellationToken = {
        onCancellationRequested: (cb: () => void) => {
          queueMicrotask(cb);
          return { dispose() {} };
        },
      };
      return task(fakeProgress, fakeCancellationToken);
    });
    openExternal.mockImplementation(async () => true); // never actually completes the round trip

    await signIn(context);

    expect(showErrorMessage).not.toHaveBeenCalled();
    expect(showInformationMessage).not.toHaveBeenCalledWith('HUPI: signed in.');
  });
});

describe('signOut', () => {
  it('clears an existing session and confirms', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession());

    await signOut(context);

    expect(await getOidcSession(context)).toBeUndefined();
    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: signed out.');
  });

  it('reports "was not signed in" when there was no session', async () => {
    const context = fakeContext();
    await signOut(context);
    expect(showInformationMessage).toHaveBeenCalledWith('HUPI: was not signed in.');
  });
});

describe('registerOidcCommands', () => {
  it('registers hupi.signIn and hupi.signOut', () => {
    const context = fakeContext();
    registerOidcCommands(context);
    expect(__getRegisteredCommand('hupi.signIn')).toBeDefined();
    expect(__getRegisteredCommand('hupi.signOut')).toBeDefined();
  });

  it('hupi.signOut really invokes signOut for the given context', async () => {
    const context = fakeContext();
    await storeOidcSession(context, baseSession());
    registerOidcCommands(context);

    await __getRegisteredCommand('hupi.signOut')!();

    expect(await getOidcSession(context)).toBeUndefined();
  });
});

describe('promptSignInRequired', () => {
  it('shows a prompt and runs hupi.signIn when the user picks "Sign In"', async () => {
    const context = fakeContext();
    registerOidcCommands(context); // so executeCommand('hupi.signIn') has something real to dispatch to
    __setConfig({ 'oidc.issuerUrl': '', 'oidc.clientAppId': '', 'oidc.scope': '' }); // signIn will just show its own error, that's fine
    showErrorMessage.mockResolvedValueOnce('Sign In');

    promptSignInRequired();
    await vi.waitFor(() => expect(executeCommand).toHaveBeenCalledWith('hupi.signIn'));
  });

  it('does nothing further when the prompt is dismissed', async () => {
    showErrorMessage.mockResolvedValueOnce(undefined);
    promptSignInRequired();
    await Promise.resolve();
    expect(executeCommand).not.toHaveBeenCalled();
  });
});
