import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __resetVscodeMock, __setConfig } from './test/vscode-mock';
import { setApiKey } from './secrets';

vi.mock('./oidcAuth', () => ({
  getValidAccessToken: vi.fn(),
  readOidcSettings: vi.fn(),
}));

import { getValidAccessToken, readOidcSettings } from './oidcAuth';
import { loadConfig, OidcSignInRequiredError } from './config';

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
  vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: '', clientAppId: '', scope: '' });
  vi.mocked(getValidAccessToken).mockReset();
});

describe('loadConfig', () => {
  it('reads baseUrl/model/teamId from workspace config, with defaults', async () => {
    const context = fakeContext();
    __setConfig({});
    const cfg = await loadConfig(context);
    expect(cfg.baseUrl).toBe('http://localhost:8787');
    expect(cfg.model).toBe('');
    expect(cfg.teamId).toBe('');
  });

  it('honors explicitly set baseUrl/model/teamId', async () => {
    const context = fakeContext();
    __setConfig({ baseUrl: 'https://hupi.example.com', model: 'work-openai', teamId: 'acme-eng' });
    const cfg = await loadConfig(context);
    expect(cfg.baseUrl).toBe('https://hupi.example.com');
    expect(cfg.model).toBe('work-openai');
    expect(cfg.teamId).toBe('acme-eng');
  });

  it('falls back to the stored API key when OIDC is not configured', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_direct');
    vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: '', clientAppId: '', scope: '' });

    const cfg = await loadConfig(context);
    expect(cfg.apiKey).toBe('hupi_sk_direct');
    expect(getValidAccessToken).not.toHaveBeenCalled();
  });

  it('uses the OIDC access token when issuerUrl and clientAppId are both set', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_should_be_ignored');
    const oidc = { issuerUrl: 'https://idp.example.com', clientAppId: 'client-1', scope: 'openid' };
    vi.mocked(readOidcSettings).mockReturnValue(oidc);
    vi.mocked(getValidAccessToken).mockResolvedValue('real-oidc-token');

    const cfg = await loadConfig(context);
    expect(cfg.apiKey).toBe('real-oidc-token');
    expect(getValidAccessToken).toHaveBeenCalledWith(context, oidc);
  });

  it('throws OidcSignInRequiredError, not falling back to the stored API key, when OIDC is configured but has no valid session', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_must_not_be_used');
    vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: 'https://idp.example.com', clientAppId: 'client-1', scope: 'openid' });
    vi.mocked(getValidAccessToken).mockResolvedValue(undefined);

    await expect(loadConfig(context)).rejects.toBeInstanceOf(OidcSignInRequiredError);
  });

  it('does not treat only one of issuerUrl/clientAppId being set as "OIDC configured"', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_fallback');
    vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: 'https://idp.example.com', clientAppId: '', scope: '' });

    const cfg = await loadConfig(context);
    expect(cfg.apiKey).toBe('hupi_sk_fallback');
    expect(getValidAccessToken).not.toHaveBeenCalled();
  });
});
