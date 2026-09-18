import { createHash } from 'node:crypto';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  buildAuthorizationUrl,
  codeChallengeFromVerifier,
  discoveryUrl,
  exchangeCodeForToken,
  fetchDiscoveryDocument,
  generateCodeVerifier,
  isExpiringSoon,
  RefreshFailedError,
  refreshAccessToken,
  settingsMatch,
  TokenEndpointRequestError,
  type DiscoveryDocument,
} from './oidc';

function fakeFetchResponse(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as Response;
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('generateCodeVerifier', () => {
  it('produces an RFC 7636-compliant verifier (43-128 chars, unreserved alphabet)', () => {
    const verifier = generateCodeVerifier();
    expect(verifier.length).toBeGreaterThanOrEqual(43);
    expect(verifier.length).toBeLessThanOrEqual(128);
    expect(verifier).toMatch(/^[A-Za-z0-9\-._~]+$/);
  });

  it('is not the same value twice', () => {
    expect(generateCodeVerifier()).not.toBe(generateCodeVerifier());
  });
});

describe('codeChallengeFromVerifier', () => {
  it('is the base64url SHA-256 digest of the verifier, computed independently here', () => {
    const verifier = 'fixed-test-verifier-for-determinism-check-only';
    const expected = createHash('sha256').update(verifier).digest('base64url');
    expect(codeChallengeFromVerifier(verifier)).toBe(expected);
  });

  it('is deterministic for the same input', () => {
    const verifier = generateCodeVerifier();
    expect(codeChallengeFromVerifier(verifier)).toBe(codeChallengeFromVerifier(verifier));
  });
});

describe('discoveryUrl', () => {
  it('appends the well-known path', () => {
    expect(discoveryUrl('https://login.microsoftonline.com/tenant/v2.0')).toBe(
      'https://login.microsoftonline.com/tenant/v2.0/.well-known/openid-configuration',
    );
  });

  it('strips a trailing slash before appending', () => {
    expect(discoveryUrl('https://login.microsoftonline.com/tenant/v2.0/')).toBe(
      'https://login.microsoftonline.com/tenant/v2.0/.well-known/openid-configuration',
    );
  });
});

describe('fetchDiscoveryDocument', () => {
  it('returns the parsed document on success', async () => {
    const doc: DiscoveryDocument = {
      authorization_endpoint: 'https://idp.example.com/authorize',
      token_endpoint: 'https://idp.example.com/token',
    };
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(fakeFetchResponse(200, doc));
    await expect(fetchDiscoveryDocument('https://idp.example.com')).resolves.toEqual(doc);
  });

  it('throws on a non-2xx response', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(fakeFetchResponse(500, {}));
    await expect(fetchDiscoveryDocument('https://idp.example.com')).rejects.toThrow(/discovery failed \(500\)/);
  });

  it('throws when required fields are missing', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(fakeFetchResponse(200, { issuer: 'https://idp.example.com' }));
    await expect(fetchDiscoveryDocument('https://idp.example.com')).rejects.toThrow(/missing authorization_endpoint/);
  });
});

describe('buildAuthorizationUrl', () => {
  it('includes every required PKCE/OAuth query parameter', () => {
    const discovery: DiscoveryDocument = {
      authorization_endpoint: 'https://idp.example.com/authorize',
      token_endpoint: 'https://idp.example.com/token',
    };
    const url = new URL(
      buildAuthorizationUrl(discovery, {
        clientAppId: 'client-123',
        scope: 'api://resource/access_as_user offline_access',
        redirectUri: 'http://127.0.0.1:12345/callback',
        state: 'state-abc',
        codeChallenge: 'challenge-xyz',
      }),
    );
    expect(url.origin + url.pathname).toBe('https://idp.example.com/authorize');
    expect(url.searchParams.get('response_type')).toBe('code');
    expect(url.searchParams.get('client_id')).toBe('client-123');
    expect(url.searchParams.get('redirect_uri')).toBe('http://127.0.0.1:12345/callback');
    expect(url.searchParams.get('scope')).toBe('api://resource/access_as_user offline_access');
    expect(url.searchParams.get('state')).toBe('state-abc');
    expect(url.searchParams.get('code_challenge')).toBe('challenge-xyz');
    expect(url.searchParams.get('code_challenge_method')).toBe('S256');
  });
});

describe('exchangeCodeForToken', () => {
  it('POSTs the authorization_code grant with the code_verifier', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(fakeFetchResponse(200, { access_token: 'at-1', refresh_token: 'rt-1', expires_in: 3600 }));

    const before = Date.now();
    const result = await exchangeCodeForToken('https://idp.example.com/token', {
      clientAppId: 'client-123',
      code: 'auth-code',
      redirectUri: 'http://127.0.0.1:12345/callback',
      codeVerifier: 'verifier-abc',
    });

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const [url, init] = fetchSpy.mock.calls[0];
    expect(url).toBe('https://idp.example.com/token');
    expect(init?.method).toBe('POST');
    expect(init?.headers).toEqual({ 'Content-Type': 'application/x-www-form-urlencoded' });
    const body = new URLSearchParams(init?.body as string);
    expect(body.get('grant_type')).toBe('authorization_code');
    expect(body.get('client_id')).toBe('client-123');
    expect(body.get('code')).toBe('auth-code');
    expect(body.get('redirect_uri')).toBe('http://127.0.0.1:12345/callback');
    expect(body.get('code_verifier')).toBe('verifier-abc');

    expect(result.accessToken).toBe('at-1');
    expect(result.refreshToken).toBe('rt-1');
    expect(result.expiresAt).toBeGreaterThanOrEqual(before + 3600 * 1000);
  });

  it('throws TokenEndpointRequestError with the IdP error code on failure', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      fakeFetchResponse(400, { error: 'invalid_grant', error_description: 'code expired' }),
    );
    await expect(
      exchangeCodeForToken('https://idp.example.com/token', {
        clientAppId: 'client-123',
        code: 'stale-code',
        redirectUri: 'http://127.0.0.1:12345/callback',
        codeVerifier: 'verifier-abc',
      }),
    ).rejects.toMatchObject({ name: 'TokenEndpointRequestError', code: 'invalid_grant', message: 'code expired' });
  });
});

describe('refreshAccessToken', () => {
  it('POSTs the refresh_token grant', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(fakeFetchResponse(200, { access_token: 'at-2', expires_in: 3600 }));

    await refreshAccessToken('https://idp.example.com/token', {
      clientAppId: 'client-123',
      refreshToken: 'rt-1',
    });

    const [, init] = fetchSpy.mock.calls[0];
    const body = new URLSearchParams(init?.body as string);
    expect(body.get('grant_type')).toBe('refresh_token');
    expect(body.get('refresh_token')).toBe('rt-1');
  });

  it('wraps an invalid_grant rejection in RefreshFailedError', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(
      fakeFetchResponse(400, { error: 'invalid_grant', error_description: 'token revoked' }),
    );
    await expect(
      refreshAccessToken('https://idp.example.com/token', { clientAppId: 'client-123', refreshToken: 'dead' }),
    ).rejects.toBeInstanceOf(RefreshFailedError);
  });

  it('propagates a non-invalid_grant failure as TokenEndpointRequestError, not RefreshFailedError', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(fakeFetchResponse(500, { error: 'server_error' }));
    const promise = refreshAccessToken('https://idp.example.com/token', {
      clientAppId: 'client-123',
      refreshToken: 'rt-1',
    });
    await expect(promise).rejects.toBeInstanceOf(TokenEndpointRequestError);
    await expect(promise).rejects.not.toBeInstanceOf(RefreshFailedError);
  });
});

describe('isExpiringSoon', () => {
  const now = 1_000_000;

  it('is false well before expiry', () => {
    expect(isExpiringSoon(now + 10 * 60 * 1000, now)).toBe(false);
  });

  it('is true within the default skew window', () => {
    expect(isExpiringSoon(now + 60 * 1000, now)).toBe(true);
  });

  it('is true once already expired', () => {
    expect(isExpiringSoon(now - 1, now)).toBe(true);
  });

  it('respects a custom skew', () => {
    expect(isExpiringSoon(now + 5000, now, 1000)).toBe(false);
    expect(isExpiringSoon(now + 500, now, 1000)).toBe(true);
  });
});

describe('settingsMatch', () => {
  const base = { issuerUrl: 'https://idp.example.com', clientAppId: 'client-123', scope: 'openid' };

  it('is true for identical settings', () => {
    expect(settingsMatch(base, { ...base })).toBe(true);
  });

  it('is false when any single field differs', () => {
    expect(settingsMatch(base, { ...base, issuerUrl: 'https://other.example.com' })).toBe(false);
    expect(settingsMatch(base, { ...base, clientAppId: 'client-456' })).toBe(false);
    expect(settingsMatch(base, { ...base, scope: 'openid profile' })).toBe(false);
  });
});
