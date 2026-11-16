'use strict';
const {createHash, randomUUID} = require('node:crypto');

class BridgeError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
function modelID(model) {
  const id = encodeURIComponent(model.vendor) + '/' + encodeURIComponent(model.id);
  return id.length <= 200 ? id : 'vscode/' + createHash('sha256').update(id).digest('hex');
}
function text(value, field) {
  if (value === null || value === undefined) return '';
  if (typeof value !== 'string') throw new BridgeError(400, field + ' must be plain text. Images and multipart input are not supported.');
  return value;
}
function requestParts(vscode, body) {
  if (!object(body) || typeof body.model !== 'string' || !Array.isArray(body.messages) || !body.messages.length) throw new BridgeError(400, 'Provide a model and a nonempty messages array.');
  if (body.stream) throw new BridgeError(400, 'This bridge returns complete responses; use stream: false.');
  for (const field of ['temperature', 'max_tokens', 'max_completion_tokens', 'reasoning_effort', 'response_format', 'tool_choice']) {
    if (body[field] !== undefined && body[field] !== null) throw new BridgeError(400, field + ' is not supported by this bridge. Use provider defaults.');
  }
  const messages = [], pending = new Set(), seen = new Set();
  for (const message of body.messages) {
    if (!object(message)) throw new BridgeError(400, 'Invalid message.');
    const content = text(message.content, 'Message content'), parts = [];
    let role;
    if (message.role === 'system' || message.role === 'developer') {
      if (messages.length) throw new BridgeError(400, 'Instructions must precede the conversation.');
      role = vscode.LanguageModelChatMessageRole.User;
      parts.push(new vscode.LanguageModelTextPart('LiteAgent instructions:\n' + content + '\n\n'));
    } else if (message.role === 'user' || message.role === 'assistant') {
      if (pending.size) throw new BridgeError(400, 'Missing tool results before the next message.');
      role = message.role === 'user' ? vscode.LanguageModelChatMessageRole.User : vscode.LanguageModelChatMessageRole.Assistant;
      if (content) parts.push(new vscode.LanguageModelTextPart(content));
      if (message.tool_calls !== undefined) {
        if (message.role !== 'assistant' || !Array.isArray(message.tool_calls)) throw new BridgeError(400, 'Invalid tool calls.');
        for (const call of message.tool_calls) {
          if (!object(call) || call.type !== 'function' || typeof call.id !== 'string' || !call.id || seen.has(call.id) || !object(call.function) || typeof call.function.name !== 'string' || !call.function.name) throw new BridgeError(400, 'Invalid or duplicate tool call.');
          let input;
          try { input = JSON.parse(call.function.arguments); } catch { throw new BridgeError(400, 'Tool arguments must be a JSON object string.'); }
          if (!object(input)) throw new BridgeError(400, 'Tool arguments must be a JSON object.');
          seen.add(call.id); pending.add(call.id);
          parts.push(new vscode.LanguageModelToolCallPart(call.id, call.function.name, input));
        }
      }
    } else if (message.role === 'tool') {
      if (!pending.delete(message.tool_call_id)) throw new BridgeError(400, 'Tool result has no matching pending call.');
      role = vscode.LanguageModelChatMessageRole.User;
      parts.push(new vscode.LanguageModelToolResultPart(message.tool_call_id, [new vscode.LanguageModelTextPart(content)]));
    } else throw new BridgeError(400, 'Unsupported message role.');
    if (!parts.length) parts.push(new vscode.LanguageModelTextPart(''));
    // The public consumer API has user/assistant roles only. Keep ordering while
    // combining adjacent instructions, user text or parallel tool results.
    if (messages.at(-1)?.role === role) messages.at(-1).content.push(...parts);
    else messages.push(new vscode.LanguageModelChatMessage(role, parts));
  }
  if (pending.size) throw new BridgeError(400, 'Every tool call needs a result before requesting another reply.');
  const tools = [], names = new Set();
  if (body.tools !== undefined) {
    if (!Array.isArray(body.tools) || body.tools.length > 128) throw new BridgeError(400, 'Provide at most 128 tools.');
    for (const tool of body.tools) {
      const fn = tool?.function;
      if (tool?.type !== 'function' || !object(fn) || typeof fn.name !== 'string' || !fn.name || names.has(fn.name) || !object(fn.parameters)) throw new BridgeError(400, 'Invalid or duplicate tool definition.');
      names.add(fn.name);
      tools.push({name: fn.name, description: text(fn.description, 'Tool description'), inputSchema: fn.parameters});
    }
  }
  return {messages, tools, names};
}
function cancellable(promise, token) {
  if (token.isCancellationRequested) {
    Promise.resolve(promise).catch(() => {});
    return Promise.reject(new BridgeError(408, 'Model request cancelled or timed out.'));
  }
  return new Promise((resolve, reject) => {
    const subscription = token.onCancellationRequested(() => reject(new BridgeError(408, 'Model request cancelled or timed out.')));
    Promise.resolve(promise).then(resolve, reject).finally(() => subscription.dispose());
  });
}
async function completion(vscode, model, body, token) {
  const {messages, tools, names} = requestParts(vscode, body);
  let tokens = 0;
  for (const message of messages) tokens += await cancellable(model.countTokens(message, token), token);
  if (tools.length) tokens += await cancellable(model.countTokens(JSON.stringify(tools), token), token);
  if (tokens > model.maxInputTokens) throw new BridgeError(400, 'Conversation and tools exceed this model’s input limit. Start a new chat or reduce the context.');
  const options = tools.length ? {tools, toolMode: vscode.LanguageModelChatToolMode.Auto} : {};
  const response = await cancellable(model.sendRequest(messages, options, token), token);
  const iterator = response.stream[Symbol.asyncIterator]();
  let content = '', reasoning = '', bytes = 0, parts = 0, skipped = false;
  const calls = [], seen = new Set();
  const isPart = (part, constructor) => typeof constructor === 'function' && part instanceof constructor;
  const countBytes = size => {
    bytes += size;
    if (bytes > 2 * 1024 * 1024) throw new BridgeError(502, 'The model response exceeded the bridge size limit.');
  };
  const plainText = value => {
    if (typeof value !== 'string') throw new BridgeError(502, 'The model returned invalid text. No partial response was used.');
    countBytes(Buffer.byteLength(value));
    return value;
  };
  while (true) {
    const {value: part, done} = await cancellable(iterator.next(), token);
    if (done) break;
    if (++parts > 65536) throw new BridgeError(502, 'The model response exceeded the bridge part limit.');
    if (part instanceof vscode.LanguageModelTextPart) {
      content += plainText(part.value);
    } else if (part instanceof vscode.LanguageModelToolCallPart) {
      if (!names.has(part.name) || typeof part.callId !== 'string' || !part.callId || seen.has(part.callId) || !object(part.input) || calls.length >= 128) throw new BridgeError(502, 'The model returned an invalid tool call. No tools were executed.');
      seen.add(part.callId);
      const args = JSON.stringify(part.input); countBytes(Buffer.byteLength(args));
      calls.push({id: part.callId, type: 'function', function: {name: part.name, arguments: args}});
    } else if (isPart(part, vscode.LanguageModelThinkingPart)) {
      // Available on some VS Code versions. Only forward the visible value;
      // ids, signatures, encrypted state and metadata stay out of the wire reply.
      for (const value of Array.isArray(part.value) ? part.value : [part.value]) reasoning += plainText(value);
    } else if (isPart(part, vscode.LanguageModelDataPart)) {
      if (!(part.data instanceof Uint8Array)) throw new BridgeError(502, 'The model returned invalid data. No partial response was used.');
      countBytes(part.data.byteLength);
      if (part.mimeType === 'text/plain' || part.mimeType === 'text/markdown') {
        try { content += new TextDecoder('utf-8', {fatal: true}).decode(part.data); }
        catch { throw new BridgeError(502, 'The model returned invalid UTF-8 text data. No partial response was used.'); }
      } else skipped = true;
    } else {
      // The public stream deliberately includes unknown future parts. Do not
      // stringify them, guess text/tool calls from their fields or abort an
      // otherwise valid reply. Still consume to completion so errors propagate.
      skipped = true;
    }
  }
  if (!content && !calls.length) throw new BridgeError(502, skipped || reasoning
    ? 'The model returned no supported answer text or tool calls. Thinking or other data alone cannot complete a reply.'
    : 'The model returned an empty response.');
  const message = {role: 'assistant', content};
  if (reasoning) message.reasoning_content = reasoning;
  if (calls.length) message.tool_calls = calls;
  return {id: 'chatcmpl-' + randomUUID(), object: 'chat.completion', created: Math.floor(Date.now() / 1000), model: body.model, choices: [{index: 0, message, finish_reason: calls.length ? 'tool_calls' : 'stop'}]};
}
module.exports = {BridgeError, modelID, requestParts, cancellable, completion};
