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

// Mirrors hupiClient.ts's ChatAttachment exactly — duplicated rather than
// imported for the same reason Citation/CitationRef are above: this file
// must never import anything that pulls in the `openai` package or
// `vscode`, since it's bundled separately for the webview's plain-browser
// context (esbuild.mjs's webviewConfig).
interface ChatAttachment {
  type: 'document' | 'image';
  filename?: string;
  content_type?: string;
  data: string;
}

type ToWebview =
  | { type: 'userEcho'; text: string }
  | { type: 'fileContext'; relativePath: string }
  | { type: 'delta'; text: string }
  | { type: 'citations'; items: Citation[] }
  | { type: 'attachmentWarnings'; warnings: string[] }
  | { type: 'done' }
  | { type: 'error'; message: string };

// Mirrors chatParticipant.ts's own MAX_ATTACHMENT_BYTES/IMAGE_MIME_BY_EXTENSION
// exactly — same reasoning: rejecting an oversized file here, client-side,
// with a friendly message, is cheaper than a real network round trip only
// for the server to reject it with a 400.
const MAX_ATTACHMENT_BYTES = 8 * 1024 * 1024;
const IMAGE_MIME_BY_EXTENSION: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.webp': 'image/webp',
  '.gif': 'image/gif',
};

function extensionOf(filename: string): string {
  const i = filename.lastIndexOf('.');
  return i === -1 ? '' : filename.slice(i).toLowerCase();
}

function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      // dataURL shape: "data:<mime>;base64,<data>" — only the part after
      // the comma is the base64 payload HUPI's wire format wants.
      const result = reader.result as string;
      resolve(result.slice(result.indexOf(',') + 1));
    };
    reader.onerror = () => reject(reader.error ?? new Error(`could not read ${file.name}`));
    reader.readAsDataURL(file);
  });
}

const vscode = acquireVsCodeApi();
const log = document.getElementById('log') as HTMLDivElement;
const empty = document.getElementById('empty') as HTMLDivElement;
const input = document.getElementById('input') as HTMLTextAreaElement;
const sendButton = document.getElementById('send') as HTMLButtonElement;
const newChatButton = document.getElementById('newChat') as HTMLButtonElement;
const attachButton = document.getElementById('attachBtn') as HTMLButtonElement;
const fileInput = document.getElementById('fileInput') as HTMLInputElement;
const attachmentChips = document.getElementById('attachmentChips') as HTMLDivElement;
const inputRow = document.getElementById('inputRow') as HTMLDivElement;

let currentAssistantContent: HTMLDivElement | null = null;
let currentAssistantRaw = '';
// Held separately from currentAssistantRaw rather than folded into the
// streamed markdown text: the 'done' handler's final render replaces
// currentAssistantContent's whole innerHTML from currentAssistantRaw, so
// anything appended earlier (e.g. during a 'citations'/'attachmentWarnings'
// message, which both always arrive before 'done' — see hupiClient.ts's
// streamChat, whose onCitations/onAttachmentWarnings only ever fire on the
// terminal chunk) would just get wiped out again by that re-render if it
// lived in the same string.
let currentCitations: Citation[] = [];
let currentAttachmentWarnings: string[] = [];

// Files the user has attached (attach button or drag-and-drop) for the
// *next* message, cleared once that message is sent. Each entry keeps the
// original filename separately from the wire-format ChatAttachment (which
// has filename as an optional field) purely so the chip UI/remove-by-index
// doesn't need to re-derive it.
let pendingAttachments: { filename: string; attachment: ChatAttachment }[] = [];

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

function renderAttachmentWarningsHtml(warnings: string[]): string {
  if (warnings.length === 0) {
    return '';
  }
  return `<div class="attachmentWarning">${escapeHtml(warnings.join('; '))}</div>`;
}

/** Re-renders the pending-attachment chip row above the input from
 *  pendingAttachments — called after every add/remove rather than
 *  incrementally patched, since the list is always small (server caps at
 *  5 per request) and this keeps add/remove/clear all going through one
 *  code path. */
function renderAttachmentChips(): void {
  attachmentChips.innerHTML = '';
  attachmentChips.style.display = pendingAttachments.length > 0 ? 'flex' : 'none';
  pendingAttachments.forEach((p, i) => {
    const chip = document.createElement('span');
    chip.className = 'attachmentChip';
    const label = document.createElement('span');
    label.textContent = (p.attachment.type === 'image' ? '🖼 ' : '📎 ') + p.filename;
    const remove = document.createElement('button');
    remove.type = 'button';
    remove.className = 'attachmentChipRemove';
    remove.textContent = '×';
    remove.title = `Remove ${p.filename}`;
    remove.addEventListener('click', () => {
      pendingAttachments.splice(i, 1);
      renderAttachmentChips();
    });
    chip.append(label, remove);
    attachmentChips.appendChild(chip);
  });
}

/** Transient, auto-dismissing note above the input — used for a file
 *  rejected client-side (too large) so it isn't a mysterious silent
 *  no-op, without needing a whole chat-log error bubble for something
 *  that isn't really a conversation turn. */
function showTransientNote(text: string): void {
  const note = document.createElement('div');
  note.className = 'transientNote';
  note.textContent = text;
  inputRow.insertAdjacentElement('beforebegin', note);
  setTimeout(() => note.remove(), 5000);
}

async function addFiles(files: FileList | File[]): Promise<void> {
  for (const file of Array.from(files)) {
    if (file.size > MAX_ATTACHMENT_BYTES) {
      showTransientNote(`Skipped ${file.name} — exceeds the 8 MB attachment size limit.`);
      continue;
    }
    const imageMime = IMAGE_MIME_BY_EXTENSION[extensionOf(file.name)];
    try {
      const data = await fileToBase64(file);
      pendingAttachments.push({
        filename: file.name,
        attachment: { type: imageMime ? 'image' : 'document', filename: file.name, content_type: imageMime, data },
      });
    } catch {
      showTransientNote(`Could not read ${file.name} — skipped.`);
    }
  }
  renderAttachmentChips();
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
  const attachments = pendingAttachments.map((p) => p.attachment);
  const userBubble = appendMessage(text, 'user');
  if (pendingAttachments.length > 0) {
    const names = pendingAttachments.map((p) => p.filename).join(', ');
    userBubble.insertAdjacentHTML('beforeend', `<div class="fileContextBadge">📎 attached: ${escapeHtml(names)}</div>`);
  }
  input.value = '';
  pendingAttachments = [];
  renderAttachmentChips();
  currentAssistantContent = appendMessage('', 'assistant');
  currentAssistantRaw = '';
  setStreaming(true);
  vscode.postMessage(attachments.length > 0 ? { type: 'send', text, attachments } : { type: 'send', text });
}

function newChat(): void {
  log.querySelectorAll('.msg').forEach((el) => el.remove());
  empty.style.display = '';
  currentAssistantContent = null;
  currentAssistantRaw = '';
  currentCitations = [];
  currentAttachmentWarnings = [];
  pendingAttachments = [];
  renderAttachmentChips();
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

attachButton.addEventListener('click', () => fileInput.click());
fileInput.addEventListener('change', () => {
  if (fileInput.files && fileInput.files.length > 0) {
    void addFiles(fileInput.files);
  }
  fileInput.value = '';
});

// Drag-and-drop straight onto the input row — standard HTML5 DnD, works
// the same inside a webview as any other browser context.
inputRow.addEventListener('dragover', (e) => {
  e.preventDefault();
  inputRow.classList.add('dragOver');
});
inputRow.addEventListener('dragleave', () => {
  inputRow.classList.remove('dragOver');
});
inputRow.addEventListener('drop', (e) => {
  e.preventDefault();
  inputRow.classList.remove('dragOver');
  if (e.dataTransfer?.files && e.dataTransfer.files.length > 0) {
    void addFiles(e.dataTransfer.files);
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
    case 'attachmentWarnings': {
      currentAttachmentWarnings = message.warnings;
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
        currentAssistantContent.insertAdjacentHTML('beforeend', renderAttachmentWarningsHtml(currentAttachmentWarnings));
        log.scrollTop = log.scrollHeight;
      }
      currentAssistantContent = null;
      currentAssistantRaw = '';
      currentCitations = [];
      currentAttachmentWarnings = [];
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
      currentAttachmentWarnings = [];
      setStreaming(false);
      break;
    }
  }
});
