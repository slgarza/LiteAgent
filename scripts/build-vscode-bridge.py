#!/usr/bin/env python3
"""Build a local VSIX using an explicit source list; never package runtime data."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
dist = root / 'dist'
dist.mkdir(exist_ok=True)
source = root / 'extensions' / 'vscode-bridge'
try:
    node_version = subprocess.check_output(['node', '-p', 'process.versions.node'], text=True).strip()
except (OSError, subprocess.CalledProcessError):
    raise SystemExit('Install Node.js 22 or newer with npm to package the extension.')
if int(node_version.split('.')[0]) < 22:
    raise SystemExit('VSIX packaging requires Node.js 22 or newer; the installed extension uses VS Code’s runtime.')
with tempfile.TemporaryDirectory(prefix='liteagent-vsix-') as temporary:
    directory = Path(temporary)
    for name in ['package.json', 'extension.js', 'bridge.js', 'protocol.js', 'README.md']:
        shutil.copyfile(source / name, directory / name)
    shutil.copyfile(root / 'LICENSE', directory / 'LICENSE')
    subprocess.run([os.environ.get('LITEAGENT_NPX_BIN', 'npx.cmd' if os.name == 'nt' else 'npx'),
                    '--yes', '@vscode/vsce@4.1.1', 'package', '--no-dependencies',
                    '--out', str(dist / 'LiteAgent-vscode-bridge.vsix')], cwd=directory, check=True)
