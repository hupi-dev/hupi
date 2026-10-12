# Scripted OpenAI-compatible chat model for the HUPI agent's Windows
# end-to-end test (see ../README.md). It streams tool calls the way real
# providers do (arguments in fragments). Main scenario: read_file ->
# edit_file -> run_command -> MCP smoke_echo (read-only) -> MCP smoke_write
# (needs VS Code's confirmation) -> a final answer quoting each result.
# A prompt containing SCENARIO2 runs the refusal/deny/stop scenario instead.
# Usage: scripted_model.py PORT LOGFILE (listens on 127.0.0.1 only).
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = open(sys.argv[2], 'a')

def plan(messages, tools):
    after_user = []
    for m in reversed(messages):
        if m.get('role') == 'user':
            break
        after_user.append(m)
    done = [m for m in reversed(after_user) if m.get('role') == 'tool']
    user = next((m.get('content') or '' for m in reversed(messages) if m.get('role') == 'user'), '')
    if isinstance(user, list):
        user = ' '.join(p.get('text', '') for p in user if isinstance(p, dict))
    if 'SCENARIO2' in user:
        # Refusals and cancel: a secret file, an absolute Windows path, an
        # edit the user denies, then a long command the user stops.
        absolute = user.split('ABS=', 1)[1].strip() if 'ABS=' in user else ''
        steps2 = [
            ('read_file', {'path': '.env'}),
            ('read_file', {'path': absolute}),
            ('edit_file', {'path': 'src/config.ts', 'old_text': 'export const answer = 41;', 'new_text': 'export const answer = 99;'}),
            ('run_command', {'command': 'Write-Output STARTED; Start-Sleep 120; Write-Output NOT_STOPPED', 'timeout_seconds': 300}),
        ]
        if len(done) < len(steps2):
            name, args = steps2[len(done)]
            return ('tool', 'call_%d' % len(done), name, args)
        return ('text', 'SCENARIO2 should have been stopped before this answer.')
    echo = next((t for t in tools if t.endswith('smoke_echo')), None)
    write = next((t for t in tools if t.endswith('smoke_write')), None)
    steps = [
        ('read_file', {'path': 'src/config.ts'}),
        ('edit_file', {'path': 'src/config.ts', 'old_text': 'export const answer = 41;', 'new_text': 'export const answer = 42;'}),
        ('run_command', {'command': 'Write-Output "E2E_OK $((Get-Content src\\config.ts) -match \'answer\')"'}),
    ]
    if echo:
        steps.append((echo, {'text': 'from-windows'}))
    if write:
        steps.append((write, {'text': 'confirmed'}))
    if len(done) < len(steps):
        name, args = steps[len(done)]
        return ('tool', 'call_%d' % len(done), name, args)
    def first(i, m):
        c = (m.get('content') or '').strip()
        if steps[i][0] == 'run_command':
            return ' / '.join(c.splitlines())[:200]
        return c.splitlines()[0][:100] if c else ''
    lines = ['%s: %s' % (steps[i][0], first(i, m)) for i, m in enumerate(done)]
    missing = [n for n, t in (('smoke_echo', echo), ('smoke_write', write)) if not t]
    text = 'Done. ' + ' | '.join(lines) + (' | MISSING MCP tools: ' + ','.join(missing) if missing else '')
    return ('text', text)

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        msgs = body.get('messages', [])
        tools = [t['function']['name'] for t in body.get('tools', [])]
        p = plan(msgs, tools)
        last_tool = next(((m.get('content') or '')[:160] for m in reversed(msgs) if m.get('role') == 'tool'), None)
        LOG.write(json.dumps({'roles': [m.get('role') for m in msgs], 'last_tool_result': last_tool, 'tools': tools, 'plan': list(p[:3]) if p[0] == 'tool' else ['text', p[1]]}) + '\n'); LOG.flush()
        self.send_response(200); self.send_header('Content-Type', 'text/event-stream'); self.end_headers()
        def chunk(delta, finish=None):
            d = {'choices': [{'index': 0, 'delta': delta, 'finish_reason': finish}]}
            self.wfile.write(('data: ' + json.dumps(d) + '\n\n').encode()); self.wfile.flush(); time.sleep(0.05)
        if p[0] == 'tool':
            _, cid, name, args = p
            a = json.dumps(args)
            chunk({'role': 'assistant', 'tool_calls': [{'index': 0, 'id': cid, 'type': 'function', 'function': {'name': name, 'arguments': ''}}]})
            for i in range(0, len(a), 12):
                chunk({'tool_calls': [{'index': 0, 'function': {'arguments': a[i:i+12]}}]})
            chunk({}, 'tool_calls')
        else:
            for w in p[1].split(' '):
                chunk({'content': w + ' '})
            chunk({}, 'stop')
        self.wfile.write(b'data: [DONE]\n\n'); self.wfile.flush()

ThreadingHTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
