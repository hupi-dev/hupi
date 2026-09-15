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
const input = document.getElementById('input') as HTMLTextAreaElement;
const sendButton = document.getElementById('send') as HTMLButtonElement;

let currentAssistantBubble: HTMLDivElement | null = null;
let currentAssistantRaw = '';

function appendMessage(text: string, cls: string): HTMLDivElement {
  const div = document.createElement('div');
  div.className = `msg ${cls}`;
  div.textContent = text;
  log.appendChild(div);
  log.scrollTop = log.scrollHeight;
  return div;
}

function send(): void {
  const text = input.value;
  if (text.trim() === '') {
    return;
  }
  appendMessage(text, 'user');
  input.value = '';
  currentAssistantBubble = appendMessage('', 'assistant');
  currentAssistantRaw = '';
  vscode.postMessage({ type: 'send', text });
}

sendButton.addEventListener('click', send);
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
      if (currentAssistantBubble) {
        // marked.parse is synchronous for the default (non-async-extension)
        // config used here — re-rendering the whole accumulated text on
        // every delta is simple and plenty fast for chat-length responses.
        currentAssistantBubble.innerHTML = marked.parse(currentAssistantRaw) as string;
        log.scrollTop = log.scrollHeight;
      }
      break;
    }
    case 'done': {
      currentAssistantBubble = null;
      currentAssistantRaw = '';
      break;
    }
    case 'error': {
      if (currentAssistantBubble && currentAssistantRaw === '') {
        // No partial answer arrived — replace the empty placeholder bubble
        // with the error instead of leaving a blank one behind.
        currentAssistantBubble.remove();
      }
      appendMessage(message.message, 'error');
      currentAssistantBubble = null;
      currentAssistantRaw = '';
      break;
    }
  }
});
