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

// Streaming deltas can arrive faster than every animation frame (bursts
// after a network hiccup, or just a fast model) — re-parsing the whole
// accumulated markdown and replacing innerHTML on every single delta was
// doing that synchronous work far more often than the screen can even
// show it. Coalesce to at most one render per frame: whichever delta
// handler runs first in a frame schedules the render, later ones in the
// same frame just update currentAssistantRaw and let the scheduled one
// pick up the latest text.
let renderScheduled = false;
function scheduleRender(): void {
  if (renderScheduled || !currentAssistantContent) {
    return;
  }
  renderScheduled = true;
  requestAnimationFrame(() => {
    renderScheduled = false;
    if (currentAssistantContent) {
      currentAssistantContent.innerHTML = marked.parse(currentAssistantRaw) as string;
      log.scrollTop = log.scrollHeight;
    }
  });
}

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

// Guards against overlapping sends corrupting currentAssistantContent (see
// chatViewProvider.ts's inFlight AbortController comment for the full
// story) — this is the first line of defense: with the input disabled,
// there's no click/Enter for the user to trigger a second send with while
// one is already streaming, so the host-side abort-on-new-send logic is
// belt-and-suspenders rather than the only thing preventing the race.
function setStreaming(streaming: boolean): void {
  input.disabled = streaming;
  sendButton.disabled = streaming;
  if (!streaming) {
    input.focus();
  }
}

function send(): void {
  const text = input.value;
  if (text.trim() === '' || sendButton.disabled) {
    return;
  }
  appendMessage(text, 'user');
  input.value = '';
  currentAssistantContent = appendMessage('', 'assistant');
  currentAssistantRaw = '';
  setStreaming(true);
  vscode.postMessage({ type: 'send', text });
}

function newChat(): void {
  log.querySelectorAll('.msg').forEach((el) => el.remove());
  empty.style.display = '';
  currentAssistantContent = null;
  currentAssistantRaw = '';
  setStreaming(false);
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
      scheduleRender();
      break;
    }
    case 'done': {
      // A render may still be scheduled for this frame with the final
      // text already in currentAssistantRaw — let it run rather than
      // clearing currentAssistantContent out from under it, then do one
      // last synchronous render to guarantee the final text is shown
      // even if no frame was pending.
      if (currentAssistantContent) {
        currentAssistantContent.innerHTML = marked.parse(currentAssistantRaw) as string;
        log.scrollTop = log.scrollHeight;
      }
      currentAssistantContent = null;
      currentAssistantRaw = '';
      setStreaming(false);
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
      setStreaming(false);
      break;
    }
  }
});
