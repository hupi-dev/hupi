import * as path from 'path';
import * as vscode from 'vscode';
import { createClient, streamChat, type ChatAttachment, type ChatMessage, type Citation } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { currentFileContext } from './chatViewProvider';

// Mirrors internal/gateway/attachments.go's maxAttachmentBytes exactly —
// rejecting an oversized file here, with a friendly message, is cheaper
// than letting the server reject the whole request with a 400 after a
// real network round trip.
const MAX_ATTACHMENT_BYTES = 8 * 1024 * 1024;

// Extensions routed as `type: "image"` (captioned by a vision-capable
// provider server-side, internal/gateway/attachments.go's
// describeImageAttachment) — mirrors
// internal/gateway/attachments.go's allowedImageMIME. Anything else
// readable as a file is sent as `type: "document"` and left to
// internal/ingest's own format sniffing/graceful-degradation (a
// genuinely unsupported binary surfaces as a warning, not a hard
// failure) — no extension allowlist needed on this side for documents.
const IMAGE_MIME_BY_EXTENSION: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.webp': 'image/webp',
  '.gif': 'image/gif',
};

// VS Code's native Chat view lets a user attach a file (drag-and-drop,
// the attach-file picker, or a "#file:" mention) — these show up in
// ChatRequest.references, not in the prompt text itself. This resolves
// the subset of references that point at a real file on disk into
// HUPI's attachments wire shape; references VS Code uses for other
// things (a code selection, a workspace symbol, a bare string) are not
// file Uris and are silently left alone — they're not this extension's
// concern, and the participant's own prompt-text handling above already
// covers them where relevant.
function referenceFileUri(value: unknown): vscode.Uri | undefined {
  if (value instanceof vscode.Uri) {
    return value;
  }
  if (value && typeof value === 'object' && 'uri' in (value as Record<string, unknown>)) {
    const maybeUri = (value as { uri: unknown }).uri;
    if (maybeUri instanceof vscode.Uri) {
      return maybeUri;
    }
  }
  return undefined;
}

interface ResolvedAttachments {
  attachments: ChatAttachment[];
  /** Filenames skipped for exceeding MAX_ATTACHMENT_BYTES — surfaced to
   *  the user so a silently-dropped attachment isn't mysterious. */
  tooLarge: string[];
}

async function resolveAttachments(references: readonly vscode.ChatPromptReference[]): Promise<ResolvedAttachments> {
  const attachments: ChatAttachment[] = [];
  const tooLarge: string[] = [];
  for (const ref of references) {
    const uri = referenceFileUri(ref.value);
    if (!uri) {
      continue;
    }
    let bytes: Uint8Array;
    try {
      bytes = await vscode.workspace.fs.readFile(uri);
    } catch {
      // Not a real readable file (e.g. a virtual/untitled document, or
      // a reference whose Uri scheme isn't a filesystem) — not this
      // resolver's concern, skip quietly.
      continue;
    }
    const filename = path.basename(uri.fsPath);
    if (bytes.byteLength > MAX_ATTACHMENT_BYTES) {
      tooLarge.push(filename);
      continue;
    }
    const imageMime = IMAGE_MIME_BY_EXTENSION[path.extname(uri.fsPath).toLowerCase()];
    attachments.push({
      type: imageMime ? 'image' : 'document',
      filename,
      content_type: imageMime,
      data: Buffer.from(bytes).toString('base64'),
    });
  }
  return { attachments, tooLarge };
}

// Renders citations as plain markdown for VS Code's native chat view.
// chatViewProvider.ts's own citations rendering lives in
// src/webview/chat.ts instead (renderCitationsHtml) — that surface posts
// the raw Citation[] across the extension-host/webview boundary and
// builds HTML there, since the webview's own marked.parse call would
// otherwise re-interpret this function's markdown output a second time.
function citationsMarkdown(citations: Citation[]): string {
  if (citations.length === 0) {
    return '';
  }
  const lines = citations.map((c) => {
    const used = c.used === true ? ' ✓ used' : c.used === false ? ' (not relied on)' : '';
    const snippet = c.snippet.length > 200 ? c.snippet.slice(0, 200) + '…' : c.snippet;
    return `- **${c.ref.kind}**${used}: ${snippet.replace(/\n/g, ' ')}`;
  });
  return `\n\n---\n**Sources**\n${lines.join('\n')}`;
}

// Registers HUPI as a participant in VS Code's own native Chat view
// (invoked as "@hupi", same mechanism GitHub Copilot Chat's own
// participants use) — a second front door onto the exact same backend
// the dedicated HUPI sidebar (chatViewProvider.ts) already uses, not a
// parallel reimplementation. This doesn't replace that sidebar: a
// third-party participant can't be made the *default*, unqualified
// handler in VS Code's Chat view (that's reserved for the host's own
// default participant — there's no `isDefault` a third party can set),
// so an always-visible, zero-prefix sidebar and an "@hupi" participant
// genuinely serve different moments rather than one obsoleting the
// other. Both call the same loadConfig/createClient/streamChat.
//
// Conversation history and cancellation both come from the platform
// here instead of being hand-rolled: context.history replaces
// chatViewProvider's own `history` array, and `token` replaces its own
// AbortController-based supersede logic — VS Code already serializes
// requests to a participant and gives a real cancellation token when a
// user starts a new turn or cancels, so there's nothing left for this
// module to track itself.
function historyToMessages(context: vscode.ChatContext): ChatMessage[] {
  const messages: ChatMessage[] = [];
  for (const turn of context.history) {
    if (turn instanceof vscode.ChatRequestTurn) {
      messages.push({ role: 'user', content: turn.prompt });
      continue;
    }
    if (turn instanceof vscode.ChatResponseTurn) {
      const text = turn.response
        .filter((part): part is vscode.ChatResponseMarkdownPart => part instanceof vscode.ChatResponseMarkdownPart)
        .map((part) => part.value.value)
        .join('');
      if (text) {
        messages.push({ role: 'assistant', content: text });
      }
    }
  }
  return messages;
}

async function handleChatRequest(
  context: vscode.ExtensionContext,
  request: vscode.ChatRequest,
  chatContext: vscode.ChatContext,
  stream: vscode.ChatResponseStream,
  token: vscode.CancellationToken,
): Promise<void> {
  const trimmed = request.prompt.trim();
  if (trimmed === '') {
    return;
  }

  const settings = vscode.workspace.getConfiguration('hupi');
  const fileContextEnabled = settings.get<boolean>('fileContext.enabled', true);
  const fileContext = fileContextEnabled ? currentFileContext() : undefined;
  const userContent = fileContext ? `${fileContext.text}\n\n${trimmed}` : trimmed;
  const messages = historyToMessages(chatContext);
  messages.push({ role: 'user', content: userContent });
  if (fileContext) {
    // VS Code's own chat UI renders this as a visible reference chip on
    // the response, the native way to show "this file informed the
    // answer" (review finding A13 — this used to be sent with no UI
    // indication at all).
    stream.reference(fileContext.uri);
  }

  // Files/images the user explicitly attached to this message (drag-
  // and-drop, the attach-file picker, or a "#file:" mention) — distinct
  // from fileContext above, which is the currently-open editor's
  // content, sent inline as plain text rather than as an attachment.
  const { attachments, tooLarge } = await resolveAttachments(request.references);
  if (tooLarge.length > 0) {
    stream.markdown(`\n\n_Skipped ${tooLarge.map((f) => `\`${f}\``).join(', ')} — exceeds the attachment size limit._`);
  }

  let cfg;
  try {
    cfg = await loadConfig(context);
  } catch (err) {
    if (err instanceof OidcSignInRequiredError) {
      stream.markdown(err.message);
      promptSignInRequired();
      return;
    }
    stream.markdown(`Config error: ${(err as Error).message}`);
    return;
  }

  const controller = new AbortController();
  token.onCancellationRequested(() => controller.abort());

  const citationsEnabled = settings.get<boolean>('citations.enabled', true);
  const deepCitations = settings.get<boolean>('citations.deep', false);

  const client = createClient(cfg);
  try {
    await streamChat(client, {
      model: cfg.model,
      messages,
      signal: controller.signal,
      onDelta: (delta) => stream.markdown(delta),
      explain: citationsEnabled ? (deepCitations ? 'deep' : 'on') : undefined,
      onCitations: (citations) => stream.markdown(citationsMarkdown(citations)),
      attachments: attachments.length > 0 ? attachments : undefined,
      onAttachmentWarnings: (warnings) =>
        stream.markdown(`\n\n_${warnings.join('; ')}_`),
    });
  } catch (err) {
    if (controller.signal.aborted) {
      return;
    }
    stream.markdown(
      `HUPI request failed: ${(err as Error).message}. Check hupi.baseUrl and your API key (HUPI: Set API Key) or sign-in (HUPI: Sign In).`,
    );
  }
}

export function registerChatParticipant(context: vscode.ExtensionContext): vscode.Disposable {
  const participant = vscode.chat.createChatParticipant('hupi.chat', (request, chatContext, stream, token) =>
    handleChatRequest(context, request, chatContext, stream, token),
  );
  participant.iconPath = vscode.Uri.joinPath(context.extensionUri, 'resources', 'activitybar-icon.png');
  return participant;
}
