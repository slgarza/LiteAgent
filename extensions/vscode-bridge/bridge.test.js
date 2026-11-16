'use strict';
const {test} = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const {startBridge} = require('./bridge');
const {modelID, requestParts} = require('./protocol');

class CancellationTokenSource {
  constructor() {
    this.listeners = new Set();
    this.token = {isCancellationRequested: false, onCancellationRequested: fn => { this.listeners.add(fn); return {dispose: () => this.listeners.delete(fn)}; }};
  }
  cancel() { if (this.token.isCancellationRequested) return; this.token.isCancellationRequested = true; for (const fn of this.listeners) fn(); }
  dispose() { this.listeners.clear(); }
}
class LanguageModelTextPart { constructor(value) { this.value = value; } }
class LanguageModelToolCallPart { constructor(callId, name, input) { Object.assign(this, {callId, name, input}); } }
class LanguageModelToolResultPart { constructor(callId, content) { Object.assign(this, {callId, content}); } }
class LanguageModelThinkingPart { constructor(value, id, metadata) { Object.assign(this, {value, id, metadata}); } }
class LanguageModelDataPart { constructor(data, mimeType) { Object.assign(this, {data, mimeType}); } }
class LanguageModelChatMessage { constructor(role, content) { Object.assign(this, {role, content}); } }
const api = {CancellationTokenSource, LanguageModelTextPart, LanguageModelToolCallPart, LanguageModelToolResultPart, LanguageModelThinkingPart, LanguageModelDataPart, LanguageModelChatMessage, LanguageModelChatMessageRole: {User: 1, Assistant: 2}, LanguageModelChatToolMode: {Auto: 1}};
const token = 'fixture-bridge-token-no-account';
const tools = [{type: 'function', function: {name: 'filesystem_read', description: 'Read a file', parameters: {type: 'object', properties: {path: {type: 'string'}}, required: ['path']}}}];
function model(overrides = {}) { return {vendor: 'fixture', id: 'text-model', name: 'Fixture model', maxInputTokens: 1000, countTokens: async () => 1, sendRequest: async () => ({stream: (async function* () { yield new LanguageModelTextPart('Hello'); })()}), ...overrides}; }
async function fixture(t, models, options = {}) {
  const {apiOverrides, ...bridgeOptions} = options;
  const vscode = {...api, ...apiOverrides, lm: {selectChatModels: async () => typeof models === 'function' ? models() : models}};
  const bridge = await startBridge(vscode, {token, port: 0, canSend: () => true, ...bridgeOptions});
  t.after(() => bridge.close());
  const request = async (path = '/models', body, headers = {}) => {
    const response = await fetch(bridge.url + path, {method: body === undefined ? 'GET' : 'POST', headers: {Authorization: 'Bearer ' + token, 'Content-Type': 'application/json', ...headers}, body: body === undefined ? undefined : JSON.stringify(body)});
    return {status: response.status, body: await response.json()};
  };
  return {bridge, request};
}
test('catalog is live, vendor-qualified and authenticated without inference', async t => {
  let entries = [model({sendRequest() { assert.fail('discovery invoked inference'); }})];
  const {request, bridge} = await fixture(t, () => entries);
  assert.equal((await request('/models', undefined, {Authorization: ''})).status, 401);
  assert.equal((await request('/models', undefined, {Origin: 'https://example.com'})).status, 403);
  const rejectedHost = await new Promise((resolve, reject) => {
    http.get(bridge.url + '/models', {headers: {Host: 'attacker.example', Authorization: 'Bearer ' + token}}, response => { response.resume(); resolve(response.statusCode); }).on('error', reject);
  });
  assert.equal(rejectedHost, 403);
  assert.equal((await request('/models')).body.data[0].id, 'fixture/text-model');
  entries = [model({vendor: 'another'}), model()];
  assert.deepEqual((await request('/models')).body.data.map(m => m.id), ['another/text-model', 'fixture/text-model']);
  entries = [];
  assert.deepEqual((await request('/models')).body.data, []);
  assert.ok(modelID(model({id: 'x'.repeat(300)})).length <= 200);
});
test('tool calls and results round trip without the bridge executing tools', async t => {
  let step = 0;
  const selected = model({sendRequest: async (messages, options) => {
    assert.equal(messages[0].role, 1);
    assert.equal(messages[0].content[0].value, 'LiteAgent instructions:\nFollow instructions.\n\n');
    assert.equal(options.tools[0].inputSchema.type, 'object');
    if (step++ === 0) return {stream: (async function* () {
      yield new LanguageModelTextPart('Reading.');
      yield new LanguageModelToolCallPart('call-1', 'filesystem_read', {path: '~/report.txt'});
    })()};
    assert.equal(messages[1].role, 2);
    assert.deepEqual(messages[1].content[1].input, {path: '~/report.txt'});
    assert.equal(messages[2].content[0].callId, 'call-1');
    assert.equal(messages[2].content[0].content[0].value, 'Report contents');
    return {stream: (async function* () { yield new LanguageModelTextPart('Done.'); })()};
  }});
  const {request} = await fixture(t, [selected]);
  const body = {model: modelID(selected), tools, stream: false, messages: [{role: 'system', content: 'Follow instructions.'}, {role: 'user', content: 'Read the report.'}]};
  const first = await request('/chat/completions', body);
  assert.equal(first.status, 200);
  assert.equal(first.body.choices[0].finish_reason, 'tool_calls');
  assert.equal(first.body.choices[0].message.tool_calls[0].function.arguments, '{"path":"~/report.txt"}');
  body.messages.push(first.body.choices[0].message, {role: 'tool', tool_call_id: 'call-1', content: 'Report contents'});
  const second = await request('/chat/completions', body);
  assert.equal(second.status, 200);
  assert.equal(second.body.choices[0].message.content, 'Done.');
});
test('permissions, quota, missing model and provider errors stay explicit and private', async t => {
  const selected = model();
  let allowed = false;
  const {request} = await fixture(t, [selected], {canSend: () => allowed});
  const body = {model: modelID(selected), messages: [{role: 'user', content: 'Hello'}]};
  assert.equal((await request('/chat/completions', body)).status, 403);
  allowed = true;
  for (const [code, status] of [['Blocked', 429], ['NoPermissions', 403], ['NotFound', 404], ['Other', 502]]) {
    selected.sendRequest = async () => { const error = new Error('private provider details'); error.code = code; throw error; };
    const result = await request('/chat/completions', body);
    assert.equal(result.status, status);
    assert.ok(!JSON.stringify(result).includes('private provider details'));
  }
  assert.equal((await request('/chat/completions', {...body, model: 'missing'})).status, 404);
});
test('unsupported options and oversized context are rejected before inference', async t => {
  const selected = model({sendRequest() { assert.fail('invalid request reached inference'); }});
  const {request} = await fixture(t, [selected]);
  const body = {model: modelID(selected), messages: [{role: 'user', content: 'Hello'}]};
  for (const override of [{stream: true}, {temperature: 0.2}, {max_completion_tokens: 100}, {reasoning_effort: 'high'}, {messages: [{role: 'user', content: [{type: 'image'}]}]}]) {
    assert.equal((await request('/chat/completions', {...body, ...override})).status, 400);
  }
  selected.maxInputTokens = 0;
  assert.equal((await request('/chat/completions', body)).status, 400);
});
test('no partial tool calls escape a broken response stream', async t => {
  const selected = model({sendRequest: async () => ({stream: (async function* () {
    yield new LanguageModelToolCallPart('call-1', 'filesystem_read', {});
    throw new Error('stream interrupted');
  })()})});
  const {request} = await fixture(t, [selected]);
  const result = await request('/chat/completions', {model: modelID(selected), tools, messages: [{role: 'user', content: 'Read'}]});
  assert.equal(result.status, 502);
  assert.ok(!result.body.choices);
});
test('thinking and additional response parts do not discard valid text or tool calls', async t => {
  const opaque = {signature: 'fixture-private-signature', encrypted: 'fixture-private-state', _completeThinking: 'fixture-private-metadata'};
  const unknown = {get value() { assert.fail('unknown part was inspected as text'); }, toJSON() { assert.fail('unknown part was serialized'); }};
  let requests = 0;
  const selected = model({sendRequest: async messages => {
    requests++;
    if (requests === 2) {
      assert.ok(!JSON.stringify(messages).includes('fixture-private'));
      assert.ok(messages.some(m => m.content.some(p => p instanceof LanguageModelToolResultPart)));
      return {stream: (async function* () { yield new LanguageModelTextPart('Done.'); })()};
    }
    return {stream: (async function* () {
      yield new LanguageModelThinkingPart('Checking ', 'fixture-private-id', opaque);
      yield new LanguageModelThinkingPart(['the ', 'report.']);
      yield new LanguageModelThinkingPart('', undefined, opaque);
      yield unknown;
      yield new LanguageModelDataPart(Buffer.from('fixture-private-image'), 'image/png');
      yield new LanguageModelDataPart(Buffer.from('ephemeral'), 'cache_control');
      yield new LanguageModelTextPart('Reading ');
      yield new LanguageModelDataPart(Buffer.from('**the report**'), 'text/markdown');
      yield new LanguageModelDataPart(Buffer.from(' 🌎.'), 'text/plain');
      yield new LanguageModelToolCallPart('call-1', 'filesystem_read', {path: '~/report.txt'});
    })()};
  }});
  const {request} = await fixture(t, [selected]);
  const body = {model: modelID(selected), tools, messages: [{role: 'user', content: 'Read'}]};
  const result = await request('/chat/completions', body);
  assert.equal(result.status, 200, JSON.stringify(result));
  const choice = result.body.choices[0];
  assert.equal(choice.message.content, 'Reading **the report** 🌎.');
  assert.equal(choice.message.reasoning_content, 'Checking the report.');
  assert.equal(choice.finish_reason, 'tool_calls');
  assert.equal(choice.message.tool_calls[0].function.arguments, '{"path":"~/report.txt"}');
  assert.ok(!JSON.stringify(result).includes('fixture-private'));
  assert.equal(requests, 1, 'bridge must not retry inference');
  body.messages.push(choice.message, {role: 'tool', tool_call_id: 'call-1', content: 'Report contents'});
  assert.equal((await request('/chat/completions', body)).body.choices[0].message.content, 'Done.');
});
test('older APIs without optional part constructors still accept valid responses', async t => {
  const selected = model({sendRequest: async () => ({stream: (async function* () {
    yield new LanguageModelThinkingPart('not exposed by this API');
    yield new LanguageModelDataPart(Buffer.from('metadata'), 'application/json');
    yield {value: 'not a native text part', name: 'filesystem_read', callId: 'not-a-native-call', input: {}};
    yield null;
    yield new LanguageModelTextPart('OK');
    yield new LanguageModelToolCallPart('native-call', 'filesystem_read', {});
  })()})});
  const {request} = await fixture(t, [selected], {apiOverrides: {LanguageModelThinkingPart: undefined, LanguageModelDataPart: undefined}});
  const result = await request('/chat/completions', {model: modelID(selected), tools, messages: [{role: 'user', content: 'Read'}]});
  assert.equal(result.status, 200);
  assert.equal(result.body.choices[0].message.content, 'OK');
  assert.equal(result.body.choices[0].message.reasoning_content, undefined);
  assert.equal(result.body.choices[0].message.tool_calls.length, 1);
});
test('thinking, images or unknown data alone are not a completed answer', async t => {
  const selected = model();
  const {request} = await fixture(t, [selected]);
  for (const part of [new LanguageModelThinkingPart('fixture-private-thought'), new LanguageModelDataPart(Buffer.from('fixture-private-image'), 'image/png'), {value: 'fixture-private-unknown'}]) {
    selected.sendRequest = async () => ({stream: (async function* () { yield part; })()});
    const result = await request('/chat/completions', {model: modelID(selected), messages: [{role: 'user', content: 'Hello'}]});
    assert.equal(result.status, 502);
    assert.match(result.body.error.message, /no supported answer/);
    assert.ok(!result.body.choices);
    assert.ok(!JSON.stringify(result).includes('fixture-private'));
  }
});
test('a failure after auxiliary parts still discards every partial tool call', async t => {
  let requests = 0;
  const selected = model({sendRequest: async () => {
    requests++;
    return {stream: (async function* () {
      yield new LanguageModelThinkingPart('A visible progress note.');
      yield {metadata: 'fixture-private-metadata'};
      yield new LanguageModelToolCallPart('call-1', 'filesystem_read', {});
      yield new LanguageModelTextPart('Partial answer');
      throw new Error('fixture-private-stream-error');
    })()};
  }});
  const {request} = await fixture(t, [selected]);
  const result = await request('/chat/completions', {model: modelID(selected), tools, messages: [{role: 'user', content: 'Read'}]});
  assert.equal(result.status, 502);
  assert.ok(!result.body.choices);
  assert.ok(!JSON.stringify(result).includes('fixture-private'));
  assert.equal(requests, 1);
});
test('mixed responses retain size limits and text/tool validation', async t => {
  const selected = model();
  const {request} = await fixture(t, [selected]);
  const parts = [
    new LanguageModelThinkingPart('x'.repeat(2 * 1024 * 1024 + 1)),
    new LanguageModelDataPart(new Uint8Array(2 * 1024 * 1024 + 1), 'image/png'),
    new LanguageModelDataPart(new Uint8Array([0xff]), 'text/plain'),
    new LanguageModelDataPart('invalid data', 'text/plain'),
    new LanguageModelThinkingPart(['valid', 42]),
    new LanguageModelTextPart({value: 'invalid text'}),
    new LanguageModelToolCallPart('bad', 'unregistered_tool', {}),
    new LanguageModelToolCallPart('bad', 'filesystem_read', [])
  ];
  for (const part of parts) {
    selected.sendRequest = async () => ({stream: (async function* () { yield new LanguageModelTextPart('Partial'); yield part; })()});
    const result = await request('/chat/completions', {model: modelID(selected), tools, messages: [{role: 'user', content: 'Read'}]});
    assert.equal(result.status, 502);
    assert.ok(!result.body.choices);
  }
  selected.sendRequest = async () => ({stream: (async function* () { for (let i = 0; i < 65537; i++) yield {}; })()});
  const result = await request('/chat/completions', {model: modelID(selected), messages: [{role: 'user', content: 'Read'}]});
  assert.equal(result.status, 502);
  assert.match(result.body.error.message, /part limit/);
});
test('inconsistent tool history is rejected', () => {
  const body = {model: 'fixture/text-model', messages: [{role: 'assistant', tool_calls: [{id: 'x', type: 'function', function: {name: 'test', arguments: '{}'}}]}]};
  assert.throws(() => requestParts(api, body), /Every tool call needs a result/);
  body.messages.push({role: 'tool', tool_call_id: 'wrong', content: ''});
  assert.throws(() => requestParts(api, body), /no matching pending call/);
});
test('timeout cancels a nonresponsive model', async t => {
  let cancelled = false;
  const selected = model({sendRequest: (_messages, _options, cancellation) => {
    cancellation.onCancellationRequested(() => { cancelled = true; });
    return new Promise(() => {});
  }});
  const {request} = await fixture(t, [selected], {timeoutMs: 40});
  const result = await request('/chat/completions', {model: modelID(selected), messages: [{role: 'user', content: 'Wait'}]});
  assert.equal(result.status, 408);
  assert.equal(cancelled, true);
});
test('disconnect and bridge shutdown cancel outstanding requests', async t => {
  for (const action of ['disconnect', 'shutdown']) {
    let started, cancelled;
    const entered = new Promise(resolve => { started = resolve; });
    const stopped = new Promise(resolve => { cancelled = resolve; });
    const selected = model({sendRequest: (_messages, _options, cancellation) => {
      cancellation.onCancellationRequested(cancelled); started();
      return new Promise(() => {});
    }});
    const {bridge} = await fixture(t, [selected]);
    const abort = new AbortController();
    const request = fetch(bridge.url + '/chat/completions', {method: 'POST', headers: {Authorization: 'Bearer ' + token, 'Content-Type': 'application/json'}, body: JSON.stringify({model: modelID(selected), messages: [{role: 'user', content: 'Wait'}]}), signal: abort.signal}).catch(() => {});
    await entered;
    if (action === 'disconnect') abort.abort(); else await bridge.close();
    await Promise.race([stopped, new Promise((_resolve, reject) => { const timer = setTimeout(() => reject(new Error('request was not cancelled')), 1000); timer.unref(); })]);
    await request;
  }
});
