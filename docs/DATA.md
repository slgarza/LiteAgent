# Data and publishing

[Project README](../README.md)

## Runtime storage

State lives in the OS user configuration directory plus `LiteAgent`, or an explicit `--data` directory. It can contain chats, API keys, MCP tokens, skills and schedules. The dedicated browser profile contains cookies and sign-ins. Downloads and logs may also contain private information.

Never include runtime state, browser profiles, downloads, driver caches, logs, `.env` files, credential exports or live-user screenshots in Git or distribution packages. On POSIX, persisted state uses `0600` permissions. Credentials are not encrypted at rest; public API state strips their values and onboarding commands redact them from history.

## Model and tool access

Remote inference sends conversation context, selected procedures and relevant tool results to the provider. Browser pages and files read for a task may therefore leave the computer. Offline inference depends on the selected model/provider.

The loopback service checks host/origin and requires a write header. It is designed for one local user, not as an authenticated multi-user web service. MCP servers have their own capabilities and boundaries.

## Repository

Only code, documentation, licenses and reviewed fixture screenshots are tracked. `scripts/check-repository.py --staged` verifies the Git index and common credential formats without printing matched values. Generated packages, diagnostics and local design caches stay excluded.

The initial screenshots come from temporary test applications, not the live chat. See [the screenshot index](screenshots/README.md). Public visibility does not change the existing proprietary license.

## Optional VS Code bridge

The bridge keeps its local authentication token in VS Code SecretStorage. LiteAgent stores the same token as its connection credential. Requests forward LiteAgent conversation context, instructions and tool results to the selected VS Code model provider. The extension does not read credentials or conversations from other extensions and does not record prompts/responses. Provider data handling and account limits still apply. See [setup and data flow](VSCODE.md).

The private `skills/` directory stores imported instructions, scripts, templates and other resources by package revision. Back it up together with `state.json`; package metadata alone cannot restore resource files. Older revisions and removed packages are retained until startup to support active tasks, then pruned. Never add runtime skill packages to the source repository.
