#!/usr/bin/env python3
"""Test the VS Code API with an isolated profile and a synthetic model provider."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]
source = root / 'extensions' / 'vscode-bridge'
with tempfile.TemporaryDirectory(prefix='liteagent-vscode-test-') as temporary:
    directory = Path(temporary)
    fixture = directory / 'fixture'
    fixture.mkdir()
    (fixture / 'package.json').write_text(json.dumps({
        'name': 'fixture', 'publisher': 'liteagent-test', 'version': '0.0.1',
        'engines': {'vscode': '^1.104.0'}, 'main': './extension.js',
        'activationEvents': ['onLanguageModelChatProvider:liteagent-fixture'],
        'contributes': {'languageModelChatProviders': [{'vendor': 'liteagent-fixture', 'displayName': 'LiteAgent test provider'}]},
    }))
    shutil.copyfile(source / 'test-fixture.js', fixture / 'extension.js')
    environment = dict(os.environ)
    environment.pop('ELECTRON_RUN_AS_NODE', None)
    environment.pop('VSCODE_IPC_HOOK_CLI', None)
    environment['LITEAGENT_HOST_TEST_RESULT'] = str(directory / 'result')
    executable = Path(shutil.which(os.environ.get('LITEAGENT_CODE_BIN', 'code')) or 'code').resolve()
    # Launch Electron itself, rather than a CLI that returns before tests finish.
    for candidate in [executable.parent.parent / 'code', executable.parent.parent / 'Code.exe']:
        if executable.parent.name.lower() == 'bin' and candidate.is_file():
            executable = candidate
            break
    command = [str(executable), '--new-window', '--disable-gpu',
               '--disable-workspace-trust', '--skip-welcome', '--skip-release-notes',
               '--user-data-dir', str(directory / 'profile'), '--extensions-dir', str(directory / 'extensions'),
               '--extensionDevelopmentPath=' + str(source), '--extensionDevelopmentPath=' + str(fixture),
               '--extensionTestsPath=' + str(source / 'test-host.js')]
    result = subprocess.run(command, env=environment, timeout=90, check=False, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, errors='replace')
    if result.returncode or not (directory / 'result').is_file():
        print(result.stdout[-8000:])
        raise SystemExit('VS Code host test did not produce a success result.')
    print('VS Code host passed: activation, native model discovery, tool round trip and cancellation; synthetic provider, no live account.')
    raise SystemExit(result.returncode)
