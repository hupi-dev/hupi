// Pure OIDC/PKCE logic — no `vscode` import, so it's unit-testable in plain
// Node the same way hupiClient.ts is kept vscode-free (see oidc.test.ts).
// The vscode-dependent orchestration (browser launch, loopback listener,
// SecretStorage) lives in oidcAuth.ts, which calls into this module.
//
// This talks to whichever OIDC provider docs/OIDC.md's server-side
// implementation was configured against (Azure AD/Entra ID and others) —
// nothing here is Azure-specific, it's the standard Authorization Code +
// PKCE flow (RFC 7636) any compliant provider supports.
import { createHash, randomBytes, randomUUID } from 'node:crypto';

export interface OidcSettings {
  issuerUrl: string;
  /** The extension's own public-client app registration — NOT the same as
   *  the server's HUPI_OIDC_CLIENT_ID (see docs/OIDC.md's resource-app vs
   *  client-app split). */
  clientAppId: string;
  scope: string;
}

export interface TokenSet {
  accessToken: string;
  /** Absent if the IdP didn't issue one — most commonly because `scope`
   *  didn't include offline_access. */
  refreshToken?: string;
  /** Epoch ms. */
  expiresAt: number;
}

export interface DiscoveryDocument {
  authorization_endpoint: string;
  token_endpoint: string;
}

/** A token-endpoint request was rejected by the IdP — `code` is the OAuth
 *  `error` value (e.g. "invalid_grant", "invalid_scope"). */
export class TokenEndpointRequestError extends Error {
  constructor(
    public readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = 'TokenEndpointRequestError';
  }
}

/** Specifically: a refresh_token exchange was rejected (revoked, expired,
 *  or invalidated by e.g. a password reset) — distinct from
 *  TokenEndpointRequestError so callers know a fresh interactive sign-in
 *  is required, not just a retry. */
export class RefreshFailedError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'RefreshFailedError';
  }
}

/** RFC 7636 requires 43-128 chars from the unreserved [A-Za-z0-9-._~]
 *  alphabet; 32 random bytes base64url-encoded (no padding) is 43 chars —
 *  right at the minimum, safely within the allowed set. */
export function generateCodeVerifier(): string {
  return randomBytes(32).toString('base64url');
}

export function codeChallengeFromVerifier(verifier: string): string {
  return createHash('sha256').update(verifier).digest('base64url');
}

export function generateState(): string {
  return randomUUID();
}

/** Same trailing-slash handling as hupiClient.ts's resolveBaseUrl. */
export function discoveryUrl(issuerUrl: string): string {
  return `${issuerUrl.replace(/\/+$/, '')}/.well-known/openid-configuration`;
}

export async function fetchDiscoveryDocument(issuerUrl: string): Promise<DiscoveryDocument> {
  const res = await fetch(discoveryUrl(issuerUrl));
  if (!res.ok) {
    throw new Error(`OIDC discovery failed (${res.status}) for ${issuerUrl}`);
  }
  const body = (await res.json()) as Partial<DiscoveryDocument>;
  if (!body.authorization_endpoint || !body.token_endpoint) {
    throw new Error(
      `OIDC discovery document from ${issuerUrl} is missing authorization_endpoint/token_endpoint`,
    );
  }
  return { authorization_endpoint: body.authorization_endpoint, token_endpoint: body.token_endpoint };
}

export function buildAuthorizationUrl(
  discovery: DiscoveryDocument,
  opts: { clientAppId: string; scope: string; redirectUri: string; state: string; codeChallenge: string },
): string {
  const url = new URL(discovery.authorization_endpoint);
  url.searchParams.set('response_type', 'code');
  url.searchParams.set('client_id', opts.clientAppId);
  url.searchParams.set('redirect_uri', opts.redirectUri);
  url.searchParams.set('scope', opts.scope);
  url.searchParams.set('state', opts.state);
  url.searchParams.set('code_challenge', opts.codeChallenge);
  url.searchParams.set('code_challenge_method', 'S256');
  return url.toString();
}

interface TokenEndpointSuccessBody {
  access_token: string;
  refresh_token?: string;
  expires_in: number;
}
interface TokenEndpointErrorBody {
  error: string;
  error_description?: string;
}

async function postToken(tokenEndpoint: string, params: Record<string, string>): Promise<TokenSet> {
  const res = await fetch(tokenEndpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams(params).toString(),
  });
  const body = (await res.json().catch(() => ({}))) as Partial<
    TokenEndpointSuccessBody & TokenEndpointErrorBody
  >;
  if (!res.ok || !body.access_token) {
    const code = body.error ?? 'unknown_error';
    throw new TokenEndpointRequestError(code, body.error_description || code);
  }
  return {
    accessToken: body.access_token,
    refreshToken: body.refresh_token,
    // expires_in is optional per RFC 6749 (though every provider we've
    // tested against, Azure AD included, always sends it) — treat a
    // missing value as already-expired rather than guessing a lifetime,
    // so the next getValidAccessToken call refreshes it promptly instead
    // of trusting an unknown expiry.
    expiresAt: Date.now() + (body.expires_in ?? 0) * 1000,
  };
}

export function exchangeCodeForToken(
  tokenEndpoint: string,
  opts: { clientAppId: string; code: string; redirectUri: string; codeVerifier: string },
): Promise<TokenSet> {
  return postToken(tokenEndpoint, {
    grant_type: 'authorization_code',
    client_id: opts.clientAppId,
    code: opts.code,
    redirect_uri: opts.redirectUri,
    code_verifier: opts.codeVerifier,
  });
}

/** Throws RefreshFailedError specifically on `invalid_grant` (revoked/dead
 *  refresh token) so getValidAccessToken can tell "sign in again" apart
 *  from a transient network/server error, which propagates as-is. */
export async function refreshAccessToken(
  tokenEndpoint: string,
  opts: { clientAppId: string; refreshToken: string },
): Promise<TokenSet> {
  try {
    return await postToken(tokenEndpoint, {
      grant_type: 'refresh_token',
      client_id: opts.clientAppId,
      refresh_token: opts.refreshToken,
    });
  } catch (err) {
    if (err instanceof TokenEndpointRequestError && err.code === 'invalid_grant') {
      throw new RefreshFailedError(err.message);
    }
    throw err;
  }
}

/** Default 2-minute skew so a token doesn't expire between this check and
 *  the request actually reaching the gateway. */
const DEFAULT_SKEW_MS = 2 * 60 * 1000;

export function isExpiringSoon(expiresAt: number, now: number, skewMs: number = DEFAULT_SKEW_MS): boolean {
  return expiresAt - now <= skewMs;
}

export function settingsMatch(a: OidcSettings, b: OidcSettings): boolean {
  return a.issuerUrl === b.issuerUrl && a.clientAppId === b.clientAppId && a.scope === b.scope;
}
