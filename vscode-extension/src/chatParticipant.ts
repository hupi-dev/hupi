import * as vscode from 'vscode';
import { createClient, streamChat, type ChatMessage } from './hupiClient';
import { loadConfig, OidcSignInRequiredError } from './config';
import { promptSignInRequired } from './oidcAuth';
import { currentFileContext } from './chatViewProvider';

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

  const fileContext = currentFileContext();
  const userContent = fileContext ? `${fileContext}\n\n${trimmed}` : trimmed;
  const messages = historyToMessages(chatContext);
  messages.push({ role: 'user', content: userContent });

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

  const client = createClient(cfg);
  try {
    await streamChat(client, {
      model: cfg.model,
      messages,
      signal: controller.signal,
      onDelta: (delta) => stream.markdown(delta),
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
