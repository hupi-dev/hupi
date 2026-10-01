// Runs inside the webview's iframe (a plain browser context) — this file
// must never import `vscode` (it doesn't exist here); all extension-host
// communication goes through postMessage/onDidReceiveMessage. Bundled
// separately from the extension host by esbuild.mjs's webviewConfig.
import { marked } from 'marked';

declare function acquireVsCodeApi(): {
  postMessage: (message: unknown) => void;
};

interface CitationRef {
  kind: string;
  scope: { kind: string; owner: string };
  id: string;
}

interface Citation {
  ref: CitationRef;
  snippet: string;
  used?: boolean;
}

type ToWebview =
  | { type: 'userEcho'; text: string }
  | { type: 'fileContext'; relativePath: string }
  | { type: 'delta'; text: string }
  | { type: 'citations'; items: Citation[] }
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
// Held separately from currentAssistantRaw rather than folded into the
// streamed markdown text: the 'done' handler's final render replaces
// currentAssistantContent's whole innerHTML from currentAssistantRaw, so
// anything appended earlier (e.g. during a 'citations' message, which
// always arrives before 'done' — see hupiClient.ts's streamChat, whose
// onCitations only ever fires on the terminal chunk) would just get
// wiped out again by that re-render if it lived in the same string.
let currentCitations: Citation[] = [];

function escapeHtml(s: string): string {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function renderCitationsHtml(citations: Citation[]): string {
  if (citations.length === 0) {
    return '';
  }
  const items = citations
    .map((c) => {
      const used = c.used === true ? ' <span class="used">✓ used</span>' : c.used === false ? ' <span class="unused">(not relied on)</span>' : '';
      const snippet = c.snippet.length > 200 ? c.snippet.slice(0, 200) + '…' : c.snippet;
      return `<li><strong>${escapeHtml(c.ref.kind)}</strong>${used}: ${escapeHtml(snippet)}</li>`;
    })
    .join('');
  return `<div class="citations"><div class="citationsTitle">Sources</div><ul>${items}</ul></div>`;
}

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
  currentCitations = [];
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
    case 'fileContext': {
      // Surfaces, on the just-sent user bubble, that this turn also
      // attached the active file/selection as context (review finding
      // A13 — this used to be sent with no UI indication at all).
      const lastUserMsg = log.querySelectorAll('.msg.user');
      const target = lastUserMsg[lastUserMsg.length - 1]?.querySelector('.content');
      if (target) {
        target.insertAdjacentHTML(
          'beforeend',
          `<div class="fileContextBadge">📎 includes code context from ${escapeHtml(message.relativePath)}</div>`,
        );
      }
      break;
    }
    case 'delta': {
      currentAssistantRaw += message.text;
      scheduleRender();
      break;
    }
    case 'citations': {
      currentCitations = message.items;
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
        currentAssistantContent.insertAdjacentHTML('beforeend', renderCitationsHtml(currentCitations));
        log.scrollTop = log.scrollHeight;
      }
      currentAssistantContent = null;
      currentAssistantRaw = '';
      currentCitations = [];
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
      currentCitations = [];
      setStreaming(false);
      break;
    }
  }
});
