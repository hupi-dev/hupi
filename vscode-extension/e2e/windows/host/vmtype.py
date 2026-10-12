# Types a command into the Windows VM's Run dialog (Win+R) through QEMU's monitor.
import os, subprocess, sys, time
def send(lines):
    subprocess.run(['docker', 'exec', '-i', os.environ.get('HUPI_WIN_CONTAINER', 'hupi-win'), 'sh', '-c', 'nc -U -q1 /run/shm/monitor.sock >/dev/null'], input=('\n'.join(lines) + '\n').encode(), check=True)
special = {' ': 'spc', '-': 'minus', '\\': 'backslash', '.': 'dot', ':': 'shift-semicolon', '_': 'shift-minus', '/': 'slash', '=': 'equal', ',': 'comma', "'": 'apostrophe', '"': 'shift-apostrophe'}
def keys(text):
    out = []
    for ch in text:
        if ch in special: out.append('sendkey ' + special[ch])
        elif ch.isupper(): out.append('sendkey shift-' + ch.lower())
        else: out.append('sendkey ' + ch)
    return out
send(['sendkey meta_l-r']); time.sleep(2)
send(keys(sys.argv[1])); time.sleep(1)
send(['sendkey ret'])
