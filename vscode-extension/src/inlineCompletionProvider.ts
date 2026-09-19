import * as vscode from 'vscode';
import { createClient, chat, type ChatMessage } from './hupiClient';
import { loadConfig } from './config';
import { stripCodeFences } from './textUtils';

// Bounds on how much surrounding code goes into the prompt — this is
// ghost text, not the chat sidebar's grounding context: it needs to be
// fast and cheap enough to fire on a typing pause, not exhaustive.
const MAX_PREFIX_CHARS = 4000;
const MAX_SUFFIX_CHARS = 1000;
const MAX_COMPLETION_TOKENS = 256;
const DEFAULT_DEBOUNCE_MS = 300;

const SYSTEM_PROMPT =
  'You are a code completion engine embedded in an editor. Given the code ' +
  'immediately before and after the cursor (marked <CURSOR>), respond with ' +
  'ONLY the text that should be inserted at the cursor to continue the code ' +
  'naturally — no repetition of the given code, no markdown code fences, no ' +
  'commentary, no explanation. Keep it short: usually a single line or a few ' +
  'lines, never a whole new file.';

/** Resolves once `ms` has elapsed or the token is cancelled, whichever
 *  comes first — this is the actual debounce: VS Code calls
 *  provideInlineCompletionItems on every keystroke, and without a delay
 *  every one of those would fire a real request to the gateway. */
function debounce(ms: number, token: vscode.CancellationToken): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(resolve, ms);
    token.onCancellationRequested(() => {
      clearTimeout(timer);
      resolve();
    });
  });
}

export class HupiInlineCompletionProvider implements vscode.InlineCompletionItemProvider {
  constructor(private readonly context: vscode.ExtensionContext) {}

  async provideInlineCompletionItems(
    document: vscode.TextDocument,
    position: vscode.Position,
    _completionContext: vscode.InlineCompletionContext,
    token: vscode.CancellationToken,
  ): Promise<vscode.InlineCompletionItem[] | undefined> {
    const settings = vscode.workspace.getConfiguration('hupi');
    // Opt-in: a suggestion on every typing pause means a real network round
    // trip to the gateway that often, which is a real latency/cost trade-off
    // the chat sidebar and inline edit (both explicitly invoked) don't have.
    if (!settings.get<boolean>('inlineSuggestions.enabled', false)) {
      return undefined;
    }

    await debounce(settings.get<number>('inlineSuggestions.debounceMs', DEFAULT_DEBOUNCE_MS), token);
    if (token.isCancellationRequested) {
      return undefined;
    }

    const offset = document.offsetAt(position);
    const fullText = document.getText();
    const prefix = fullText.slice(Math.max(0, offset - MAX_PREFIX_CHARS), offset);
    const suffix = fullText.slice(offset, offset + MAX_SUFFIX_CHARS);

    let cfg;
    try {
      cfg = await loadConfig(this.context);
    } catch {
      // Ghost text fails silently on config/sign-in errors rather than
      // popping a message on every typing pause — the sidebar, @hupi, and
      // inline edit all surface those clearly already when explicitly used.
      return undefined;
    }

    const controller = new AbortController();
    token.onCancellationRequested(() => controller.abort());

    const messages: ChatMessage[] = [
      { role: 'system', content: SYSTEM_PROMPT },
      {
        role: 'user',
        content: `Language: ${document.languageId}\n\nCode before cursor:\n${prefix}\n\n<CURSOR>\n\nCode after cursor:\n${suffix}`,
      },
    ];

    const client = createClient(cfg);
    let completion: string;
    try {
      completion = await chat(client, {
        model: cfg.model,
        messages,
        signal: controller.signal,
        maxTokens: MAX_COMPLETION_TOKENS,
        temperature: 0.2,
      });
    } catch {
      return undefined;
    }
    if (token.isCancellationRequested) {
      return undefined;
    }

    const cleaned = stripCodeFences(completion).replace(/^<CURSOR>\s*/, '');
    if (cleaned.trim() === '') {
      return undefined;
    }

    return [new vscode.InlineCompletionItem(cleaned, new vscode.Range(position, position))];
  }
}

export function registerInlineCompletions(context: vscode.ExtensionContext): vscode.Disposable {
  return vscode.languages.registerInlineCompletionItemProvider(
    { pattern: '**' },
    new HupiInlineCompletionProvider(context),
  );
}
