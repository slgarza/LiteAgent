# Development

[Project README](../README.md)

Use Go 1.25+ for scoped filesystem operations. Packaging uses Bash and Python 3. Node is optional for JavaScript syntax checks; there is no frontend build step.

```bash
go run ./cmd/liteagent serve --no-open --port 8797 --data /tmp/liteagent-dev
go test -race ./...
go vet ./...
node --check web/app.js
node --check web/manage.js
node --check web/notifications.js
node --check web/setup.js
```

Use a separate port/data directory from the normal app. Tests use temporary directories and local fake providers without live credentials or model inference.

## Browser verification and screenshots

Chrome must be installed. Tests may download a matching ChromeDriver; the optional cache contains the driver, not the user's browser profile.

```bash
LITEAGENT_BROWSER_TEST=1 \
LITEAGENT_TEST_DATA=/tmp/liteagent-browser-cache \
LITEAGENT_SCREENSHOT_DIR=/tmp/liteagent-screenshots \
go test ./internal/app \
  -run 'TestBrowserIntegration|TestAgentBrowserE2E|TestWorkspaceUI' -v
```

`TestWorkspaceUI` covers composer controls, model discovery, missing/empty Ollama setup, automatic discovery of newly installed models, settings, notifications, schedules, chat deletion, Quit and responsive layouts. Its screenshots use fake conversations/providers. Permission prompts and audio devices are stubbed: the test checks UI behavior and audio scheduling, not physical speaker playback. Lifecycle tests check shutdown authorization, cancellation, socket/storage-lock release and desktop instance discovery.

Inspect each screenshot before copying selected images into `docs/screenshots/`. Never publish live-user captures. Record fixture provenance in the screenshot index.

## VS Code bridge

The optional extension is plain JavaScript with Node/VS Code built-ins and no runtime npm dependencies. Its bridge translates the OpenAI Chat subset used by LiteAgent into the public `vscode.lm` API. Credentials of other extensions are never read. The generated VSIX stays in ignored `dist/`.

```bash
node --test extensions/vscode-bridge/bridge.test.js
python3 scripts/test-vscode-bridge.py
python3 scripts/build-vscode-bridge.py
```

Packaging needs Node.js 22+, npm and Python, and downloads pinned `@vscode/vsce`. The host test requires desktop VS Code and a display; `LITEAGENT_CODE_BIN` can select its executable. It creates temporary user/extension directories and registers a synthetic language-model provider, so it does not use a signed-in account. A completion marker is required for success. Protocol tests cover authentication, origin/host checks, discovery, tool-result translation, access/quota failures, context limits and cancellation. Chrome checks the Settings flow separately. Real-provider behavior and Windows extension runtime still need manual verification.

See [VS Code setup](VSCODE.md) for the supported protocol and lifecycle.

## Document changes

Add user-visible changes under Unreleased in [CHANGELOG.md](../CHANGELOG.md). Update the affected guides/examples and describe concrete behavior and validation in commits. At release, date the version and align `internal/app/version.go`, README and user guide. State any untested platform behavior.

## Before committing

`.gitignore` cannot protect already tracked or forcibly staged files. Inspect staged paths and check staged bytes:

```bash
git add cmd internal web scripts docs README.md AGENTS.md CHANGELOG.md \
  LICENSE THIRD_PARTY_NOTICES.txt go.mod go.sum .gitignore .gitattributes
git diff --cached --stat
python3 scripts/check-repository.py --staged
```

The check validates allowed paths, common credential patterns and relative Markdown links. It reports filenames/rules without matched secret values and does not read the runtime directory. Pattern checks do not detect every secret; source and image review still matter.

## Packages

```bash
./scripts/build.sh
LITEAGENT_GO_BIN=/path/to/go ./scripts/build.sh
```

The second form selects a toolchain. `dist/` receives Linux/Windows AMD64 binaries, unpacked distributions, ZIPs and checksums. An explicit package list includes application binaries, docs and licenses, never runtime folders. Replacement does not delete existing local runtime directories. Generated packages are excluded from Git.

Chrome, browser sessions, models and credentials are not included. Windows is cross-compiled and still needs runtime validation.

### Windows desktop and console builds

On Windows, build the production executable with:

```powershell
go build -tags desktop -ldflags "-H=windowsgui" -o LiteAgent.exe ./cmd/liteagent
```

The [Go linker flag](https://pkg.go.dev/cmd/link) chooses the Windows GUI subsystem. The Windows-only `desktop` build tag enables native error dialogs, local diagnostics and reopening an existing instance. ChromeDriver and the browser launcher hide their child consoles. The server runs until **Quit LiteAgent**, process termination or system shutdown; closing a browser tab does not stop it.

Ordinary `go run` and builds keep the development console. Build the terminal client with:

```powershell
go build -o LiteAgent-console.exe ./cmd/liteagent
.\LiteAgent-console.exe chat
```

`scripts/build.sh` cross-compiles both Windows variants. Check the resulting PE subsystem (GUI for the desktop executable, console for the terminal executable), then validate launch, relaunch, startup errors, ChromeDriver and Quit on real Windows before claiming platform support. Wine checks do not replace that validation.
