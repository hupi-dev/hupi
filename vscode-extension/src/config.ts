import * as vscode from 'vscode';
import { getApiKey } from './secrets';
import type { HupiConfig } from './hupiClient';

export async function loadConfig(context: vscode.ExtensionContext): Promise<HupiConfig> {
  const cfg = vscode.workspace.getConfiguration('hupi');
  return {
    baseUrl: cfg.get<string>('baseUrl', 'http://localhost:8787'),
    model: cfg.get<string>('model', ''),
    teamId: cfg.get<string>('teamId', ''),
    apiKey: await getApiKey(context),
  };
}
