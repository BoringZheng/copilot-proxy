"""Check the Git publication set, not ignored runtime data. Never print matches."""
import hashlib
import json
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(__file__).resolve().parents[1]
tracked = subprocess.check_output(['git', '-C', str(root), 'ls-files', '-z']).decode().split('\0')
patterns = {
    'github-token': re.compile(rb'\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b'),
    'private-key': re.compile(rb'-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----(?:\r?\n|\\n)[A-Za-z0-9+/=\\\r\n]{64,}'),
    'api-key': re.compile(rb'\bsk-[A-Za-z0-9_-]{32,}\b'),
    'aws-access-key': re.compile(rb'\b(?:AKIA|ASIA)[A-Z0-9]{16}\b'),
}
failures = []
for name in filter(None, tracked):
    path = pathlib.PurePosixPath(name)
    if (any(part in ('secrets', 'runtime', 'backups', '.git', 'node_modules', 'target') for part in path.parts)
            or path.name in ('.env', 'github_token', 'secrets.json', 'auth.json', 'id_rsa', 'id_ed25519')
            or path.suffix in ('.db', '.sqlite', '.pem', '.key', '.log')):
        failures.append((name, 'forbidden-publication-path'))
        continue
    content = (root / name).read_bytes()
    for rule, pattern in patterns.items():
        if pattern.search(content):
            failures.append((name, rule))

manifest = json.loads((root / 'source-provenance.json').read_text(encoding='utf-8'))
for name, expected in manifest['sha256'].items():
    path = root / name
    content = path.read_bytes() if path.is_file() else b''
    if b'\0' not in content:
        content = content.replace(b'\r\n', b'\n')
    if not path.is_file() or hashlib.sha256(content).hexdigest() != expected:
        failures.append((name, 'source-snapshot-changed'))

for path in ('.env', 'secrets/session_secret', 'secrets/crypto_secret', 'runtime/test.db', 'backups/test.tar'):
    ignored = subprocess.run(['git', '-C', str(root), 'check-ignore', '-q', '--', path], check=False)
    if ignored.returncode != 0:
        failures.append((path, 'runtime-path-not-ignored'))

if failures:
    for name, rule in failures:
        print(f'FAIL {rule}: {name}')
    sys.exit(1)
print(f'PASS: {len(list(filter(None, tracked)))} publication files; source hashes and runtime exclusions verified.')
print('Pattern scanning reduces risk but is not a guarantee against every possible secret format.')

