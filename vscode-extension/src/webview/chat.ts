// Runs inside the webview's iframe (a plain browser context) — this file
// must never import `vscode` (it doesn't exist here); all extension-host
// communication goes through postMessage/onDidReceiveMessage. Bundled
// separately from the extension host by esbuild.mjs's webviewConfig.
import { marked } from 'marked';

declare function acquireVsCodeApi(): {
  postMessage: (message: unknown) => void;
};

type ToWebview =
  | { type: 'userEcho'; text: string }
  | { type: 'delta'; text: string }
  | { type: 'done' }
  | { type: 'error'; message: string };

const vscode = acquireVsCodeApi();
const log = document.getElementById('log') as HTMLDivElement;
const empty = document.getElementById('empty') as HTMLDivElement;
const input = document.getElementById('input') as HTMLTextAreaElement;
const sendButton = document.getElementById('send') as HTMLButtonElement;
const newChatButton = document.getElementById('newChat') as HTMLButtonElement;

let currentAssistantContent: HTMLDivElement | null = null;
let currentAssistantRaw = '';

const ROLE_LABELS: Record<string, string> = { user: 'You', assistant: 'HUPI', error: 'Error' };

/** Appends a message card (role label + content area) and returns the
 *  content element, which callers update directly (e.g. re-rendering
 *  markdown on each streamed delta) without touching the role label. */
function appendMessage(text: string, cls: string): HTMLDivElement {
  empty.style.display = 'none';
  const div = document.createElement('div');
  div.className = `msg ${cls}`;
  const role = document.createElement('span');
  role.className = 'role';
  role.textContent = ROLE_LABELS[cls] ?? cls;
  const content = document.createElement('div');
  content.className = 'content';
  content.textContent = text;
  div.append(role, content);
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
  return content;
}

function send(): void {
  const text = input.value;
  if (text.trim() === '') {
    return;
  }
  appendMessage(text, 'user');
  input.value = '';
  currentAssistantContent = appendMessage('', 'assistant');
  currentAssistantRaw = '';
  vscode.postMessage({ type: 'send', text });
}

function newChat(): void {
  log.querySelectorAll('.msg').forEach((el) => el.remove());
  empty.style.display = '';
  currentAssistantContent = null;
  currentAssistantRaw = '';
  vscode.postMessage({ type: 'clear' });
}

sendButton.addEventListener('click', send);
newChatButton.addEventListener('click', newChat);
input.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' && !e.shiftKey) {
    e.preventDefault();
    send();
  }
});

window.addEventListener('message', (event: MessageEvent<ToWebview>) => {
  const message = event.data;
  switch (message.type) {
    case 'delta': {
      currentAssistantRaw += message.text;
      if (currentAssistantContent) {
        // marked.parse is synchronous for the default (non-async-extension)
        // config used here — re-rendering the whole accumulated text on
        // every delta is simple and plenty fast for chat-length responses.
        currentAssistantContent.innerHTML = marked.parse(currentAssistantRaw) as string;
        log.scrollTop = log.scrollHeight;
      }
      break;
    }
    case 'done': {
      currentAssistantContent = null;
      currentAssistantRaw = '';
      break;
    }
    case 'error': {
      if (currentAssistantContent && currentAssistantRaw === '') {
        // No partial answer arrived — replace the empty placeholder bubble
        // with the error instead of leaving a blank one behind.
        currentAssistantContent.closest('.msg')?.remove();
      }
      appendMessage(message.message, 'error');
      currentAssistantContent = null;
      currentAssistantRaw = '';
      break;
    }
  }
});
