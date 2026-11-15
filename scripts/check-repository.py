#!/usr/bin/env python3
"""Check staged paths/content without reading or printing runtime credentials."""

import argparse
import posixpath
import re
import subprocess
import sys
from pathlib import PurePosixPath
from urllib.parse import unquote, urlsplit


ROOT_FILES = {
    '.gitignore', '.gitattributes', 'go.mod', 'go.sum',
    'README.md', 'AGENTS.md', 'CHANGELOG.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt',
}
SCRIPTS = {'build.sh', 'package.py', 'check-repository.py', 'Start-LiteAgent.bat', 'build-vscode-bridge.py', 'test-vscode-bridge.py'}
PRIVATE_PARTS = {
    'data', 'dist', '.qa', '.hallmark', '.codex', '.claude', '.env',
    'browser-profile', 'downloads', 'drivers', 'state.json', 'state.lock',
}
SECRET_RULES = {
    'provider credential': rb'\bsk-(?:proj-|ant-(?:api\d+-)?)?[A-Za-z0-9_-]{24,}',
    'GitHub credential': rb'\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,})',
    'private key': rb'-----BEGIN (?:[A-Z]+ )?PRIVATE KEY-----',
    'AWS access key': rb'\b(?:AKIA|ASIA)[A-Z0-9]{16}\b',
    'Slack credential': rb'\bxox[baprs]-[A-Za-z0-9-]{25,}',
}


def allowed_path(name):
    path = PurePosixPath(name)
    if any(part in PRIVATE_PARTS or part.startswith('.env.') for part in path.parts):
        return False
    if name in ROOT_FILES:
        return True
    if path.parent in (PurePosixPath('cmd/liteagent'), PurePosixPath('internal/app')):
        return path.suffix == '.go'
    if path.parent == PurePosixPath('web'):
        return path.suffix in {'.go', '.js', '.css', '.html', '.txt'}
    if path.parent == PurePosixPath('scripts'):
        return path.name in SCRIPTS
    if path.parent == PurePosixPath('extensions/vscode-bridge'):
        return path.name in {'package.json', 'README.md', 'extension.js', 'bridge.js', 'protocol.js', 'bridge.test.js', 'test-host.js', 'test-fixture.js'}
    if path.parts[0] == 'docs':
        return path.suffix == '.md' or path.parent == PurePosixPath('docs/screenshots') and path.suffix == '.png'
    return False


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--staged', action='store_true', help='Inspect the Git index (the default).')
    parser.parse_args()
    try:
        index = subprocess.check_output(['git', 'ls-files', '--stage', '-z'], stderr=subprocess.DEVNULL)
    except subprocess.CalledProcessError:
        print('Run this check inside the repository after staging intended files.', file=sys.stderr)
        return 1
    files = {}
    findings = []
    for record in index.split(b'\0'):
        if not record:
            continue
        metadata, raw_name = record.split(b'\t', 1)
        mode, oid, stage = metadata.split()
        name = raw_name.decode('utf-8')
        if stage != b'0' or mode not in {b'100644', b'100755'}:
            findings.append((name, 'unmerged entry, symlink or unsupported Git mode'))
            continue
        if not allowed_path(name):
            findings.append((name, 'outside the source/documentation allowlist'))
            continue
        data = subprocess.check_output(['git', 'cat-file', 'blob', oid.decode('ascii')])
        files[name] = data
        if len(data) > 4 * 1024 * 1024:
            findings.append((name, 'file exceeds the repository size limit'))
        if name.endswith('.png'):
            if not data.startswith(b'\x89PNG\r\n\x1a\n'):
                findings.append((name, 'invalid screenshot file'))
            continue
        try:
            data.decode('utf-8')
        except UnicodeDecodeError:
            findings.append((name, 'unexpected binary file'))
        for rule, pattern in SECRET_RULES.items():
            if re.search(pattern, data):
                findings.append((name, rule))
    if not files:
        findings.append(('Git index', 'no source files staged'))
    for name, data in files.items():
        if not name.endswith('.md'):
            continue
        for target in re.findall(r'!?\[[^\]]*\]\(([^\s)]+)', data.decode('utf-8')):
            link = urlsplit(target.strip('<>'))
            if link.scheme or link.netloc or not link.path:
                continue
            resolved = posixpath.normpath(posixpath.join(posixpath.dirname(name), unquote(link.path)))
            if resolved not in files:
                findings.append((name, 'local link missing from index: ' + resolved))
    if findings:
        for name, rule in findings:
            print(f'{name}: {rule}', file=sys.stderr)
        return 1
    print(f'Checked {len(files)} staged files: paths, common credential patterns and local Markdown links passed.')
    print('Screenshots still require visual review; this is not an exhaustive secret detector.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
