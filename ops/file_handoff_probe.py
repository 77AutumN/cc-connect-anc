"""Fictional model substitute, executed inside the real launcher by ordinary UID.

Only the disposable identity test passes this source to Python. No model,
provider, Feishu connection or runtime dependency is used.
"""
import hashlib
import io
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
import zipfile

step = json.load(sys.stdin)
uid_map = Path('/proc/self/uid_map').read_text().split()
assert uid_map == ['0', str(step['model_uid']), '1'], 'host model identity changed'
status = Path('/proc/self/status').read_text()
for capability in ('CapEff', 'CapPrm', 'CapBnd', 'CapAmb'):
    assert capability + ':\t0000000000000000' in status, 'model retained capabilities'
for path in step['hidden']:
    try:
        os.stat(path)
    except (FileNotFoundError, PermissionError):
        pass
    else:
        raise AssertionError('other work, ledger, snapshot, admin or config visible')


def tool(command, value):
    result = subprocess.run(['cc-connect-file', command], input=json.dumps(value),
                            text=True, capture_output=True, timeout=15)
    assert result.returncode == 0, result.stdout
    return json.loads(result.stdout)


work = tool('work-context', {})
originals = {}
for entry in work['inputs']:
    path = Path(entry['path'])
    data = path.read_bytes()
    assert hashlib.sha256(data).hexdigest() == entry['sha256']
    originals[entry['name']] = data
    try:
        with path.open('ab'):
            pass
    except OSError:
        pass
    else:
        raise AssertionError('input is writable')
source = originals[step['input']]
if work['artifacts']:
    latest = work['artifacts'][-1]
    source = (Path(work['output_dir']) / latest['path']).read_bytes()
    assert hashlib.sha256(source).hexdigest() == latest['sha256']
with zipfile.ZipFile(io.BytesIO(source)) as archive:
    parts = {name: archive.read(name).decode() for name in archive.namelist()}
part = 'word/document.xml' if step['extension'] == 'docx' else 'xl/worksheets/sheet1.xml'
assert step['from'] in parts[part]
parts[part] = parts[part].replace(step['from'], step['to'])
if step['reference']:
    with zipfile.ZipFile(io.BytesIO(originals[step['reference']])) as archive:
        assert 'Fictional approved venue: Cedar Hall' in archive.read('word/document.xml').decode()
    parts[part] = parts[part].replace('</w:body>', '<w:p><w:r><w:t>Fictional approved venue: Cedar Hall</w:t></w:r></w:p></w:body>')
body = io.BytesIO()
with zipfile.ZipFile(body, 'w', zipfile.ZIP_DEFLATED) as archive:
    for name, content in parts.items():
        archive.writestr(name, content)
relative = 'revisions/result-v%d.%s' % (work['latest_version'] + 1, step['extension'])
output = Path(work['output_dir']) / relative
output.parent.mkdir(mode=0o700, exist_ok=True)
output.write_bytes(body.getvalue())
output.chmod(0o600)
assert stat.S_IMODE(output.stat().st_mode) == 0o600
artifact = tool('file-deliver', {'work_id':work['work_id'], 'path':relative,
    'sha256':hashlib.sha256(body.getvalue()).hexdigest(), 'expected_version':work['latest_version']})
assert stat.S_IMODE(output.stat().st_mode) == 0o640
assert stat.S_IMODE(output.parent.stat().st_mode) == 0o2750
# The socket mount is the real restricted HTTP listener, never the daemon API.
with socket.socket(socket.AF_UNIX) as peer:
    peer.settimeout(3)
    peer.connect('/run/cc-connect/action-tools.sock')
    peer.sendall(b'POST /send HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
    assert b' 404 ' in peer.recv(4096).split(b'\r\n')[0]
print(json.dumps({'Work':work, 'Artifact':artifact, 'Checks':[
    'distinct host UID', 'zero model capabilities', 'other work/ledger/snapshot/admin hidden',
    'original read-only', '0600 output published as 0640', 'management route denied']}))
