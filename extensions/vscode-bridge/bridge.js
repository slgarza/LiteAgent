'use strict';
const http = require('node:http');
const {timingSafeEqual} = require('node:crypto');
const {BridgeError, modelID, cancellable, completion} = require('./protocol');

function reply(res, status, value) {
  if (res.destroyed) return;
  res.writeHead(status, {'Content-Type': 'application/json', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff'});
  res.end(JSON.stringify(value));
}
function errorResponse(error) {
  if (error instanceof BridgeError) return {status: error.status, message: error.message};
  // Provider errors may contain user content or credentials. Report the category.
  if (error?.code === 'NoPermissions') return {status: 403, message: 'Authorize this model from VS Code using LiteAgent: Authorize and Test a Model.'};
  if (error?.code === 'NotFound') return {status: 404, message: 'This model is no longer available in VS Code. Refresh models.'};
  if (error?.code === 'Blocked') return {status: 429, message: 'VS Code blocked the request. Check the provider’s quota and access status in VS Code.'};
  return {status: 502, message: 'The VS Code model request failed. Check model access, tool support and provider status in VS Code.'};
}
async function readBody(req) {
  const chunks = []; let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > 2 * 1024 * 1024) throw new BridgeError(413, 'Request exceeds the 2 MiB bridge limit.');
    chunks.push(chunk);
  }
  try { return JSON.parse(Buffer.concat(chunks).toString('utf8')); }
  catch { throw new BridgeError(400, 'Request body must contain JSON.'); }
}
async function startBridge(vscode, {token, port = 8788, timeoutMs = 900000, canSend = () => false}) {
  if (typeof token !== 'string' || token.length < 24) throw new Error('A bridge token is required.');
  const expected = Buffer.from('Bearer ' + token), running = new Set();
  let closing = false;
  const server = http.createServer(async (req, res) => {
    let source, timer;
    try {
      const host = req.headers.host;
      const actualPort = server.address().port;
      if (![ `127.0.0.1:${actualPort}`, `localhost:${actualPort}` ].includes(host) || req.headers.origin || req.headers['sec-fetch-site'] === 'cross-site') throw new BridgeError(403, 'Only local application requests are accepted.');
      const supplied = Buffer.from(req.headers.authorization || '');
      if (supplied.length !== expected.length || !timingSafeEqual(supplied, expected)) throw new BridgeError(401, 'Paste the bridge token from VS Code into the LiteAgent connection.');
      if (closing) throw new BridgeError(503, 'The VS Code bridge is stopping.');
      if (req.url !== '/v1/models' && req.url !== '/v1/chat/completions') throw new BridgeError(404, 'Unknown bridge endpoint.');
      if (req.method !== (req.url === '/v1/models' ? 'GET' : 'POST')) throw new BridgeError(405, 'Method not allowed.');
      if (running.size >= 8) throw new BridgeError(429, 'Too many bridge requests. Try again when the active requests finish.');
      source = new vscode.CancellationTokenSource(); running.add(source);
      const abort = () => source.cancel();
      req.on('aborted', abort);
      res.on('close', () => { if (!res.writableEnded) abort(); });
      timer = setTimeout(abort, req.url === '/v1/models' ? Math.min(10000, timeoutMs) : timeoutMs);
      const models = await cancellable(vscode.lm.selectChatModels(), source.token);
      if (req.url === '/v1/models') {
        reply(res, 200, {object: 'list', data: models.map(model => ({id: modelID(model), object: 'model', name: model.name, owned_by: model.vendor}))});
      } else {
        if (req.headers['content-type']?.split(';')[0].trim().toLowerCase() !== 'application/json') throw new BridgeError(415, 'Use Content-Type: application/json.');
        const body = await cancellable(readBody(req), source.token);
        const model = models.find(model => modelID(model) === body?.model);
        if (!model) throw new BridgeError(404, 'The selected model is not available in VS Code. Refresh models in LiteAgent.');
        if (!canSend(model)) throw new BridgeError(403, 'Run LiteAgent: Authorize and Test a Model in VS Code before using this model.');
        const result = await completion(vscode, model, body, source.token);
        if (Buffer.byteLength(JSON.stringify(result)) > 2 * 1024 * 1024) throw new BridgeError(502, 'The encoded response exceeded the bridge size limit.');
        reply(res, 200, result);
      }
    } catch (error) {
      const {status, message} = errorResponse(error);
      reply(res, status, {error: {message, type: 'bridge_error'}});
    } finally {
      clearTimeout(timer);
      if (source) { source.cancel(); source.dispose(); running.delete(source); }
    }
  });
  server.requestTimeout = 30000; server.headersTimeout = 10000; server.keepAliveTimeout = 5000;
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, '127.0.0.1', () => { server.removeListener('error', reject); resolve(); });
  });
  return {
    url: 'http://127.0.0.1:' + server.address().port + '/v1',
    async close() {
      closing = true;
      for (const source of running) source.cancel();
      await new Promise(resolve => { server.close(resolve); server.closeAllConnections(); });
    }
  };
}
module.exports = {startBridge, errorResponse};
