"""Package only distribution files, never local runtime data or credentials."""
import hashlib
import os
import shutil
import tempfile
from pathlib import Path
import zipfile

root = Path(__file__).resolve().parents[1]
dist = root / 'dist'
common = [(root / name, name) for name in ['README.md', 'AGENTS.md', 'CHANGELOG.md', 'LICENSE', 'THIRD_PARTY_NOTICES.txt']]
common += [(dist / 'GO-LICENSE.txt', 'GO-LICENSE.txt'), (root / 'web/LICENSES.txt', 'LICENSES.txt'), (root / 'web/LICENSES.txt', 'web/LICENSES.txt')]
# Keep the documentation boundary explicit; no runtime or design-cache globbing.
common += [(path, str(path.relative_to(root))) for path in sorted((root / 'docs').rglob('*'))
           if path.is_file() and path.suffix in {'.md', '.png'}]
for name, files in [
    ('LiteAgent-windows-amd64.zip', [dist / 'LiteAgent.exe', dist / 'Start-LiteAgent.bat']),
    ('LiteAgent-linux-amd64.zip', [dist / 'liteagent']),
]:
    # Refresh the unpacked distribution without touching runtime data folders.
    # Atomic replacement also works when the previous Linux binary is running.
    unpacked = dist / name.removesuffix('.zip')
    unpacked.mkdir(exist_ok=True)
    packaged = [(path, path.name) for path in files] + common
    for path, relative in packaged:
        destination = unpacked / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        handle, temporary = tempfile.mkstemp(prefix='.package-', dir=destination.parent)
        os.close(handle)
        try:
            shutil.copy2(path, temporary)
            os.replace(temporary, destination)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
    with zipfile.ZipFile(dist / name, 'w', zipfile.ZIP_DEFLATED) as archive:
        for path, relative in packaged:
            archive.write(path, relative)
paths = [dist / name for name in ['LiteAgent.exe', 'liteagent', 'LiteAgent-windows-amd64.zip', 'LiteAgent-linux-amd64.zip']]
(dist / 'SHA256SUMS.txt').write_text(''.join(f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n' for path in paths))
for path in paths:
    print(f'{path.name}: {path.stat().st_size / 1024 / 1024:.2f} MiB')
