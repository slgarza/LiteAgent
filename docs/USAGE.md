# LiteAgent user guide

[Back to the project README](../README.md)

A local AI chat with browser and local file tools, persistent skills and scheduled reminders. One small Go executable serves the chat and runs the agent. The application is independently implemented; no OpenClaw or Hermes code is included.

**Version 0.3.0.** The interface and commands are in English; the model is instructed to reply in the user's language. Browser automation is tested with real Chrome on Linux; provider protocols use mock servers in tests. The Windows executable is cross-compiled and still needs testing on Windows. Actual model reliability depends on the model and your tasks.

## Start chatting

Configure a remote model with an API key, or a local model through Ollama. Chrome is required only for browser automation. Go, Node and Python are not required to run an already compiled package.

**GitHub currently contains source code only.** Its **Code → Download ZIP** does not contain the compiled application, and no binary Release has been published. `dist/` is generated locally by the build script and excluded from Git. If you downloaded source, install **Go version 1.25 or newer** and follow the [source instructions in the README](../README.md#run-from-github-source). Go is the language/compiler used to turn the code into an executable.

On Windows, fully extract the source ZIP and open PowerShell in the folder containing `go.mod`, `cmd`, `internal` and `web`. `Test-Path .\go.mod` must return `True`. If Go reports `go.mod file not found`, compilation has not produced `LiteAgent.exe`; fix the working directory first. The README includes an example that starts the executable only after a successful build.

The following instructions apply when you already have a compiled package:

**Windows:** extract `LiteAgent-windows-amd64.zip` and open `LiteAgent.exe`. The chat opens at `http://127.0.0.1:8787`. The production executable runs without a console window. Use **Quit LiteAgent** in the sidebar to stop the server, cancel active work and stop processing reminders until the next launch. Closing a browser tab leaves the server running; reopening the same executable/data folder brings back the running instance. Desktop notifications still require an open tab. Startup errors appear in a Windows dialog; diagnostics go to `liteagent.log` in the runtime data directory. `Start-LiteAgent.bat` uses a portable `data` folder next to the executable. Quit any instance using the default port before switching data folders, or use another `--port`.

When building from source, use `go build -tags desktop -ldflags "-H=windowsgui" -o LiteAgent.exe ./cmd/liteagent` for this behavior. `go run ./cmd/liteagent serve` and ordinary Go builds retain a console for development. The packaged `LiteAgent-console.exe` provides terminal commands and console diagnostics.

**Linux:** extract `LiteAgent-linux-amd64.zip`, open a terminal in that folder and run `./liteagent`. If extraction did not preserve executable permissions, run `chmod +x liteagent` first. Keep that terminal open; Ctrl+C or **Quit LiteAgent** stops the server.

Open **Settings → Models & API keys** to save and activate an Ollama, OpenAI-compatible or Anthropic connection. Each profile keeps its own endpoint, model and optional key. In the terminal, `/onboard` still guides you through setup. The web `/onboard` opens the connection settings. The **model and reasoning selector beside Send** opens a compact panel to change either setting. **Refresh models** updates the list after installing a model. The selection is saved and applies to all chats; finish or stop an active reply before switching. `/models` and `/model MODEL_ID` remain available in the web chat and CLI, including for providers that do not list models.

When adding or editing a connection, the **Model** dropdown loads IDs directly from the provider before you save. Enter the base URL and, for OpenAI or Claude, your API key; use **Fetch models** to refresh the list. Existing connections can reuse their saved key for discovery without exposing it to the browser. Previously used models appear separately as a fallback. Choose **Enter model ID manually** for an endpoint that does not list models. Discovery does not save or activate a connection, and changing the endpoint cancels an outstanding lookup. A listed model is not a guarantee of chat or tool support; choose a text model that supports your selected API.

The selector describes the known local Qwen profiles in English: 0.6B prioritizes speed for basic chat, 4B balances speed and capability for everyday tasks, and 8B prioritizes capability with longer waits on CPU. **About this model** opens a detail popup with suggested uses, limitations and expected response time when a profile exists. Models without a profile show only their name. The description stays out of the conversation. These are qualitative guides, not timing guarantees. Friendly labels keep the original model IDs as option values and hover titles.

If another application uses port 8787:

```text
LiteAgent.exe serve --port 8797
```

On Linux, substitute `./liteagent` for `LiteAgent.exe` in the examples. Use `serve --no-open` to start without opening the web chat. For a machine without a desktop, send `/config headless true` before using browser tools.

## Workspace controls

**Commands** opens a searchable glass palette, also available with **Ctrl/Cmd+K**. Typing `/` in the composer shows matching commands; use arrows and Tab to complete them. In the web UI, `/help`, `/status`, `/config`, `/skills`, `/models` and schedule-list commands open their corresponding panels without adding messages to the conversation. Commands with arguments and terminal commands remain available through the shared backend.

**Settings** contains six sections:

- **Chat & runs:** temperature, output token limit, Ollama context size, history window, request/run timeouts, action limit and custom instructions.
- **Models & API keys:** named provider profiles; add, edit, fetch and select available models, test discovery, activate and remove connections. API keys are never returned to the browser. An empty key preserves the existing credential only for the same provider and endpoint; a checkbox explicitly removes it. Pasting a new key clears that checkbox. Changing endpoints never carries a saved key to the new address. Editing the active profile updates the current connection.
- **Notifications:** enable desktop alerts, mute or enable the chime, choose whether to be notified about replies while away, and test delivery. Preferences are saved in this browser.
- **Crons & reminders:** all schedules from every chat, including those created by the agent. Inspect their expression, time zone, next execution, last result and errors; filter, pause, resume, run now, open the conversation or delete a schedule. The sidebar Schedules button opens this section too.
- **Skills:** create, inspect, edit and remove saved procedures. A skill referenced by a schedule cannot be removed until the schedule is removed.
- **MCP servers:** save Streamable HTTP endpoints with optional bearer tokens; discover tools, enable/disable, edit or remove servers.

The **Reasoning** selector applies to future chats and scheduled executions. Auto leaves the provider default. Ollama supports boolean on/off and selected effort levels; OpenAI-compatible endpoints use `reasoning_effort`; Claude effort levels request adaptive thinking. Support depends on the selected model and server. Choose Auto for models that reject overrides. Claude thinking blocks are preserved internally during tool calls, including their signatures; they are not exposed as conversation text. Temperature stays unset unless explicitly chosen.

Numeric generation settings accept zero as the built-in/provider default: output tokens (provider default; Claude 4096), Ollama context (16384), history (12 turns), and run timeout (1800 seconds). Request timeout defaults to **900 seconds (15 minutes)** and can be changed in **Settings → Chat & runs → Request timeout**, from 10 to 86400 seconds. It limits each model request; the separate run timeout limits the entire reply and its actions. Request timeout and action limit require explicit values. All configuration is global; running jobs keep their starting model configuration.

## Terminal chat

```text
LiteAgent-console.exe chat
LiteAgent-console.exe chat --port 8797
LiteAgent-console.exe chat --chat CONVERSATION_ID
LiteAgent-console.exe chat --data data
```

On Linux, use `./liteagent` in these terminal examples. The CLI connects to LiteAgent on the chosen port, or starts its own local server if none is running. It shares conversations, settings, skills and schedules with the web chat. Use `/chats` and `/resume ID` to continue a conversation, and `/exit` to leave the CLI. Reminders appear while the CLI is open.

When the CLI starts its own server, exiting also stops that server. When it connects to an existing server, exiting leaves the server running. `--data PATH` chooses storage when starting a new server; an existing server keeps its own storage.

To send one message to an already running server:

```text
LiteAgent-console.exe run "Open https://example.com and tell me the heading"
```

## Commands

Commands work without a model, except `/models`, which contacts the configured provider.

| Command | Purpose |
| --- | --- |
| `/onboard` | Connect a model through a guided conversation |
| `/cancel` | Leave setup without saving unfinished changes |
| `/config` | Show settings and the supported configuration commands |
| `/config SETTING VALUE` | Change a setting; e.g. `/config timezone America/Mexico_City` |
| `/status` | Show provider, model, current activity, skill count and local time |
| `/models` | List models available from the provider |
| `/model MODEL_ID` | Switch the configured model |
| `/skills` | List installed skills |
| `/schedules` | List reminders and scheduled skills |
| `/notifications` | Open sound and desktop notification settings in the web chat |
| `/stop` | Cancel the current reply and browser operation |
| `/new` | Start a fresh conversation |
| `/reset` or `/clear` | Reset this chat’s model context while keeping its history and schedules |
| `/reasoning MODE` | Set reasoning; omit MODE to inspect provider options |
| `/context` | Show context turns and byte size |
| `/rename TITLE` | Rename the current conversation |
| `/retry confirm` | Repeat the last user request; browser actions may repeat |
| `/crons` | List all scheduled jobs |
| `/cron` | Show cron management commands |
| `/chats` | List conversations and their IDs |
| `/resume ID` | Return to a conversation |
| `/help` | Show command help and examples |

Settings include `provider`, `url`, `model`, `key`, `timezone`, `headless`, `max_steps`, `timeout`, `thinking`, `browser_path`, `driver_path`, `reasoning`, `temperature`, `max_tokens`, `context_tokens`, `history_turns`, `run_timeout` and `system_prompt`. Set temperature to `auto`, and custom instructions with `/config system_prompt TEXT` (or `clear`). For Ollama models, `/config thinking false` disables extended reasoning when supported, `/config thinking true` enables it, and `/config thinking auto` restores the model default. `/config key clear` removes a saved key. Changing provider or URL clears the old key. Set browser or driver paths to `auto` to restore detection.

Keys entered during onboarding or through `/config key` are hidden from conversation history and model context. The CLI does not create a shell history file, but terminal input itself can be visible. Setup is complete after settings are saved; `/models` checks model discovery, and a normal message checks inference.

Local models on CPU may take several minutes to process the first message, especially with tools or a long conversation. New installations allow up to 900 seconds per model request; existing settings are preserved. Adjust this in **Settings → Chat & runs → Request timeout** or with `/config timeout 900`. For longer tasks, also increase **Run timeout**, which caps the entire execution. `/config` shows the current timeout. Listing models successfully does not verify inference speed. The agent keeps its system prompt stable so the model can reuse cached instructions and tool definitions across turns.

## Providers

| Provider in `/onboard` | Default URL | Requirements |
| --- | --- | --- |
| `ollama` | `http://127.0.0.1:11434` | Running Ollama and a downloaded model supporting tool calls |
| `openai` | `https://api.openai.com/v1` | OpenAI or compatible Chat Completions API and a model supporting tools |
| `openai-responses` | `https://api.openai.com/v1` | OpenAI API key and a Responses-compatible model (including supported Codex models) |
| `anthropic` | `https://api.anthropic.com` | Anthropic API key and model ID |
| `vscode` | `http://127.0.0.1:8788/v1` | Running LiteAgent VS Code bridge, its local token and an exposed model |

### First local model

[Install Ollama](https://ollama.com/download), then open the Ollama application. If no server is running, start `ollama serve` in another terminal and keep it open. Download a small initial model from PowerShell or a terminal:

```bash
ollama pull qwen3:0.6b
ollama list
```

LiteAgent queries Ollama's [installed model list](https://docs.ollama.com/api/tags) on connection and roughly every 15 seconds while idle with the model menu closed. **Check again** on the setup card or **Refresh models** in the picker refreshes it immediately. It does not download or load every listed model into memory. Select an installed entry beside Send; Ollama loads the selected model when a request needs it.

If Ollama is unavailable, the chat shows installation/startup instructions and the configured address. If its list is empty, it shows the download command. If the saved model is absent, choose an installed model. The main Ollama picker only offers installed entries, and ordinary messages stay disabled until the selected model is listed. Slash commands and API connection settings remain available. Discovery proves installation, not successful inference: a model can still fail if memory is insufficient or the requested features are unsupported.

[Qwen3 0.6B](https://ollama.com/library/qwen3:0.6b) is a small option for basic chat, with limited reliability on complex tool tasks. For the larger Qwen3 8B GGUF model, the [official model card](https://huggingface.co/Qwen/Qwen3-8B-GGUF) provides:

```bash
ollama run hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M
```

After downloading, this command opens an interactive Ollama chat. Enter `/bye` to leave it. Keep the Ollama application/server running and choose **`hf.co/Qwen/Qwen3-8B-GGUF:Q4_K_M`** in LiteAgent; `qwen3:8b` is a separate ID and requires its own download. You can use `ollama pull` with either ID when you only want to download it. See the [Ollama CLI reference](https://docs.ollama.com/cli).

If the Ollama app is already running, do not launch a second server. LiteAgent does not install Ollama or bundle model weights. Downloads require internet access. The adapter defaults to a 16K context and preserves the model’s sampling defaults unless you override temperature; memory requirements and tool reliability vary by model and hardware.

### VS Code model bridge

Select **VS Code · model bridge** in connection Settings to route requests through models exposed by VS Code's public Language Model API. Install and start the optional extension, copy its bridge token into LiteAgent, then fetch and select a model. Keep VS Code open. If needed, authorize the model using **LiteAgent: Authorize and Test a Model** in VS Code. See [installation and troubleshooting](VSCODE.md).

The bridge token authenticates a local connection. The selected provider's account access and usage limits still apply. The bridge does not import subscription credentials or read other chat panels. It supports text and tool calls, uses default reasoning/sampling/output settings, and sends system instructions as initial user content because the public consumer API exposes only user/assistant roles. LiteAgent continues to execute its own tools.

### Remote providers

Choose **OpenAI Chat** for Chat Completions-compatible servers, or **OpenAI Responses / Codex models** for models served through the Responses API. Claude uses Anthropic Messages. Responses requests use `store: false` and retain encrypted reasoning items internally across tool calls. Model availability depends on API access. LiteAgent does not reuse Codex, ChatGPT or Claude Code subscription/login sessions; configure API credentials separately.

## Everything else is a conversation

Examples after connecting a model:

- “What skills do you have?”
- “Do you have superticket installed?”
- “Open the browser so I can sign in.”
- “Open https://example.com and tell me the heading.”
- “Create a folder on my Desktop and save this page as index.html.”
- “Read ~/Desktop/my-page/index.html and change its heading.”
- “Remember this procedure as superticket: open [URL], find the ticket summary, read the total and verify the date.”
- “Install this skill from [direct Markdown URL].”
- “Remind me every day at 2 PM to review the tickets.”
- “Run superticket every day at 2 PM.”
- “Pause the superticket schedule.”

The model receives tools for these operations. Successful tool calls create real saved skills and schedules; the action log is expandable inside the assistant's message. Skills contain reusable text instructions, not executable plugins or arbitrary shell commands. Existing skill files can be imported from direct text URLs; a plugin requiring Python, shell scripts or other runtimes will not work unchanged.

Skills persist across restarts. The agent can list, search, read, create, update and remove them. Relevant skills are selected by keyword and the model can load more through tools. To adjust a schedule's time, ask to replace it; pause, resume and delete are also supported.

## Reminders and scheduled skills

Schedules support **daily**, **one-time** and **five-field cron** execution with an IANA time zone. They can deliver a reminder, execute a saved skill, or run a standalone AI task. Open **Schedules** to create jobs and inspect their next execution, last status, errors and conversation; pause, resume, run now or delete them there. Browser onboarding uses the browser's time zone; otherwise the initial default is `America/Mexico_City`. Check `/status` or set `/config timezone YOUR_ZONE`. Existing schedules retain their own time zones.

The server checks schedules every five seconds. Reminders are saved as messages in their original conversation, even without a configured model. A scheduled skill runs the agent with that saved procedure and puts its result in the conversation. One agent operation runs at a time; scheduled tasks wait while it is busy. The oldest due job goes first. Scheduled executions use their saved instructions without inheriting unrelated chat context. Reminders can still arrive during another operation.

**LiteAgent and the computer must be running.** There is no Windows service, automatic startup, sleeping-PC wake-up, email or mobile push in this version. Keep the web chat open for browser notifications; the tab can stay in the background. In **Settings → Notifications**, click **Enable desktop alerts** and accept the browser permission prompt. **Test notification** checks the in-app card, desktop alert and sound. Audio requires an interaction with the page and can be muted independently. If permission is blocked or desktop alerts are unsupported, in-app cards still appear. Clicking an alert opens its conversation. Reminders and finished scheduled tasks notify across chats; ordinary reply alerts apply only while away and can be disabled. The first visit silently records historical alerts, subsequent polls do not replay delivered alerts, and browsers supporting Web Locks coordinate delivery across tabs.

Missed occurrences are delivered once when the server resumes, rather than replaying every missed day. An occurrence and its execution are committed atomically before any agent action. Failed, cancelled, timed-out or interrupted executions pause their job and retain the error for review. Restarting never replays an occurrence that already started. Fix the issue before resuming; a one-time job in the past can be executed manually with Run now. There are no automatic retries of external actions.

Cron examples (minute, hour, day of month, month, day of week):

```text
/cron next 0 14 * * 1-5 America/Mexico_City
/cron add {"title":"Weekday review","kind":"task","task":"Open https://example.com and report its heading","repeat":"cron","cron":"0 14 * * 1-5"}
/cron pause ID
/cron resume ID
/cron run ID
/cron history ID
/cron delete ID
```

`/cron next` previews five occurrences. Cron accepts ranges, lists and steps, with minute precision; seconds and `@every` are not supported. Standard cron day-of-month/day-of-week matching applies. Times skipped by a daylight-saving transition are skipped; a repeated local time can occur twice. A one-time schedule at a nonexistent local time is rejected. Run now does not advance the next scheduled occurrence or enable a paused job. Editing a job’s definition currently means replacing it.

You can delete other conversations while the agent is working. Deleting the conversation currently in use stops its reply, waits for tool cleanup, then removes its history and linked schedules. The confirmation explains when a reply will be stopped, and the delete button shows progress while waiting. Files already saved and website actions remain in effect. Removing a scheduled skill requires deleting the schedules that use it first.

## Browser and memory

LiteAgent uses Selenium's W3C WebDriver protocol directly, without a Python or Selenium runtime. It downloads a matching ChromeDriver from Google's Chrome for Testing distribution on first use if needed. Chrome must already be installed; the browser keeps its normal sandbox.

Browser tools operate a real Chrome session: opening pages, typing, clicking and downloading are real actions. The model cannot execute arbitrary Python/Selenium scripts. To verify browser control, give it a concrete task on a test page and inspect the action log and the page result; a conversational description of its capabilities alone does not test them.

The dedicated browser profile preserves sign-in sessions. Ask to open the browser and sign in manually there. Tools support navigation, reading page text and controls, clicking, typing, selecting options, keyboard keys, scrolling, waits, tabs, iframes, JavaScript dialogs and checking downloaded filenames, sizes and completion. No arbitrary model-generated JavaScript is executed.

Local file tools are enabled for the current user's home directory, including its Desktop and Downloads folders. `filesystem_info` reports the operating system and actual Desktop location (XDG on Linux, Known Folders on Windows). `filesystem_list`, `filesystem_read`, `filesystem_mkdir`, `filesystem_write` and `filesystem_edit` perform real directory and text-file operations. Paths can be absolute, start with `~/`, or be relative to home. Operating-system permissions still apply; paths outside home and symbolic links escaping that directory are rejected. A Desktop redirected outside home is reported as outside the available scope.

Text files, including HTML, CSS, JavaScript and Markdown, can be up to 1 MiB and must use UTF-8. `filesystem_stat` inspects file metadata without reading content. Reads and directory listings support pagination and optional snapshot checks; listings sort directories first and names alphabetically, with a 10,000-entry scan limit. See [Filesystem behavior](FILESYSTEM.md). New files never silently replace an existing file. To change a file, the agent reads it and supplies its SHA-256; a changed file must be reread. Updates stage complete contents before replacing the destination. Targeted edits require exactly one matching fragment. Successful file operations appear in the action log, and the agent is instructed to read back saved files before reporting completion. These tools do not run scripts, delete files, or parse binary documents such as PDFs and DOCX files.

Conversations survive restarts. Each new reply receives a configurable number of recent user/assistant pairs (12 by default), within a 16,000-byte context budget. Commands, reminders and scheduled runs do not consume conversational context; `/reset` creates a durable context boundary. Older browser observations are truncated during a run; current controls are reread before use. Saved history is no longer silently deleted after 200 global replies. It remains until you delete its conversation; storage usage grows with history, and the JSON store is intended for a small local workspace. Skills are stored separately from conversation context.

Current limits: Chrome 115+, one browser profile and one active agent operation, up to 120 visible controls and bounded page text per observation, a default 20-action limit and a configurable 30-minute reply deadline. Built-in tools provide no desktop application control, visual screenshot reasoning, deep shadow DOM support, CAPTCHA automation, file uploads, shell execution or binary document parsing. Replies appear when the model responds; there is no token streaming yet. A completed reply means the model stopped, not independent proof that a website task succeeded.

## MCP over HTTP

MCP supports the Streamable HTTP transport: initialization, protocol negotiation, session IDs, paginated tool discovery, JSON/SSE replies and tool calls. Both local and remote endpoints are supported. Optional authentication uses a bearer token. Supported protocol versions are 2025-03-26, 2025-06-18 and 2025-11-25. Discovery runs at the start of each reply, and enabled tools also participate in scheduled automation. Test & tools only discovers tools; it does not invoke them.

Text and structured tool results are sent to the model and recorded in the action log. The tool list is capped at 64 external tools across enabled servers. Server request timeouts and Stop cancellation apply during discovery and execution. A missing response or tool error stops the run without retrying an action whose effect may be unknown. A failed enabled server reports an explicit error; disable it to continue without its tools. Sessions are closed after each run when the server supports termination.

This lightweight client does not launch stdio processes, implement the legacy HTTP+SSE transport, OAuth login, resources/prompts, sampling, elicitation or remote asynchronous tasks. Enabled MCP tools can perform real external actions according to their server implementation. Configure servers you intend the agent to use.

## Automation API

`GET /api/health` identifies the local app and its data directory for desktop relaunches. `POST /api/shutdown` requires the same local host/origin checks and `X-LiteAgent: 1` header as other writes; it acknowledges shutdown, then cancels active work and closes the service. The UI exposes this action as **Quit LiteAgent**.

The local API still requires `X-LiteAgent: 1` for writes. `POST /api/connections/models` discovers models for an unsaved provider/endpoint/key, optionally reusing a matching saved connection's key by ID. It does not modify settings. `POST /api/chat` accepts an optional `request_id`; retrying the same payload with the same ID returns the recorded run, even after a restart. Reusing the ID for another payload is rejected. The web chat retains the ID after a failed transmission until the submission succeeds or changes.

`GET /api/schedules`, `POST /api/schedules` and `POST /api/schedules/ID` expose schedule definitions and pause/resume/run/delete actions. Creation also accepts a `request_id` to prevent duplicate jobs after a lost response, including when a fresh chat was created. Generation overrides use `PATCH /api/generation`, while connections and MCP servers have separate credential-preserving endpoints. Stop remains available while model discovery is in flight.

## Storage

Default: `%AppData%\LiteAgent` on Windows, or the user's configuration directory followed by `/LiteAgent` on Linux. Use `--data PATH` for portable storage. The server takes an operating-system file lock so two instances cannot run against the same data directory.

```text
state.json          Settings, connection profiles, MCP servers, chats, skills and schedules
browser-profile/    Agent browser cookies and sessions
downloads/          Downloaded files
drivers/            Cached ChromeDriver
chromedriver.log    Driver diagnostics
liteagent.log       Windows desktop server diagnostics
```

State uses atomic file replacement. On POSIX, the state file has `0600` permissions. Provider keys and MCP bearer tokens are stored locally without encryption; Windows user-profile permissions apply. The data directory is excluded from packages and should not be committed or shared.

The server binds to `127.0.0.1`, validates host/origin and requires an application header for writes. It is intended for one local user. With a remote provider, the conversation, selected skills, relevant browser content and text read through file tools are sent to that provider. Stopping a reply cannot undo website actions or file changes already performed.

## Build and verify

Go 1.25 or newer (for the scoped filesystem operations):

```bash
go test -race ./...
go vet ./...
LITEAGENT_BROWSER_TEST=1 go test ./internal/app -run 'TestBrowserIntegration|TestAgentBrowserE2E|TestWorkspaceUI' -v
./scripts/build.sh
```

Browser tests operate real headless Chrome against local fixtures and may download a matching driver. Tests also cover provider reasoning payloads, signed Claude thinking preservation, reset boundaries, duplicate submissions, credential redaction, profile activation, MCP JSON/SSE sessions and cancellation, cron/DST behavior, failure pausing, restart recovery and fair scheduling. `TestWorkspaceUI` exercises the settings, glass palette, model popup and management forms in real Chrome at desktop/mobile sizes. Mock model tests do not establish the reasoning quality of a real model.

The build script needs Bash and Python 3 for packaging. It generates Linux and Windows AMD64 executables, ZIPs and SHA256 checksums. Windows packages include both the desktop `LiteAgent.exe` and terminal `LiteAgent-console.exe`. Binary sizes exclude Chrome, ChromeDriver, local data and model weights.

## Code and dependencies

The Go application uses `github.com/robfig/cron/v3` for standard cron parsing and time-zone calculations. `agent.go` handles conversation and tools; `browser.go` and `driver.go` handle WebDriver; `provider.go` adapts model protocols; `commands.go` implements chat commands; `scheduler.go` handles saved procedures and schedules; `store.go` persists data; `server.go` serves the API and embedded chat; `cli.go` implements terminal chat.

The UI follows familiar chat conventions using Open WebUI's documented interface as a reference. Markdown rendering uses locally bundled Marked and DOMPurify. No CDN or package-manager installation is needed at runtime.

Application code is under the proprietary [LICENSE](../LICENSE). Third-party components retain their licenses; see [THIRD_PARTY_NOTICES.txt](../THIRD_PARTY_NOTICES.txt) and `LICENSES.txt` in the distribution.

Protocol references used by the adapters: [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create), [OpenAI Responses](https://developers.openai.com/api/reference/resources/responses/methods/create), [Ollama chat](https://docs.ollama.com/api/chat), [Claude thinking](https://platform.claude.com/docs/en/build-with-claude/thinking-steering-and-cost), [MCP Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports), and [robfig/cron](https://pkg.go.dev/github.com/robfig/cron/v3).
