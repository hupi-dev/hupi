import * as vscode from 'vscode';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';

// Must match contributes.mcpServerDefinitionProviders[].id in package.json.
export const MCP_PROVIDER_ID = 'hupi.memory';
const SERVER_LABEL = 'HUPI memory';

// Settings and secrets whose change can alter the server's URL or
// credentials (secrets.ts owns the two secret keys).
const WATCHED_SETTINGS = ['hupi.mcp.enabled', 'hupi.baseUrl', 'hupi.teamId', 'hupi.oidc'];
const WATCHED_SECRETS = ['hupi.apiKey', 'hupi.oidcSession'];

/**
 * The gateway's MCP endpoint (docs/MCP.md): /mcp for private memory,
 * /v1/team/{id}/mcp for a team workspace — the same private/team split
 * hupiClient.ts's resolveBaseUrl uses for chat.
 */
export function mcpUrl(baseUrl: string, teamId?: string): string {
  const root = baseUrl.replace(/\/+$/, '');
  const team = teamId?.trim();
  return team ? `${root}/v1/team/${encodeURIComponent(team)}/mcp` : `${root}/mcp`;
}

/**
 * Offers HUPI's /mcp endpoint to VS Code (Copilot agent mode and any other
 * MCP-aware chat), so its memory tools are available without a
 * hand-written mcp.json. provideMcpServerDefinitions only reads settings;
 * credentials are attached in resolveMcpServerDefinition, which VS Code
 * calls when it starts the server and where a sign-in prompt is allowed.
 */
export class HupiMcpProvider implements vscode.McpServerDefinitionProvider<vscode.McpHttpServerDefinition> {
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChangeMcpServerDefinitions = this.changed.event;

  constructor(private readonly context: vscode.ExtensionContext) {}

  /** Re-announce the server after anything it depends on changes. */
  watch(): vscode.Disposable[] {
    return [
      this.changed,
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (WATCHED_SETTINGS.some((s) => e.affectsConfiguration(s))) {
          this.changed.fire();
        }
      }),
      this.context.secrets.onDidChange((e) => {
        if (WATCHED_SECRETS.includes(e.key)) {
          this.changed.fire();
        }
      }),
    ];
  }

  provideMcpServerDefinitions(): vscode.McpHttpServerDefinition[] {
    const cfg = vscode.workspace.getConfiguration('hupi');
    if (!cfg.get<boolean>('mcp.enabled', true)) {
      return [];
    }
    const url = mcpUrl(cfg.get<string>('baseUrl', 'http://localhost:8787'), cfg.get<string>('teamId', ''));
    return [new vscode.McpHttpServerDefinition(SERVER_LABEL, vscode.Uri.parse(url))];
  }

  async resolveMcpServerDefinition(server: vscode.McpHttpServerDefinition): Promise<vscode.McpHttpServerDefinition | undefined> {
    let apiKey: string;
    try {
      ({ apiKey } = await loadConfig(this.context));
    } catch (err) {
      if (err instanceof OidcSignInRequiredError) {
        promptSignInRequired();
        return undefined; // don't start; signing in fires onDidChange
      }
      throw err;
    }
    // Tier 1/2 gateways need no credential; otherwise the same API key or
    // OIDC access token the chat route uses.
    server.headers = apiKey ? { Authorization: `Bearer ${apiKey}` } : {};
    return server;
  }
}

/** Registers the provider when this VS Code build has the MCP API (1.101+). */
export function registerMcpProvider(context: vscode.ExtensionContext): vscode.Disposable[] {
  if (typeof vscode.lm?.registerMcpServerDefinitionProvider !== 'function') {
    return []; // e.g. an editor fork without the API
  }
  const provider = new HupiMcpProvider(context);
  return [...provider.watch(), vscode.lm.registerMcpServerDefinitionProvider(MCP_PROVIDER_ID, provider)];
}
