# Changelog

Record user-visible work under **Unreleased**, grouped as Added, Changed, Fixed or Removed. At release, move entries into a dated version and align `internal/app/version.go`, the README and user guide.

## Unreleased

### Added

- Native reading and targeted editing of existing Excel `.xlsx`, Word `.docx` and PowerPoint `.pptx` files within the home directory. Includes paginated sections/text, Word and slide table text, worksheet values/formulas, format preservation and SHA-256 conflict checks. No Office or Python runtime is required.
- Office package bounds, atomic batch cell updates, protected/signed document edit checks, and synthetic compatibility tests with independent readers and optional LibreOffice rendering.

- Persistent follow-up queue above the composer: send while the agent works, edit or remove pending messages, and use Steer to apply a follow-up to the current reply at the next safe boundary.
- Queued messages run in order after successful replies. Stop, failed replies and restarts pause pending work; Resume starts only messages that have not been executed.

- Collapsible Thinking history alongside actions, using model-returned reasoning and Responses summaries, with a truthful waiting state and preserved expansion while the chat updates.
- Model-specific reasoning discovery from Ollama thinking metadata, Claude capabilities and compatible model metadata; the selector shows only reported controls and explains when only Auto is available.

- Optional VS Code extension and a named VS Code provider in Settings, routing text and tool calls through models exposed by the public Language Model API. Includes local token authentication, explicit model authorization, discovery, context checks, cancellation and timeout handling.
- Local VSIX packaging, protocol tests, an isolated VS Code host test with a synthetic model, and setup documentation/screenshots. Generated extensions are excluded from Git.
- Windows desktop build without a console, startup error dialogs and a local diagnostic log; the console build remains available for development and terminal commands.
- Quit LiteAgent in the sidebar, with graceful server shutdown and cancellation of active work. Reopening the desktop executable connects to the existing instance when its data directory matches.
- Ollama setup guidance for unavailable servers, empty model lists and missing selected models, with a small-model download command and a link to API settings.
- Automatic refresh of installed Ollama models while idle; the main picker no longer presents a missing default model as ready.

### Changed

- Switching models resets reasoning to Auto, preventing an effort or toggle from being carried to a model with different controls. `/reasoning` reports model capabilities instead of a generic provider list.

- Clarified source versus compiled-package startup, Go version requirements, Linux and Windows commands, and the absence of published binaries or a tracked `dist/` directory.
- Added Windows ZIP extraction and project-folder checks, troubleshooting for missing `go.mod`, and conditional startup after a successful build.
- Documented Ollama installation, model downloads, exact Hugging Face model IDs, and Windows desktop versus console builds. Windows packages include both executables.

## 0.3.0 — 2026-11-15

First documented repository snapshot. Earlier local development was not versioned in Git; the baseline summary below records the behavior included in this version.

### Added

- Source repository, concise README, user/developer guides, `AGENTS.md` and screenshots from isolated fixtures.
- Repository checks for runtime files, common credential patterns and broken local documentation links.
- `filesystem_stat` for metadata without reading content.
- Optional file-hash and directory-snapshot checks for paginated reads and listings.

### Changed

- Organized source into `cmd/liteagent`, `internal/app`, `web`, `scripts` and `docs`.
- Centralized the version used by the CLI, local API and MCP client.
- Directory listings sort folders first and names alphabetically, report metadata and limit scans to 10,000 entries.
- File operations serialize within the application so concurrent edits cannot both replace the same old content.
- Packages include documentation without copying runtime directories.

### Fixed

- Rejected updates no longer create missing parent folders or treat a missing content field as an empty file.
- Replacement writes preserve permission bits and recheck cancellation before publishing.
- A delayed Settings close event no longer clears a connection editor opened immediately afterward.

### Included from local development

- Browser and local text-file tools, persistent chats, saved skills and scheduled work.
- Ollama, OpenAI-compatible, Responses and Anthropic adapters; HTTP MCP servers.
- Named API connections, provider model discovery and credential-preserving edits.
- Configurable request timeout with a default of 900 seconds.
- Notification chimes, desktop permissions and Settings for all crons and reminders.
- Deleting other chats during a reply; cancelling and cleaning up before deleting the active chat and its schedules.
- Compact model/reasoning controls beside Send and Stop, without unavailable-profile labels.

### Validation

- Automated checks cover file scope, hash conflicts, pagination, cancellation, provider protocols, persistence and scheduling.
- Real Chrome exercises composer, settings, notifications and chat deletion with temporary state and fake providers.
- Linux and Windows AMD64 packages build. Windows runtime testing remains outstanding.
