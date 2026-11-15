'use strict';
const assert = require('node:assert/strict');
const vscode = require('vscode');
const {startBridge} = require('./bridge');

async function runTests() {
  const events = await vscode.extensions.getExtension('liteagent-test.fixture').activate();
  const extension = vscode.extensions.getExtension('slgarza.liteagent-vscode-bridge');
  await extension.activate();
  const commands = await vscode.commands.getCommands();
  assert.ok(commands.includes('liteagentBridge.start') && commands.includes('liteagentBridge.authorize'));
  const token = 'isolated-extension-host-test-token';
  const bridge = await startBridge(vscode, {token, port: 0, canSend: model => model.vendor === 'liteagent-fixture'});
  const headers = {Authorization: 'Bearer ' + token, 'Content-Type': 'application/json'};
  try {
    const catalog = await (await fetch(bridge.url + '/models', {headers})).json();
    const model = catalog.data.find(m => m.owned_by === 'liteagent-fixture');
    assert.ok(model, 'fixture model was not discovered by the native VS Code API');
    const messages = [{role: 'system', content: 'Read files using tools.'}, {role: 'user', content: 'Read the report.'}];
    const tools = [{type: 'function', function: {name: 'filesystem_read', description: 'Read a fixture', parameters: {type: 'object', properties: {path: {type: 'string'}}}}}];
    const post = async () => {
      const response = await fetch(bridge.url + '/chat/completions', {method: 'POST', headers, body: JSON.stringify({model: model.id, messages, tools})});
      const result = await response.json();
      assert.equal(response.status, 200, JSON.stringify(result));
      return result.choices[0].message;
    };
    const first = await post();
    assert.equal(first.tool_calls[0].id, 'fixture-call');
    messages.push(first, {role: 'tool', tool_call_id: 'fixture-call', content: 'Test report text'});
    assert.equal((await post()).content, 'Read the fixture report.');
    const abort = new AbortController();
    const request = fetch(bridge.url + '/chat/completions', {method: 'POST', headers, body: JSON.stringify({model: model.id, messages: [{role: 'user', content: 'Wait for cancellation'}]}), signal: abort.signal}).catch(() => {});
    const deadline = Date.now() + 5000;
    while (events.requests < 3 && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(events.requests, 3);
    abort.abort(); await request;
    while (!events.cancelled && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 20));
    assert.ok(events.cancelled, 'native model cancellation was not delivered');
    console.log('VS Code host passed: activation, native model discovery, tool round trip and cancellation. No account or live inference used.');
  } finally { await bridge.close(); }
}
async function run() {
  await runTests();
  if (process.env.LITEAGENT_HOST_TEST_RESULT) require('node:fs').writeFileSync(process.env.LITEAGENT_HOST_TEST_RESULT, 'passed');
}
module.exports = {run};
