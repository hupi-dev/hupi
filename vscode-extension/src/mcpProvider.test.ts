import { beforeEach, describe, expect, it, vi } from 'vitest';
import * as vscode from 'vscode';
import { __fireConfigChange, __resetVscodeMock, __setConfig, registerMcpServerDefinitionProvider } from './test/vscode-mock';
import { setApiKey } from './secrets';

vi.mock('./oidcAuth', () => ({
  getValidAccessToken: vi.fn(),
  readOidcSettings: vi.fn(),
  promptSignInRequired: vi.fn(),
}));

import { getValidAccessToken, promptSignInRequired, readOidcSettings } from './oidcAuth';
import { HupiMcpProvider, MCP_PROVIDER_ID, mcpUrl, registerMcpProvider } from './mcpProvider';

type SecretListener = (e: { key: string }) => void;

function fakeContext(): vscode.ExtensionContext & { __fireSecretChange(key: string): void } {
  const store = new Map<string, string>();
  let listeners: SecretListener[] = [];
  return {
    secrets: {
      get: async (key: string) => store.get(key),
      store: async (key: string, value: string) => {
        store.set(key, value);
        listeners.forEach((l) => l({ key }));
      },
      delete: async (key: string) => {
        store.delete(key);
      },
      onDidChange: (l: SecretListener) => {
        listeners.push(l);
        return new vscode.Disposable(() => {
          listeners = listeners.filter((x) => x !== l);
        });
      },
    },
    __fireSecretChange: (key: string) => listeners.forEach((l) => l({ key })),
  } as unknown as vscode.ExtensionContext & { __fireSecretChange(key: string): void };
}

beforeEach(() => {
  __resetVscodeMock();
  vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: '', clientAppId: '', scope: '' });
  vi.mocked(getValidAccessToken).mockReset();
  vi.mocked(promptSignInRequired).mockReset();
});

describe('mcpUrl', () => {
  it('builds the private and team MCP endpoints', () => {
    expect(mcpUrl('http://localhost:8787')).toBe('http://localhost:8787/mcp');
    expect(mcpUrl('https://hupi.example.com///', '  ')).toBe('https://hupi.example.com/mcp');
    expect(mcpUrl('https://hupi.example.com', 'team:acme eng')).toBe('https://hupi.example.com/v1/team/team%3Aacme%20eng/mcp');
  });
});

describe('HupiMcpProvider', () => {
  it('offers one HTTP server at the configured URL, without credentials', async () => {
    __setConfig({ baseUrl: 'https://hupi.example.com', teamId: 'acme' });
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_secret');
    const defs = new HupiMcpProvider(context).provideMcpServerDefinitions();
    expect(defs).toHaveLength(1);
    expect(defs[0].label).toBe('HUPI memory');
    expect(defs[0].uri.toString()).toBe('https://hupi.example.com/v1/team/acme/mcp');
    expect(defs[0].headers).toEqual({});
  });

  it('offers nothing when hupi.mcp.enabled is off', () => {
    __setConfig({ 'mcp.enabled': false });
    expect(new HupiMcpProvider(fakeContext()).provideMcpServerDefinitions()).toEqual([]);
  });

  it('attaches the stored API key when the server starts', async () => {
    const context = fakeContext();
    await setApiKey(context, 'hupi_sk_secret');
    const provider = new HupiMcpProvider(context);
    const [def] = provider.provideMcpServerDefinitions();
    const resolved = await provider.resolveMcpServerDefinition(def);
    expect(resolved?.headers).toEqual({ Authorization: 'Bearer hupi_sk_secret' });
  });

  it('sends no Authorization header on a no-auth (Tier 1/2) setup', async () => {
    const provider = new HupiMcpProvider(fakeContext());
    const [def] = provider.provideMcpServerDefinitions();
    expect((await provider.resolveMcpServerDefinition(def))?.headers).toEqual({});
  });

  it('uses the OIDC access token when OIDC is configured', async () => {
    vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: 'https://idp', clientAppId: 'app', scope: 's' });
    vi.mocked(getValidAccessToken).mockResolvedValue('oidc-access-token');
    const context = fakeContext();
    await setApiKey(context, 'stale-api-key');
    const provider = new HupiMcpProvider(context);
    const [def] = provider.provideMcpServerDefinitions();
    expect((await provider.resolveMcpServerDefinition(def))?.headers).toEqual({ Authorization: 'Bearer oidc-access-token' });
  });

  it('prompts sign-in and does not start when OIDC has no session', async () => {
    vi.mocked(readOidcSettings).mockReturnValue({ issuerUrl: 'https://idp', clientAppId: 'app', scope: 's' });
    vi.mocked(getValidAccessToken).mockResolvedValue(undefined);
    const provider = new HupiMcpProvider(fakeContext());
    const [def] = provider.provideMcpServerDefinitions();
    expect(await provider.resolveMcpServerDefinition(def)).toBeUndefined();
    expect(promptSignInRequired).toHaveBeenCalledOnce();
  });

  it('announces a change when relevant settings or credentials change', async () => {
    const context = fakeContext();
    const provider = new HupiMcpProvider(context);
    const disposables = provider.watch();
    const fired = vi.fn();
    provider.onDidChangeMcpServerDefinitions(fired);

    __fireConfigChange(['hupi.baseUrl']);
    __fireConfigChange(['hupi.oidc.scope']);
    __fireConfigChange(['hupi.mcp.enabled']);
    __fireConfigChange(['hupi.model']); // irrelevant
    __fireConfigChange(['editor.fontSize']); // irrelevant
    await setApiKey(context, 'new-key');
    context.__fireSecretChange('hupi.oidcSession');
    context.__fireSecretChange('something.else'); // irrelevant
    expect(fired).toHaveBeenCalledTimes(5);

    disposables.forEach((d) => d.dispose());
    __fireConfigChange(['hupi.baseUrl']);
    expect(fired).toHaveBeenCalledTimes(5);
  });
});

describe('registerMcpProvider', () => {
  it('registers under the id contributed in package.json', async () => {
    const pkg = (await import('../package.json')).default as {
      engines: { vscode: string };
      contributes: { mcpServerDefinitionProviders: { id: string }[] };
    };
    const disposables = registerMcpProvider(fakeContext());
    expect(registerMcpServerDefinitionProvider).toHaveBeenCalledWith(MCP_PROVIDER_ID, expect.any(HupiMcpProvider));
    expect(pkg.contributes.mcpServerDefinitionProviders.map((p) => p.id)).toEqual([MCP_PROVIDER_ID]);
    expect(pkg.engines.vscode).toBe('^1.101.0');
    disposables.forEach((d) => d.dispose());
  });
});
