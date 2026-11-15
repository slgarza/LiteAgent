# Changelog

Record user-visible work under **Unreleased**, grouped as Added, Changed, Fixed or Removed. At release, move entries into a dated version and align `internal/app/version.go`, the README and user guide.

## Unreleased

### Changed

- Clarified source versus compiled-package startup, Go version requirements, Linux and Windows commands, and the absence of published binaries or a tracked `dist/` directory.
- Added Windows ZIP extraction and project-folder checks, troubleshooting for missing `go.mod`, and conditional startup after a successful build.

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
