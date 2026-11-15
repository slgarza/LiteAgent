# Working on LiteAgent

## Structure and workflow

This is a local Go application with plain HTML/CSS/JavaScript. Keep the runtime small; do not introduce a frontend build system without a concrete need.

- `cmd/liteagent/main.go` calls `app.Main` and reports process errors.
- `internal/app/` owns the runtime, API, adapters, tools and tests.
- `web/` embeds only HTML, CSS, JavaScript and license notices.
- `extensions/vscode-bridge/` owns the optional VS Code extension and its tests; it uses the public Language Model API.
- `docs/` contains maintained guides and fixture screenshots.
- `dist/`, `.qa/`, browser profiles, downloads and local state are generated or private.

Read relevant code/tests, preserve local work and change only what the request needs. Add user-visible changes to `CHANGELOG.md` under Unreleased; update affected guides and screenshots. At release, align `internal/app/version.go`, README and user guide.

## Data boundaries

Never commit or upload `state.json`, API keys, MCP tokens, conversations, browser profiles/cookies, downloads, logs, `.env` files, live-user screenshots or machine-specific configuration. Do not print credentials while diagnosing failures. Test with temporary directories and fake providers, never with the user's actual data.

Stage explicit source/documentation paths, inspect `git diff --cached --stat`, then run `python3 scripts/check-repository.py --staged`. The check inspects the Git index without reading the runtime data directory. Use `TestWorkspaceUI` fixture screenshots and visually inspect them before inclusion.

Do not restart a running instance while an agent task is active. Preserve its runtime data directory when rebuilding or restarting.

## Runtime invariants

- Hold `runMu` when changing active execution, onboarding or schedule launch state. Never wait for a worker while holding a lock it needs.
- Deleting an active conversation cancels and waits for tool cleanup before removing records. Reject new work for pending deletions.
- Use `Store.Update` for mutations; failed persistence rolls back in-memory changes.
- Filesystem tools stay inside `os.Root`, validate text/size limits and require the prior SHA-256 to replace content. Preserve their lock and cancellation checks.
- Never automatically retry an external action whose outcome is unknown.
- Configuration is global; running tasks keep their starting configuration.
- Keep saved credentials out of public state and UI output.

## Checks

```bash
gofmt -w cmd internal web/assets.go
go test -race ./...
go vet ./...
node --check web/app.js
node --check web/manage.js
node --check web/notifications.js
node --check web/setup.js
LITEAGENT_BROWSER_TEST=1 go test ./internal/app -run 'TestBrowserIntegration|TestAgentBrowserE2E|TestWorkspaceUI' -v
python3 scripts/check-repository.py --staged
./scripts/build.sh
```

The Go app does not need Node at runtime. Node is used for optional UI syntax checks and VS Code extension development/packaging. Browser tests need Chrome and may download ChromeDriver. See `docs/DEVELOPMENT.md` for cache/screenshot options. Cross-compilation does not establish Windows runtime correctness.

Bridge changes also require `node --test extensions/vscode-bridge/bridge.test.js` and `python3 scripts/test-vscode-bridge.py` when desktop VS Code is available. The host test must use an isolated profile and synthetic provider. Package with `python3 scripts/build-vscode-bridge.py`; never publish the generated VSIX unless requested. Do not test live account inference automatically. Update the explicit package/repository allowlists when adding extension source files.

Retain the proprietary application license and third-party notices unless the owner explicitly changes them.
