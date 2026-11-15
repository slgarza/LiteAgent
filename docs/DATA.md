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
