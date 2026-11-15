'use strict';
const vscode = require('vscode');
const {randomBytes} = require('node:crypto');
const {startBridge, errorResponse} = require('./bridge');
const {cancellable} = require('./protocol');

let bridge, starting, status;
const authorizations = new Set();
const secretName = 'liteagent.bridgeToken';
async function stop() {
  if (starting) { try { await starting; } catch {} }
  const current = bridge; bridge = undefined;
  for (const source of authorizations) source.cancel();
  if (current) await current.close();
  if (status) { status.text = '$(debug-disconnect) LiteAgent: off'; status.tooltip = 'Start the LiteAgent model bridge'; }
}
function activate(context) {
  const config = () => vscode.workspace.getConfiguration('liteagentBridge');
  const access = model => context.languageModelAccessInformation.canSendRequest(model) === true;
  async function bridgeToken() {
    let token = await context.secrets.get(secretName);
    if (!token) { token = randomBytes(32).toString('base64url'); await context.secrets.store(secretName, token); }
    return token;
  }
  async function start() {
    if (bridge) return;
    if (starting) return starting;
    starting = (async () => {
      if (!vscode.workspace.isTrusted) throw new Error('Open a trusted VS Code window to start the bridge.');
      // Model selection is initiated by this explicit user command.
      const models = await vscode.lm.selectChatModels();
      const port = config().get('port', 8788), seconds = config().get('timeoutSeconds', 900);
      if (!Number.isInteger(port) || port < 1024 || port > 65535 || !Number.isInteger(seconds) || seconds < 10 || seconds > 86400) throw new Error('Check the bridge port and timeout in VS Code Settings.');
      bridge = await startBridge(vscode, {token: await bridgeToken(), port, timeoutMs: seconds * 1000, canSend: access});
      status.text = '$(plug) LiteAgent: on'; status.tooltip = bridge.url + ' · Click to stop';
      if (!models.length) vscode.window.showWarningMessage('Bridge started, but no language models are exposed by VS Code. Enable a provider that supports the Language Model API.');
      else vscode.window.showInformationMessage('Bridge ready at ' + bridge.url + '. Copy its token into LiteAgent’s VS Code connection and fetch models.');
    })();
    try { await starting; } finally { starting = undefined; }
  }
  async function authorize() {
    const models = await vscode.lm.selectChatModels();
    if (!models.length) { vscode.window.showWarningMessage('No language models are exposed by VS Code. A separate chat extension may not expose its models to other extensions.'); return; }
    const picked = await vscode.window.showQuickPick(models.map(model => ({label: model.name, description: model.vendor + ' · ' + model.id, model})), {title: 'Authorize and test a model for LiteAgent', placeHolder: 'Sends one short test request using your provider account and quota'});
    if (!picked) return;
    await vscode.window.withProgress({location: vscode.ProgressLocation.Notification, title: 'Authorizing the LiteAgent model bridge', cancellable: true}, async (_progress, cancellation) => {
      const source = new vscode.CancellationTokenSource(); authorizations.add(source);
      const subscription = cancellation.onCancellationRequested(() => source.cancel());
      const timer = setTimeout(() => source.cancel(), config().get('timeoutSeconds', 900) * 1000);
      try {
        const response = await cancellable(picked.model.sendRequest([vscode.LanguageModelChatMessage.User('Reply with OK.')], {justification: 'Allow LiteAgent to send its chat context and tool results to this model through the local bridge.'}, source.token), source.token);
        const iterator = response.text[Symbol.asyncIterator]();
        while (!(await cancellable(iterator.next(), source.token)).done) { /* Discard the test reply; never log conversations. */ }
        vscode.window.showInformationMessage('Model access is ready. Select this model in LiteAgent.');
      } finally { clearTimeout(timer); source.cancel(); source.dispose(); subscription.dispose(); authorizations.delete(source); }
    });
  }
  function command(name, work) {
    context.subscriptions.push(vscode.commands.registerCommand(name, async () => {
      try { await work(); }
      catch (error) {
        const message = error?.code === 'EADDRINUSE' ? 'The bridge port is already in use. Stop the bridge in the other VS Code window, or choose another port.'
          : error instanceof Error && !error.code && !name.endsWith('authorize') ? error.message : errorResponse(error).message;
        vscode.window.showErrorMessage(message);
      }
    }));
  }
  status = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 10);
  status.text = '$(debug-disconnect) LiteAgent: off'; status.command = 'liteagentBridge.toggle'; status.show();
  context.subscriptions.push(status);
  command('liteagentBridge.toggle', async () => { if (bridge) await stop(); else await start(); });
  command('liteagentBridge.start', start);
  command('liteagentBridge.stop', stop);
  command('liteagentBridge.authorize', authorize);
  command('liteagentBridge.copyToken', async () => {
    await vscode.env.clipboard.writeText(await bridgeToken());
    vscode.window.showInformationMessage('Bridge token copied. Paste it into the API key field of LiteAgent’s VS Code connection.');
  });
  command('liteagentBridge.rotateToken', async () => {
    await stop();
    await context.secrets.store(secretName, randomBytes(32).toString('base64url'));
    vscode.window.showInformationMessage('Bridge token rotated and bridge stopped. Copy the new token into LiteAgent, then start the bridge again.');
  });
}
async function deactivate() { await stop(); status = undefined; }
module.exports = {activate, deactivate};
