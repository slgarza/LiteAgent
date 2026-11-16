# LiteAgent VS Code Bridge

Use language models exposed by VS Code as a provider for LiteAgent. The extension runs a local HTTP bridge using the public `vscode.lm` API. You choose an available model in LiteAgent; its chat history and tool results are routed through VS Code.

This is an independent LiteAgent extension. It is not endorsed by Microsoft, GitHub or any model provider.

Current package version: **0.1.1**. To upgrade, stop the bridge, install the new VSIX, run **Developer: Reload Window**, then start the bridge again. The existing token remains valid.

Version 0.1.1 fixes the `unsupported response part` 502 when a model emits thinking/data alongside its normal text or tool calls.

## Install and connect

1. Build the VSIX from the LiteAgent source folder with `python3 scripts/build-vscode-bridge.py` (Windows: `py scripts/build-vscode-bridge.py`). Packaging needs Python and Node.js 22+ with npm; the first build downloads the pinned VS Code packaging tool. Running the installed extension only needs desktop VS Code 1.104+.
2. In VS Code, open Extensions, choose **… → Install from VSIX**, and select `dist/LiteAgent-vscode-bridge.vsix`.
3. Enable a model provider that exposes models through VS Code's Language Model API. Open the command palette and run **LiteAgent: Start Model Bridge**.
4. Run **LiteAgent: Copy Bridge Token**.
5. In LiteAgent, open **Settings → Models & keys → Add**. Select **VS Code · model bridge**, keep `http://127.0.0.1:8788/v1`, and paste the bridge token into **API key**.
6. Fetch models, select one, and save the connection. If access is required, run **LiteAgent: Authorize and Test a Model** in VS Code and select that model. This sends one short test request and allows VS Code to show its native consent dialog. Then retry your message in LiteAgent.

The token authenticates the local bridge; it is not a provider API key. Copying it is an explicit command. Access errors, provider quota limits and unavailable models are reported to LiteAgent.

## Behavior and limits

- Only models returned by `vscode.lm.selectChatModels()` are listed. Installing a separate chat extension does not guarantee its models are exposed. The bridge does not inject text into chat panels, read other conversations, extract login tokens or call private provider endpoints.
- The model selected in **LiteAgent** is used for each request. The selected model in VS Code's own chat panel is not automatically synchronized. IDs include the vendor to distinguish providers.
- Keep the VS Code window and bridge running. Closing/reloading that window or stopping the bridge cancels in-flight requests. Start the bridge again after a reload. It does not start automatically or switch to another provider after an error.
- Your provider account, consent requirements and usage limits still apply. This extension does not turn a subscription into an unlimited API or add access to unavailable models.
- Text chat and function calls are supported. LiteAgent executes its own tools and sends their results back; the bridge does not execute VS Code tools or provide VS Code Agent mode. Each selected model must support the requested tools.
- The public consumer API has User and Assistant messages, so LiteAgent's system instructions are sent as the first user-message content. This changes their priority compared with a native system message.
- Native thinking parts are supported when exposed by the installed VS Code version. Only visible text is sent to LiteAgent’s Thinking history; IDs, signatures, encrypted state and metadata are omitted. Text/Markdown data parts are decoded as UTF-8. Other data and unknown future parts are skipped; they are never guessed to be text or tool calls. Thinking/data alone cannot complete a reply.
- Responses are collected completely before returning them to LiteAgent; incomplete tool calls are discarded on stream errors. Images, structured-output overrides, custom reasoning, temperature and output-token overrides are not supported by this bridge. Select Auto/default generation settings.
- Input/context limits, a 2 MiB request/response limit (including visible thinking and native data bytes), 65,536 streamed parts per response, 128 tool definitions and eight concurrent bridge requests apply. Requests time out after 900 seconds by default. Change **LiteAgent Bridge: Timeout Seconds** in VS Code Settings, then restart the bridge; LiteAgent's request/run deadlines also apply.
- The extension runs on the local UI side of desktop VS Code, including remote workspaces. LiteAgent must run on that same computer. The bridge binds only to `127.0.0.1`, checks the Host header and bearer token, rejects browser origins and does not enable CORS. Each independent VS Code bridge needs its own port.

Use **LiteAgent: Stop Model Bridge** or the status bar to stop routing. **LiteAgent: Rotate Bridge Token** stops the bridge and invalidates its old token; copy the new token into LiteAgent before restarting. Change **LiteAgent Bridge: Port** if another window uses port 8788.

## Data handling

The extension forwards the supplied conversation, instructions, tool definitions and tool results to the chosen VS Code model provider. It does not read workspace files or other chat panels and does not log or persist prompts/responses. The bridge token is stored through VS Code SecretStorage; LiteAgent stores its connection token in its own local state. The provider's normal data handling applies. Stop the bridge or uninstall the extension to stop forwarding. Rotate its token to revoke existing clients.

## Development and validation

From the repository root:

```bash
node --test extensions/vscode-bridge/bridge.test.js
python3 scripts/test-vscode-bridge.py
python3 scripts/build-vscode-bridge.py
```

The host test requires desktop VS Code and a display. It uses a temporary profile, separate extension directory and a synthetic model provider, without account credentials or live model requests. The VSIX package includes only the manifest, three runtime JavaScript files, this README and the application license.

See the [LiteAgent guide](https://github.com/slgarza/LiteAgent/blob/main/docs/USAGE.md), [VS Code Language Model API](https://code.visualstudio.com/api/extension-guides/ai/language-model), and [API reference](https://code.visualstudio.com/api/references/vscode-api#lm). Report issues in the [LiteAgent repository](https://github.com/slgarza/LiteAgent/issues).
