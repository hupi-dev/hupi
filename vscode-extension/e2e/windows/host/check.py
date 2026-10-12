"""Checks one Windows e2e run's results: check.py OUT_DIR MODE EPISODES_BEFORE EPISODES_AFTER LATEST_TAGS MODEL_LOG

Prints PASS/FAIL lines and exits non-zero on any failure. OUT_DIR is the
guest's Z:\\e2e\\out-<app>-<mode> as seen from the host."""
import json, sys
from pathlib import Path

out, mode, before, after, tags, model_log = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5], sys.argv[6]
failures = []
def check(ok, what):
    print(('PASS ' if ok else 'FAIL ') + what)
    if not ok:
        failures.append(what)

r = json.loads(Path(out, 'result.json').read_text(encoding='utf-8-sig'))
config = Path(out, 'config-after.txt').read_text(encoding='utf-8-sig')
cards = [(c.get('name'), c.get('status')) for c in r.get('cards', [])]
check(not r.get('error') and not r.get('errors'), 'no driver or chat errors (%s)' % (r.get('error') or r.get('errors') or 'none'))

if mode == 'main':
    names = [n for n, _ in cards]
    check(names == ['read_file', 'edit_file', 'run_command', 'mcp_smoke_smoke_echo', 'mcp_smoke_smoke_write'], 'tool cards in order: %s' % names)
    check(all(s == 'done' for _, s in cards), 'every tool card done: %s' % [s for _, s in cards])
    check(len(r.get('approvals', [])) == 2, "HUPI's card asked twice (edit_file, run_command)")
    check(any('smoke_write' in d for d in r.get('dialogs', [])), "VS Code's own confirmation for the MCP tool that isn't read-only")
    answer = r.get('answer', '')
    check('E2E_OK export const answer = 42;' in answer, 'run_command ran in PowerShell, in the project folder, and returned its output')
    check('smoke:from-windows' in answer and 'wrote:confirmed' in answer, 'both MCP tools returned their results to the model')
    check('answer = 42;' in config, 'the approved edit is on disk')
    check(after == before + 1, 'exactly one episode stored for the turn (%d -> %d)' % (before, after))
    want = 'agent,tool:read_file,tool:edit_file,tool:run_command,tool:mcp_smoke_smoke_echo,tool:mcp_smoke_smoke_write'
    check(tags == want, 'episode tagged with the agent and each tool: %s' % tags)
else:
    check(cards == [('read_file', 'failed'), ('read_file', 'done'), ('edit_file', 'denied'), ('run_command', 'stopped')], 'cards: .env refused, absolute path read, edit denied, command stopped: %s' % cards)
    log = Path(model_log).read_text() if Path(model_log).exists() else ''
    check('commonly hold secrets (.env)' in log, ".env refused by the secret-file rule (seen by the model)")
    check('answer = 41;' in config, 'the denied edit is not on disk')
    check(r.get('stopped') is True, 'Stop was pressed while the command ran')
    check(after == before, 'a stopped turn stores nothing (%d -> %d)' % (before, after))

sys.exit(1 if failures else 0)
