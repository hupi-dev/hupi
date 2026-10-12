// Drives HUPI's sidebar chat in a running VS Code / HUPI Code through the
// Chrome DevTools Protocol (the app is started with --remote-debugging-port),
// the way a user would: types the prompt, clicks Allow on HUPI's approval
// cards and Yes on VS Code's own dialogs, takes screenshots, and writes
// OUTDIR\result.json. MODE scenario2 clicks Deny on the edit and Stop while
// the command runs instead. See ../README.md.
// Run with the app's own binary (run.ps1 does):
//   ELECTRON_RUN_AS_NODE=1 Code.exe driver.js PORT OUTDIR PROMPT [main|scenario2]
const fs = require('fs');
const path = require('path');
const [PORT, OUT, PROMPT, MODE = 'main'] = process.argv.slice(2);
fs.mkdirSync(OUT, { recursive: true });
const logFile = path.join(OUT, 'drive.log');
const say = (...a) => fs.appendFileSync(logFile, `${new Date().toISOString()} ${a.map((x) => (typeof x === 'string' ? x : JSON.stringify(x))).join(' ')}\n`);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

class Target {
  constructor(info) {
    this.info = info;
    this.next = 1;
    this.pending = new Map();
    this.contexts = [];
  }
  open() {
    return new Promise((resolve, reject) => {
      this.ws = new WebSocket(this.info.webSocketDebuggerUrl);
      this.ws.onopen = () => resolve(this);
      this.ws.onerror = (e) => reject(new Error('ws error ' + (e.message || '')));
      this.ws.onmessage = (ev) => {
        const msg = JSON.parse(ev.data);
        if (msg.method === 'Runtime.executionContextCreated') this.contexts.push(msg.params.context.id);
        if (msg.id && this.pending.has(msg.id)) {
          this.pending.get(msg.id)(msg);
          this.pending.delete(msg.id);
        }
      };
    });
  }
  call(method, params = {}, timeout = 10000) {
    const id = this.next++;
    return new Promise((resolve) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        resolve(null);
      }, timeout);
      this.pending.set(id, (m) => {
        clearTimeout(timer);
        resolve(m);
      });
      this.ws.send(JSON.stringify({ id, method, params }));
    });
  }
  async eval(expression, contextId) {
    const p = { expression, returnByValue: true, awaitPromise: true };
    if (contextId !== undefined) p.contextId = contextId;
    const r = await this.call('Runtime.evaluate', p);
    return r && r.result && r.result.result ? r.result.result.value : undefined;
  }
  close() {
    try { this.ws.close(); } catch {}
  }
}

const PROBE = "!!(document.getElementById('input') && document.getElementById('stop') && document.getElementById('log'))";

async function targets() {
  const r = await fetch(`http://127.0.0.1:${PORT}/json/list`);
  return r.json();
}

async function findChat(deadline) {
  while (Date.now() < deadline) {
    let list = [];
    try { list = await targets(); } catch (e) { say('json/list', String(e)); await sleep(1000); continue; }
    for (const t of list) {
      if (!t.webSocketDebuggerUrl) continue;
      let tgt;
      try {
        tgt = await new Target(t).open();
        await tgt.call('Runtime.enable', {}, 3000);
        await sleep(300);
        for (const ctx of [undefined, ...tgt.contexts]) {
          if (await tgt.eval(PROBE, ctx)) {
            say('found chat in', t.type, (t.url || '').slice(0, 100), 'context', ctx);
            return { tgt, ctx };
          }
        }
        tgt.close();
      } catch (e) {
        say('target error', t.type, String(e));
        if (tgt) tgt.close();
      }
    }
    await sleep(2000);
  }
  return {};
}

async function findWorkbench() {
  for (const t of await targets()) {
    if (t.type === 'page' && /workbench/.test(t.url || '') && t.webSocketDebuggerUrl) {
      const tgt = await new Target(t).open();
      await tgt.call('Runtime.enable', {}, 3000);
      return tgt;
    }
  }
  return undefined;
}

let wb;
let shotN = 0;
async function shot(name) {
  if (!wb) return;
  const r = await wb.call('Page.captureScreenshot', { format: 'png' }, 15000);
  if (r && r.result && r.result.data) {
    const file = path.join(OUT, `${String(++shotN).padStart(2, '0')}-${name}.png`);
    fs.writeFileSync(file, Buffer.from(r.result.data, 'base64'));
    say('screenshot', path.basename(file));
  }
}

(async () => {
  const result = { prompt: PROMPT, approvals: [], dialogs: [] };
  const save = () => fs.writeFileSync(path.join(OUT, 'result.json'), JSON.stringify(result, null, 2));
  try {
    const { tgt, ctx } = await findChat(Date.now() + 180000);
    wb = await findWorkbench();
    if (!tgt) {
      result.error = 'chat webview not found';
      await shot('not-found');
      return;
    }
    // Let MCP discovery and the extension settle before the turn starts.
    await sleep(8000);
    // Stock VS Code's first-run onboarding (Copilot sign-in, then theme)
    // covers the window; click through it.
    for (let i = 0; wb && i < 5; i++) {
      const clicked = await wb.eval("(() => { const b = [...document.querySelectorAll('button, a, .monaco-button, [role=button]')].find((x) => /^(Continue without Signing In|Get Started|Skip for now)$/i.test((x.innerText || '').trim())); if (!b) return ''; b.click(); return b.innerText.trim(); })()");
      if (!clicked) break;
      say('onboarding: clicked', clicked);
      await sleep(1500);
    }
    await shot('open');
    await tgt.eval(`(() => { const i = document.getElementById('input'); i.value = ${JSON.stringify(PROMPT)}; document.getElementById('send').click(); return true; })()`, ctx);
    say('sent prompt');
    const end = Date.now() + 300000;
    let sawRunning = false;
    while (Date.now() < end) {
      const approve = await tgt.eval("(() => { const b = document.querySelector('.toolButton.approve:not([disabled])'); if (!b) return ''; const card = b.closest('.toolCard'); return card ? card.innerText.slice(0, 300) : 'approve'; })()", ctx);
      if (approve) {
        await sleep(1500);
        await shot('approval-' + (result.approvals.length + 1));
        result.approvals.push(approve);
        // SCENARIO2: deny the edit; allow the command, then press Stop while it runs.
        const deny = MODE === 'scenario2' && /edit_file/.test(approve);
        await tgt.eval(`document.querySelector('.toolButton.${deny ? 'deny' : 'approve'}:not([disabled])').click(), true`, ctx);
        say(deny ? 'clicked Deny:' : 'clicked Allow:', approve.split('\n')[0]);
        if (MODE === 'scenario2' && /run_command/.test(approve)) {
          await sleep(6000);
          await shot('command-running');
          await tgt.eval("document.getElementById('stop').click(), true", ctx);
          say('clicked Stop');
          result.stopped = true;
        }
        await sleep(1000);
        continue;
      }
      if (wb) {
        const dialog = await wb.eval("(() => { const d = document.querySelector('.monaco-dialog-box'); return d ? d.innerText.slice(0, 400) : ''; })()");
        if (dialog) {
          await sleep(1000);
          await shot('vscode-dialog-' + (result.dialogs.length + 1));
          result.dialogs.push(dialog);
          const clicked = await wb.eval("(() => { const bs = [...document.querySelectorAll('.monaco-dialog-box .dialog-buttons .monaco-button')]; const b = bs.find((x) => /^(Allow|Yes|Run|Continue|Trust|Start)/i.test(x.innerText.trim())) || bs[0]; if (!b) return ''; const t = b.innerText.trim(); b.click(); return t; })()");
          say('dialog:', dialog.split('\n')[0], '-> clicked', clicked);
          await sleep(1000);
          continue;
        }
      }
      const state = await tgt.eval("JSON.stringify({ stop: getComputedStyle(document.getElementById('stop')).display !== 'none', send: getComputedStyle(document.getElementById('send')).display !== 'none', assistant: document.querySelectorAll('.msg.assistant').length, error: document.querySelectorAll('.msg.error').length })", ctx);
      const s = state ? JSON.parse(state) : {};
      if (s.stop) sawRunning = true;
      if (sawRunning && !s.stop && (s.assistant > 0 || s.error > 0)) break;
      await sleep(500);
    }
    await sleep(1500);
    await shot('done');
    result.cards = JSON.parse((await tgt.eval("JSON.stringify([...document.querySelectorAll('.toolCard')].map(c => ({ name: (c.querySelector('.toolName') || {}).textContent, status: (c.querySelector('.toolStatus') || {}).textContent, detail: ((c.querySelector('.toolDetail') || {}).textContent || '').slice(0, 300) })))", ctx)) || '[]');
    result.answer = await tgt.eval("(() => { const a = [...document.querySelectorAll('.msg.assistant .content')]; return a.length ? a[a.length - 1].innerText : ''; })()", ctx);
    result.errors = await tgt.eval("[...document.querySelectorAll('.msg.error')].map(e => e.innerText)", ctx);
  } catch (e) {
    result.error = String(e && e.stack || e);
  } finally {
    save();
    say('done');
    process.exit(0);
  }
})();
