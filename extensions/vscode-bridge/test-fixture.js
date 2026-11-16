// Loaded only by scripts/test-vscode-bridge.py in an isolated extension host.
'use strict';
const vscode = require('vscode');
function activate(context) {
  const events = {requests: 0, cancelled: false, thinking: false, data: false};
  context.subscriptions.push(vscode.lm.registerLanguageModelChatProvider('liteagent-fixture', {
    provideLanguageModelChatInformation() { return [{id: 'fixture-model', name: 'LiteAgent test model', family: 'fixture', version: '1', maxInputTokens: 8192, maxOutputTokens: 2048, capabilities: {toolCalling: true}}]; },
    provideTokenCount() { return 1; },
    async provideLanguageModelChatResponse(_model, messages, options, progress, token) {
      events.requests++;
      if (messages.some(m => m.content.some(p => p.value === 'Wait for cancellation'))) {
        await new Promise(resolve => token.onCancellationRequested(() => { events.cancelled = true; resolve(); })); return;
      }
      if (typeof vscode.LanguageModelThinkingPart === 'function') {
        progress.report(new vscode.LanguageModelThinkingPart(['Checking ', 'the fixture.'], 'fixture-private-id', {signature: 'fixture-private-signature', encrypted: 'fixture-private-state'}));
        events.thinking = true;
      }
      if (typeof vscode.LanguageModelDataPart === 'function') {
        progress.report(new vscode.LanguageModelDataPart(new TextEncoder().encode('fixture-private-metadata'), 'application/vnd.liteagent.fixture-metadata'));
        events.data = true;
      }
      const result = messages.some(m => m.content.some(p => p instanceof vscode.LanguageModelToolResultPart));
      if (!result && options.tools?.length) progress.report(new vscode.LanguageModelToolCallPart('fixture-call', options.tools[0].name, {path: '~/report.txt'}));
      else {
        const reply = result ? 'Read the fixture report.' : 'OK';
        progress.report(typeof vscode.LanguageModelDataPart === 'function'
          ? new vscode.LanguageModelDataPart(new TextEncoder().encode(reply), 'text/plain')
          : new vscode.LanguageModelTextPart(reply));
      }
    }
  }));
  return events;
}
module.exports = {activate};
