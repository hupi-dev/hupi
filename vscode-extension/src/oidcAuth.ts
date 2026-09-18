// The vscode-dependent OIDC orchestration layer: reads hupi.oidc.* settings,
// runs the interactive Authorization Code + PKCE flow (browser + a loopback
// HTTP listener), and manages the stored session (SecretStorage, via
// secrets.ts). All the provider-agnostic protocol logic (PKCE, discovery,
// token requests) lives in oidc.ts, which stays free of any `vscode`
// import — this file must never be imported from src/webview/chat.ts,
// which is a separate browser-context esbuild bundle: node:http/node:crypto
// don't exist there.
import * as http from 'node:http';
import * as vscode from 'vscode';
import {
  buildAuthorizationUrl,
  codeChallengeFromVerifier,
  exchangeCodeForToken,
  fetchDiscoveryDocument,
  generateCodeVerifier,
  generateState,
  isExpiringSoon,
  RefreshFailedError,
  refreshAccessToken,
  settingsMatch,
  type OidcSettings,
  type TokenSet,
} from './oidc';
import { clearOidcSession, getOidcSession, storeOidcSession } from './secrets';

const SIGN_IN_TIMEOUT_MS = 5 * 60 * 1000;

export function readOidcSettings(): OidcSettings {
  const cfg = vscode.workspace.getConfiguration('hupi');
  return {
    issuerUrl: cfg.get<string>('oidc.issuerUrl', '').trim(),
    clientAppId: cfg.get<string>('oidc.clientAppId', '').trim(),
    scope: cfg.get<string>('oidc.scope', '').trim(),
  };
}

/**
 * Reads the stored session (if any), discards it if it was issued under
 * different hupi.oidc.* settings, refreshes it if expiring soon, and
 * returns a valid access token — or undefined if the caller needs to run
 * signIn() (no session, settings changed, or the refresh token is dead).
 * A transient refresh error (network/server) is NOT swallowed here — it
 * propagates so the caller shows a real error rather than silently
 * demanding a fresh interactive login for what might just be a blip.
 */
export async function getValidAccessToken(
  context: vscode.ExtensionContext,
  settings: OidcSettings,
): Promise<string | undefined> {
  const session = await getOidcSession(context);
  if (!session || !settingsMatch(session, settings)) {
    return undefined;
  }
  if (!isExpiringSoon(session.expiresAt, Date.now())) {
    return session.accessToken;
  }
  if (!session.refreshToken) {
    // No refresh token to fall back on — most commonly hupi.oidc.scope
    // didn't include offline_access. Nothing to do but ask for a fresh
    // interactive sign-in.
    await clearOidcSession(context);
    return undefined;
  }
  try {
    const discovery = await fetchDiscoveryDocument(settings.issuerUrl);
    const refreshed = await refreshAccessToken(discovery.token_endpoint, {
      clientAppId: settings.clientAppId,
      refreshToken: session.refreshToken,
    });
    await storeOidcSession(context, { ...settings, ...refreshed, obtainedAt: Date.now() });
    return refreshed.accessToken;
  } catch (err) {
    if (err instanceof RefreshFailedError) {
      await clearOidcSession(context);
      return undefined;
    }
    throw err;
  }
}

export async function signIn(context: vscode.ExtensionContext): Promise<void> {
  const settings = readOidcSettings();
  if (!settings.issuerUrl || !settings.clientAppId || !settings.scope) {
    vscode.window.showErrorMessage(
      'HUPI: set hupi.oidc.issuerUrl, hupi.oidc.clientAppId, and hupi.oidc.scope before signing in — see docs/OIDC.md.',
    );
    return;
  }

  try {
    await vscode.window.withProgress(
      { location: vscode.ProgressLocation.Notification, title: 'HUPI: signing in...', cancellable: true },
      async (_progress, cancelToken) => {
        const tokenSet = await runLoopbackAuth(settings, cancelToken);
        await storeOidcSession(context, { ...settings, ...tokenSet, obtainedAt: Date.now() });
      },
    );
    vscode.window.showInformationMessage('HUPI: signed in.');
  } catch (err) {
    if ((err as Error).message !== 'cancelled') {
      vscode.window.showErrorMessage(`HUPI sign-in failed: ${(err as Error).message}`);
    }
  }
}

export async function signOut(context: vscode.ExtensionContext): Promise<void> {
  const existing = await getOidcSession(context);
  await clearOidcSession(context);
  vscode.window.showInformationMessage(existing ? 'HUPI: signed out.' : 'HUPI: was not signed in.');
}

export function registerOidcCommands(context: vscode.ExtensionContext): vscode.Disposable[] {
  return [
    vscode.commands.registerCommand('hupi.signIn', () => signIn(context)),
    vscode.commands.registerCommand('hupi.signOut', () => signOut(context)),
  ];
}

/** Shown by config.ts's callers (chatViewProvider.ts, inlineEdit.ts) when
 *  loadConfig() throws OidcSignInRequiredError — one native prompt with a
 *  one-click action, defined here so the command-wiring lives in one place. */
export function promptSignInRequired(): void {
  void vscode.window.showErrorMessage('Sign in to HUPI to continue.', 'Sign In').then((choice) => {
    if (choice === 'Sign In') {
      void vscode.commands.executeCommand('hupi.signIn');
    }
  });
}

/**
 * Runs one Authorization Code + PKCE round trip: opens the system browser
 * at the IdP's login page and listens on a loopback port for the redirect.
 *
 * The extension always runs with extensionKind "ui" (package.json) — the
 * extension host, and therefore this loopback listener, is always on the
 * user's local/client machine, in every VS Code topology (local,
 * Remote-SSH, WSL, Codespaces). There's no remote-port-forwarding boundary
 * to cross here, unlike a naive port-based flow in a "workspace"-kind
 * extension would face.
 */
async function runLoopbackAuth(settings: OidcSettings, cancelToken: vscode.CancellationToken): Promise<TokenSet> {
  const discovery = await fetchDiscoveryDocument(settings.issuerUrl);
  const codeVerifier = generateCodeVerifier();
  const codeChallenge = codeChallengeFromVerifier(codeVerifier);
  const state = generateState();

  const server = http.createServer();
  const port = await listenOnEphemeralPort(server);
  const redirectUri = `http://127.0.0.1:${port}/callback`;

  try {
    const code = await waitForCallback(server, state, cancelToken, () => {
      const authUrl = buildAuthorizationUrl(discovery, {
        clientAppId: settings.clientAppId,
        scope: settings.scope,
        redirectUri,
        state,
        codeChallenge,
      });
      // If the user's browser can't reach this loopback listener at all
      // (unusual, but possible under restrictive local firewall/security
      // software), they'll see a "site can't be reached" tab and the
      // sign-in will simply time out after SIGN_IN_TIMEOUT_MS — no
      // separate manual-paste fallback in this version (see docs).
      return vscode.env.openExternal(vscode.Uri.parse(authUrl));
    });
    return await exchangeCodeForToken(discovery.token_endpoint, {
      clientAppId: settings.clientAppId,
      code,
      redirectUri,
      codeVerifier,
    });
  } finally {
    server.close();
  }
}

function listenOnEphemeralPort(server: http.Server): Promise<number> {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      if (address && typeof address === 'object') {
        resolve(address.port);
      } else {
        reject(new Error('failed to determine loopback listener port'));
      }
    });
  });
}

/**
 * Waits for exactly one /callback request carrying a matching `state`,
 * opening the browser (via onReady, called once the server is already
 * listening) at the same time. Settles exactly once, on whichever comes
 * first: a valid callback, an error/denial callback, a state mismatch,
 * timeout, cancellation, or the browser failing to open.
 */
function waitForCallback(
  server: http.Server,
  expectedState: string,
  cancelToken: vscode.CancellationToken,
  onReady: () => Thenable<boolean>,
): Promise<string> {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timeout = setTimeout(() => settle(() => reject(new Error('sign-in timed out'))), SIGN_IN_TIMEOUT_MS);
    const cancelSub = cancelToken.onCancellationRequested(() => settle(() => reject(new Error('cancelled'))));

    function settle(action: () => void): void {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timeout);
      cancelSub.dispose();
      action();
    }

    server.on('request', (req, res) => {
      const url = new URL(req.url ?? '/', 'http://127.0.0.1');
      if (url.pathname !== '/callback') {
        res.writeHead(404).end();
        return;
      }
      const errorParam = url.searchParams.get('error');
      const returnedState = url.searchParams.get('state');
      const code = url.searchParams.get('code');

      if (errorParam) {
        const description = url.searchParams.get('error_description') || errorParam;
        respond(res, `Sign-in failed: ${escapeHtml(description)}. You can close this tab.`);
        settle(() => reject(new Error(description)));
        return;
      }
      if (returnedState !== expectedState || !code) {
        respond(res, 'Sign-in failed: unexpected response. You can close this tab.');
        settle(() => reject(new Error('OIDC callback state mismatch or missing authorization code')));
        return;
      }
      respond(res, 'Signed in to HUPI — you can close this tab.');
      settle(() => resolve(code));
    });

    Promise.resolve(onReady()).then(
      (opened) => {
        if (opened === false) {
          settle(() => reject(new Error('could not open the system browser — see docs/VSCODE_EXTENSION.md')));
        }
      },
      (err) => settle(() => reject(err)),
    );
  });
}

function respond(res: http.ServerResponse, message: string): void {
  res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
  res.end(`<html><body style="font-family: sans-serif; padding: 2rem;">${message}</body></html>`);
}

function escapeHtml(s: string): string {
  const map: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' };
  return s.replace(/[&<>"]/g, (c) => map[c]);
}
