'use strict';
const $ = id => document.getElementById(id);
const el = (tag, text, className) => {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
};
const statusNames = {running: 'Working…', completed: 'Finished', failed: 'Error', cancelled: 'Stopped', limit: 'Limit reached', interrupted: 'Interrupted', timed_out: 'Timed out'};
const toolNames = {browser_open: 'Opening a page', browser_read: 'Reading the page', browser_click: 'Clicking a control', browser_type: 'Entering text', browser_select: 'Selecting an option', browser_tabs: 'Checking tabs', browser_switch: 'Switching tabs', browser_downloads: 'Checking downloads', skills_search: 'Finding a skill', skills_read: 'Loading a skill'};
Object.assign(toolNames, {filesystem_info: 'Checking local folders', filesystem_stat: 'Checking file details', filesystem_list: 'Listing files', filesystem_read: 'Reading a file', filesystem_mkdir: 'Creating a folder', filesystem_write: 'Saving a file', filesystem_edit: 'Editing a file'});
let state = null;
let chatID = new URLSearchParams(location.search).get('chat');
let initialized = false, revision = 0, previousHistory = '', renderedChat;
const renderedTurns = new Map();
const deletingChatIDs = new Set();
let modelSource = '', availableModels = [], modelOptionsSignature = '';
let modelsLoading = false, modelSaving = false, modelsRequest = 0;
let pendingSubmission = null, pendingSchedule = null;
let settingsSource = null;
let scheduleSignature = '';
let commandIndex = 0, commandMatches = [];
const generationFields = ['temperature', 'max_tokens', 'context_tokens', 'history_turns', 'timeout_sec', 'run_timeout_sec', 'max_steps', 'system_prompt'];

function feedback(id, text, error = false) {
  const node = $(id);
  node.textContent = text;
  node.classList.toggle('error', error);
  node.classList.toggle('success', !!text && !error);
}
async function api(path, method = 'GET', body, signal) {
  const response = await fetch('/api/' + path, {
    method, signal, headers: {'Content-Type': 'application/json', 'X-LiteAgent': '1'},
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const value = await response.json();
  if (!response.ok) { const error = new Error(value.error || 'Could not complete the request.'); error.status = response.status; throw error; }
  return value;
}
async function busy(button, label, work) {
  const original = [...button.childNodes], originalLabel = button.getAttribute('aria-label');
  button.disabled = true;
  button.setAttribute('aria-busy', 'true');
  if (button.classList.contains('composer-action')) button.setAttribute('aria-label', label);
  else button.textContent = label;
  try { return await work(); }
  finally {
    button.replaceChildren(...original);
    if (originalLabel !== null) button.setAttribute('aria-label', originalLabel);
    button.disabled = false; button.removeAttribute('aria-busy'); updateControls();
  }
}
function updateControls() {
  const active = !!state?.active;
  $('send').hidden = active && !$('message').value.trim().startsWith('/');
  $('stop').hidden = !active;
  if (!$('send').hasAttribute('aria-busy')) $('send').disabled = !initialized || modelSaving;
  $('reasoning-select').disabled = !initialized || modelSaving || active;
  $('show-settings').disabled = !initialized || modelSaving || active;
  $('model-select').disabled = !initialized || modelsLoading || modelSaving || active;
  $('model-menu-toggle').disabled = !initialized || modelSaving;
  $('refresh-models').disabled = !initialized || modelsLoading || modelSaving;
  $('model-select').title = active ? 'Finish or stop the current reply before switching models.' : state?.config.model || 'Model for all chats';
  const run = state?.runs.find(run => run.id === state.active);
  const lastTool = run?.events.filter(event => event.kind === 'tool').at(-1);
  $('run-status').textContent = active ? (lastTool ? toolNames[lastTool.name] || 'Working…' : 'Thinking…') : '';
}
function providerSource(config) {
  return JSON.stringify([config.provider, config.base_url]);
}
function modelProfile(model, provider) {
  if (provider === 'ollama') {
    switch (model.toLowerCase()) {
      case 'liteagent-quick:0.6b':
      case 'qwen3:0.6b':
        return {
          name: model.toLowerCase() === 'liteagent-quick:0.6b' ? 'Quick · 0.6B' : 'Qwen3 · 0.6B',
          summary: 'Fast replies, limited reasoning',
          label: 'Fast · Basic chat',
          detail: 'Best for short questions, simple rewrites and casual chat. May lose track of context or struggle with complex instructions and tools.',
          speed: 'Usually the quickest option. Loading the model or enabling reasoning can still add a noticeable wait.',
          reasoning: true,
        };
      case 'liteagent-qwen:4b':
      case 'hf.co/unsloth/qwen3-4b-instruct-2507-gguf:q4_k_m':
        return {
          name: model.toLowerCase() === 'liteagent-qwen:4b' ? 'LiteAgent · 4B' : 'Qwen3 Instruct · 4B',
          summary: 'A balance of speed and capability',
          label: 'Balanced · Everyday tasks',
          detail: 'Best for everyday conversation and simple tasks with browser tools. Can struggle with calculations and long, complex workflows.',
          speed: 'A middle ground between 0.6B and 8B. First replies and long conversations can still be slow on CPU.',
        };
      case 'liteagent-bravo:8b':
      case 'qwen3:8b':
        return {
          name: model.toLowerCase() === 'liteagent-bravo:8b' ? 'Bravo · 8B' : 'Qwen3 · 8B',
          summary: 'More capable, with longer waits',
          label: 'More capable · Slower replies',
          detail: 'Best for multi-step instructions, structured answers and tasks with tools. Choose it when answer quality matters more than speed; it can still make mistakes.',
          speed: 'May take several minutes on CPU, especially for the first reply or when reasoning is enabled.',
          reasoning: true,
        };
    }
  }
  return {name: model};
}
function renderModelDescription() {
  const profile = modelProfile(state.config.model, state.config.provider);
  const reasoning = profile.reasoning && !['auto', 'off', 'none'].includes(currentReasoning())
    ? 'Reasoning is on: the model spends extra time working through your request before answering.' : '';
  for (const [id, text] of [['model-summary', profile.summary || ''], ['model-detail', profile.detail || ''], ['model-speed', profile.speed ? 'Response time: ' + profile.speed : ''], ['model-reasoning', reasoning]]) {
    if ($(id).textContent !== text) $(id).textContent = text;
  }
  $('model-reasoning').hidden = !reasoning;
  $('model-description').hidden = !profile.summary;
  $('about-model').hidden = !profile.summary;
  renderComposerModel();
}
function renderModelOptions() {
  if (!state || modelSaving) return;
  renderModelDescription();
  const selected = state.config.model;
  const models = [...new Set([selected, ...availableModels])];
  const signature = JSON.stringify([state.config.provider, selected, models]);
  if (signature === modelOptionsSignature) return;
  modelOptionsSignature = signature;
  const options = models.map(model => {
    const profile = modelProfile(model, state.config.provider);
    const option = el('option', profile.name + (profile.label ? ' — ' + profile.label : ''));
    option.value = model;
    option.title = model;
    return option;
  });
  $('model-select').replaceChildren(...options);
  $('model-select').value = selected;
}
function syncModelPicker() {
  const source = providerSource(state.config);
  if (source !== modelSource) {
    modelSource = source;
    availableModels = [];
    renderModelOptions();
    loadModels();
  } else renderModelOptions();
}
async function loadModels() {
  if (!state) return;
  const source = modelSource, request = ++modelsRequest;
  modelsLoading = true;
  $('refresh-models').setAttribute('aria-busy', 'true');
  feedback('model-feedback', 'Loading models…');
  updateControls();
  try {
    const result = await api('models');
    if (request !== modelsRequest || source !== modelSource) return;
    if (providerSource(result) !== source) {
      await refresh();
      return;
    }
    availableModels = [...new Set(result.models.filter(model => typeof model === 'string' && model.trim()))].sort();
    renderModelOptions();
    feedback('model-feedback', availableModels.length ? '' : 'No models listed. Use /onboard or /model MODEL_ID.');
  } catch (error) {
    if (request === modelsRequest && source === modelSource) feedback('model-feedback', error.message + ' Use Refresh to retry.', true);
  } finally {
    if (request === modelsRequest) {
      modelsLoading = false;
      $('refresh-models').removeAttribute('aria-busy');
      updateControls();
    }
  }
}
async function changeModel() {
  const model = $('model-select').value;
  if (!state || model === state.config.model || modelSaving) return;
  const source = modelSource;
  modelSaving = true;
  revision++;
  feedback('model-feedback', 'Switching model…');
  updateControls();
  try {
    const result = await api('model', 'PUT', {model, provider: state.config.provider, base_url: state.config.base_url});
    revision++;
    if (source === modelSource) {
      state.config.model = result.model;
      feedback('model-feedback', 'Model changed · used for all chats');
    }
  } catch (error) {
    feedback('model-feedback', error.message, true);
  } finally {
    modelSaving = false;
    modelOptionsSignature = '';
    renderModelOptions();
    updateControls();
    await refresh();
  }
}
function selectChat(id) {
  chatID = id;
  revision++;
  pendingSubmission = null;
  $('message').value = '';
  renderCommands();
  const url = new URL(location.href);
  if (id) url.searchParams.set('chat', id); else url.searchParams.delete('chat');
  url.searchParams.delete('run');
  url.hash = '';
  history.replaceState(null, '', url);
  document.body.classList.remove('sidebar-open');
  $('toggle-sidebar').setAttribute('aria-expanded', 'false');
  feedback('chat-feedback', '');
  refresh();
  $('message').focus();
}
function renderHistory() {
  const signature = JSON.stringify([state.chats, chatID, state.active_chat_id, [...deletingChatIDs]]);
  if (signature === previousHistory) return;
  previousHistory = signature;
  const list = $('history-list');
  list.replaceChildren();
  list.removeAttribute('aria-busy');
  if (!state.chats.length) { list.append(el('p', 'Your conversations will appear here.', 'empty')); return; }
  for (const chat of state.chats) {
    const row = el('div', undefined, 'chat-history-row');
    const button = el('button', chat.title, 'history-item');
    button.type = 'button';
    button.title = chat.title;
    button.setAttribute('aria-current', String(chat.id === chatID));
    button.addEventListener('click', () => selectChat(chat.id));
    const remove = el('button', '×', 'delete-chat');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Delete chat: ' + chat.title);
    remove.disabled = deletingChatIDs.has(chat.id);
    remove.textContent = remove.disabled ? '…' : '×';
    remove.title = state.active_chat_id === chat.id ? 'Stop reply and delete chat' : 'Delete chat';
    remove.addEventListener('click', async () => {
      if (deletingChatIDs.has(chat.id)) return;
      const active = state.active_chat_id === chat.id;
      const question = active ? 'Stop this reply and delete its conversation, history and schedules? Files already saved and website actions are kept.'
        : 'Delete this conversation, its history and its schedules? Any reply running in this conversation will be stopped.';
      if (!confirm(question)) return;
      deletingChatIDs.add(chat.id); renderHistory();
      feedback('chat-feedback', active ? 'Stopping the reply and deleting this conversation…' : 'Deleting conversation…');
      try {
        await api('chats/' + chat.id, 'DELETE');
        if (chatID === chat.id) selectChat(null); else await refresh();
        feedback('chat-feedback', 'Conversation deleted.');
      } catch (error) { feedback('chat-feedback', error.message, true); }
      finally { deletingChatIDs.delete(chat.id); renderHistory(); }
    });
    row.append(button, remove);
    list.append(row);
  }
}
function markdown(text) {
  const node = el('div', undefined, 'markdown');
  if (!window.marked || !window.DOMPurify) { node.textContent = text; return node; }
  node.innerHTML = DOMPurify.sanitize(marked.parse(text, {breaks: true}), {
    ALLOWED_TAGS: ['p', 'br', 'strong', 'em', 'del', 'code', 'pre', 'ul', 'ol', 'li', 'blockquote', 'h1', 'h2', 'h3', 'h4', 'hr', 'table', 'thead', 'tbody', 'tr', 'th', 'td', 'a'],
    ALLOWED_ATTR: ['href', 'title', 'start'],
  });
  for (const link of node.querySelectorAll('a')) {
    if (!/^https?:\/\//i.test(link.getAttribute('href') || '')) link.removeAttribute('href');
    else { link.target = '_blank'; link.rel = 'noopener noreferrer'; }
  }
  return node;
}
function trace(run) {
  const actions = run.events.filter(event => event.kind === 'tool');
  if (!actions.length && !run.events.some(event => event.kind === 'error')) return null;
  const details = el('details', undefined, 'action-trace');
  details.dataset.traceId = run.id;
  details.append(el('summary', actions.length + (actions.length === 1 ? ' action' : ' actions')));
  const list = el('ol', undefined, 'events');
  for (const event of run.events) {
    if (event.kind === 'status') continue;
    const row = el('li', undefined, 'event ' + event.kind);
    const header = el('div', undefined, 'event-head');
    const time = el('time', new Date(event.time).toLocaleTimeString('en-US'));
    time.dateTime = event.time;
    header.append(time, el('span', event.name || (event.kind === 'assistant' ? 'Agent' : 'Error')));
    row.append(header);
    if (event.kind === 'tool' || event.kind === 'observation') {
      const observation = el('details');
      observation.append(el('summary', event.kind === 'tool' ? 'Arguments' : 'Observation'));
      let text = event.content;
      try { text = JSON.stringify(JSON.parse(text), null, 2); } catch {}
      observation.append(el('pre', text));
      row.append(observation);
    } else row.append(el('p', event.content));
    list.append(row);
  }
  details.append(list);
  return details;
}
function renderConversation() {
  const container = $('messages'), scroller = $('message-scroll');
  const changedChat = renderedChat !== chatID;
  const nearBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 140;
  if (changedChat) { container.replaceChildren(); renderedTurns.clear(); renderedChat = chatID; }
  const runs = [...state.runs].reverse();
  const liveIDs = new Set(runs.map(run => run.id));
  for (const id of renderedTurns.keys()) {
    if (!liveIDs.has(id)) { document.getElementById('turn-' + id)?.remove(); renderedTurns.delete(id); }
  }
  if (!runs.length) {
    if (!container.querySelector('.welcome')) {
      container.replaceChildren();
      const welcome = el('div', undefined, 'welcome');
      welcome.append(el('h1', 'What can I help with?'), el('p', 'Just ask. Your browser, skills and reminders are here.'));
      const setup = el('button', 'Connect a model', 'text-button'); setup.type='button'; setup.addEventListener('click',()=>showSettings('connections')); welcome.append(setup);
      container.append(welcome);
    }
    return;
  }
  container.querySelector('.welcome')?.remove();
  let changed = changedChat;
  for (const run of runs) {
    const signature = JSON.stringify(run);
    if (renderedTurns.get(run.id) === signature) continue;
    renderedTurns.set(run.id, signature);
    changed = true;
    const existing = document.getElementById('turn-' + run.id);
    const wasOpen = existing?.querySelector('.action-trace')?.open;
    const turn = el('article', undefined, 'turn'); turn.id = 'turn-' + run.id;
    const user = el('div', undefined, 'message user-message');
    user.setAttribute('aria-label', 'You');
    user.append(el('p', run.task));
    const assistant = el('div', undefined, 'message assistant-message');
    assistant.append(el('div', 'LiteAgent', 'speaker'));
    if (run.skill_ids.length) {
      const names = run.skill_ids.map(id => state.skills.find(skill => skill.id === id)?.name).filter(Boolean);
      if (names.length) assistant.append(el('p', 'Skills loaded: ' + names.join(', '), 'loaded-skills'));
    }
    const activity = trace(run);
    if (activity) { activity.open = !!wasOpen; assistant.append(activity); }
    const text = run.result || run.events.filter(event => event.kind === 'assistant').map(event => event.content).join('\n\n');
    if (text) assistant.append(markdown(text));
    if (run.status === 'running') assistant.append(el('p', text ? 'Working…' : 'Thinking…', 'thinking'));
    else if (run.status !== 'completed') assistant.append(el('p', statusNames[run.status], 'run-error'));
    if (run.status === 'completed') {
      const actions = el('div', undefined, 'message-actions');
      const copy = el('button', 'Copy', 'text-button'); copy.type = 'button';
      copy.addEventListener('click', async () => { try { await navigator.clipboard.writeText(run.result); copy.textContent = 'Copied'; } catch { feedback('chat-feedback', 'Copy is unavailable in this browser.', true); } });
      actions.append(copy); assistant.append(actions);
    }
    if(run.origin==='reminder' || run.origin==='scheduled') { assistant.prepend(el('p',run.origin==='reminder'?'Reminder':'Scheduled skill','scheduled-label')); turn.append(assistant); } else turn.append(user, assistant);
    if (existing) existing.replaceWith(turn); else container.append(turn);
  }
  if (changed && (nearBottom || changedChat)) scroller.scrollTop = scroller.scrollHeight;
}
async function refresh() {
  const requestedChat = chatID, request = ++revision;
  try {
    const next = await api('state?chat_id=' + encodeURIComponent(requestedChat || 'new'));
    if (request !== revision || requestedChat !== chatID) return;
    state = next;
    if (chatID && !state.chats.some(chat => chat.id === chatID)) { selectChat(null); return; }
    initialized = true;
    $('connection').textContent = 'Connected locally'; $('connection').classList.remove('error');
    syncModelPicker();
    renderReasoning(); renderSchedules(); renderManagement(); renderStatus(); renderHistory(); renderConversation(); updateControls(); notifyUpdates(state.alerts || []);
  } catch (error) {
    $('connection').textContent = 'Connection lost. Retrying…'; $('connection').classList.add('error');
    feedback('chat-feedback', error.message, true);
  }
}
$('chat-form').addEventListener('submit', async event => {
  event.preventDefault();
  if ($('send').hasAttribute('aria-busy')) return;
  if (modelSaving) return;
  if (!$('message').value.trim()) return;
  if(state?.active && !$('message').value.trim().startsWith('/')) { feedback('chat-feedback','Wait for this reply or press Stop.',true); return; }
  let message = $('message').value;
  const submittedChat = chatID;
  const submittedText = message;
  if (openCommandUI(message.trim())) { $('message').value = ''; renderCommands(); updateControls(); return; }
  feedback('chat-feedback', '');
  await busy($('send'), 'Sending…', async () => {
    try {
      const fingerprint = JSON.stringify([submittedChat, message]);
      if (pendingSubmission?.fingerprint !== fingerprint) pendingSubmission = {fingerprint, id: crypto.randomUUID()};
      const run = await api('chat', 'POST', {message, chat_id: submittedChat || '', request_id: pendingSubmission.id, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone});
      pendingSubmission = null;
      if (chatID !== submittedChat) { await refresh(); return; }
      revision++;
      chatID = run.chat_id;
      const url = new URL(location.href); url.searchParams.set('chat', chatID); history.replaceState(null, '', url);
      if ($('message').value === submittedText) $('message').value = '';
      renderCommands();
      await refresh(); $('message-scroll').scrollTop = $('message-scroll').scrollHeight;
    } catch (error) { feedback('chat-feedback', error.message, true); }
  });
  $('message').focus();
});
$('message').addEventListener('keydown', event => {
  if (handleCommandKey(event)) return;
  if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) { event.preventDefault(); $('chat-form').requestSubmit(); }
});
$('new-chat').addEventListener('click', () => { $('message').value = ''; selectChat(null); });
$('toggle-sidebar').addEventListener('click', () => { const open = document.body.classList.toggle('sidebar-open'); $('toggle-sidebar').setAttribute('aria-expanded', String(open)); });
$('stop').addEventListener('click', async () => { try { await api('stop', 'POST', {}); feedback('chat-feedback', 'Stopping…'); } catch (error) { feedback('chat-feedback', error.message, true); } });
function sendCommand(command) { $('message').value=command; $('chat-form').requestSubmit(); }
$('help').addEventListener('click', () => openPalette());
$('model-select').addEventListener('change', changeModel);
$('refresh-models').addEventListener('click', loadModels);
$('onboard')?.addEventListener('click',()=>showSettings('connections'));
$('message').addEventListener('input', () => { updateControls(); commandIndex = 0; renderCommands(); });
async function poll() { await refresh(); setTimeout(poll, state?.active ? 800 : 3000); }
window.addEventListener('DOMContentLoaded', () => { initExtras(); initManagement(); initNotifications(); poll(); });

const commands = [
  ['/help', 'All commands'], ['/new', 'New conversation'], ['/reset', 'Fresh context; keep history'],
  ['/status', 'Model, activity and local time'], ['/reasoning', 'Set reasoning effort'],
  ['/config', 'Inspect and change settings'], ['/context', 'Inspect conversation context'],
  ['/rename', 'Rename this chat'], ['/retry', 'Repeat the latest request'], ['/stop', 'Stop the current run'],
  ['/cron', 'Create, preview and manage jobs'], ['/crons', 'List scheduled jobs'],
  ['/skills', 'Installed procedures'], ['/models', 'List models'], ['/model', 'Switch model'],
  ['/chats', 'Saved conversations'], ['/resume', 'Resume by ID'], ['/onboard', 'Connect a provider'],
  ['/cancel', 'Cancel setup'], ['/notifications', 'Sounds and desktop alerts'],
];
function renderCommands() {
  const value = $('message').value;
  commandMatches = /^\/\w*$/.test(value) ? commands.filter(([name]) => name.startsWith(value.toLowerCase())) : [];
  $('command-menu').hidden = !commandMatches.length;
  commandIndex = Math.min(commandIndex, Math.max(0, commandMatches.length - 1));
  $('command-menu').replaceChildren(...commandMatches.map(([name, description], index) => {
    const button = el('button', name); button.type = 'button'; button.id = 'command-option-' + index;
    button.setAttribute('role', 'option'); button.setAttribute('aria-selected', String(index === commandIndex));
    button.append(el('span', description)); button.addEventListener('click', () => chooseCommand(index));
    return button;
  }));
  if (commandMatches.length) $('message').setAttribute('aria-activedescendant', 'command-option-' + commandIndex);
  else $('message').removeAttribute('aria-activedescendant');
}
function chooseCommand(index) {
  $('message').value = commandMatches[index][0] + ' '; renderCommands(); updateControls(); $('message').focus();
}
function handleCommandKey(event) {
  if (!commandMatches.length || $('command-menu').hidden || event.isComposing) return false;
  if (event.key === 'Escape') { $('command-menu').hidden = true; $('message').removeAttribute('aria-activedescendant'); return true; }
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault(); commandIndex = (commandIndex + (event.key === 'ArrowDown' ? 1 : -1) + commandMatches.length) % commandMatches.length;
    renderCommands(); $('command-option-' + commandIndex)?.scrollIntoView({block: 'nearest'}); return true;
  }
  if (event.key === 'Tab' || event.key === 'Enter' && !event.shiftKey && $('message').value !== commandMatches[commandIndex][0]) {
    event.preventDefault(); chooseCommand(commandIndex); return true;
  }
  return false;
}
function currentReasoning() {
  return state.config.reasoning || (state.config.provider === 'ollama' && state.config.thinking !== undefined ? (state.config.thinking ? 'on' : 'off') : 'auto');
}
function reasoningLabel(mode) {
  return ({auto: 'Auto', off: 'Off', none: 'None', on: 'On', minimal: 'Minimal', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Max'})[mode] || mode;
}
function renderComposerModel() {
  if (!state) return;
  const name = modelProfile(state.config.model, state.config.provider).name, reasoning = reasoningLabel(currentReasoning());
  $('composer-model').textContent = name;
  $('composer-reasoning').textContent = reasoning;
  $('model-menu-toggle').setAttribute('aria-label', 'Model: ' + name + '. Reasoning: ' + reasoning + '. Change model and reasoning');
}
function setModelMenu(open, returnFocus = false) {
  $('model-menu').hidden = !open;
  $('model-menu-toggle').setAttribute('aria-expanded', String(open));
  if (open) ($('model-select').disabled ? $('close-model-menu') : $('model-select')).focus();
  else if (returnFocus) $('model-menu-toggle').focus();
}
function renderReasoning() {
  if (modelSaving) return;
  const select = $('reasoning-select'), modes = state.reasoning_modes || ['auto'];
  if (select.dataset.modes !== JSON.stringify(modes)) {
    select.replaceChildren(...modes.map(mode => { const option = el('option', mode === 'auto' ? 'Auto (default)' : reasoningLabel(mode)); option.value = mode; return option; }));
    select.dataset.modes = JSON.stringify(modes);
  }
  select.value = currentReasoning();
  select.title = 'Used for all chats. Available levels depend on the model. Auto sends no override.';
  renderComposerModel();
}
function generationSource() { return {provider: state.config.provider, model: state.config.model, base_url: state.config.base_url}; }
async function saveGeneration(settings, source) {
  modelSaving = true; revision++; updateControls();
  try {
    const config = await api('generation', 'PATCH', {...source, settings});
    revision++; state.config = config;
  } finally { modelSaving = false; await refresh(); updateControls(); }
}
function showSettings(tab = 'generation') {
  if (!state) return;
  settingsSource = generationSource();
  for (const field of generationFields) {
    const value = state.config[field];
    $('setting-' + field).value = value ?? '';
  }
  $('setting-context_tokens').disabled = state.config.provider !== 'ollama';
  $('setting-temperature').max = state.config.provider === 'anthropic' ? '1' : '2';
  feedback('settings-feedback', '');
  selectSettingsTab(tab); renderManagement();
  if (!$('settings-dialog').open) $('settings-dialog').showModal();
}
function scheduleFields() {
  const kind = $('schedule-kind').value, repeat = $('schedule-repeat').value;
  $('cron-field').hidden = repeat !== 'cron'; $('schedule-cron').required = repeat === 'cron';
  $('time-field').hidden = repeat === 'cron'; $('schedule-time').required = repeat !== 'cron';
  $('date-field').hidden = repeat !== 'once'; $('schedule-date').required = repeat === 'once';
  $('skill-field').hidden = kind !== 'skill'; $('schedule-skill').required = kind === 'skill';
  $('task-field').hidden = kind === 'reminder'; $('schedule-task').required = kind === 'task';
}
function prepareSchedules() {
  if (!state) return;
  if (!$('schedule-timezone').value) $('schedule-timezone').value = state.config.timezone;
  const selected = $('schedule-skill').value;
  $('schedule-skill').replaceChildren(...state.skills.map(skill => { const option = el('option', skill.name); option.value = skill.id; return option; }));
  if (selected) $('schedule-skill').value = selected;
  scheduleSignature = ''; scheduleFields(); renderSchedules();
}
function renderSchedules() {
  if (!state || $('panel-schedules').hidden) return;
  const filter = $('schedule-filter').value;
  const signature = JSON.stringify([state.schedules, state.active, filter]);
  if (signature === scheduleSignature) return;
  scheduleSignature = signature;
  const list = $('schedule-list'); list.replaceChildren();
  const all = state.schedules || [], active = all.filter(s => s.enabled).length;
  $('schedule-summary').textContent = `${all.length} total · ${active} active · ${all.length - active} inactive`;
  const schedules = all.filter(s => filter === 'all' || filter === 'active' && s.enabled || filter === 'paused' && !s.enabled || filter === 'attention' && s.last_error);
  if (!schedules.length) { list.append(el('p', all.length ? 'No schedules match this filter.' : 'No schedules yet. Ask LiteAgent for a reminder or create one below.', 'empty-state')); return; }
  for (const schedule of schedules) {
    const card = el('article', undefined, 'schedule-card');
    card.append(el('strong', schedule.title));
    const expression = schedule.repeat === 'cron' ? schedule.cron : [schedule.repeat, schedule.date, schedule.time].filter(Boolean).join(' ');
    card.append(el('p', `${schedule.enabled ? 'Active' : 'Paused'} · ${schedule.kind} · ${expression} · ${schedule.timezone}`));
    if (schedule.next_run && schedule.enabled) card.append(el('p', 'Next: ' + new Date(schedule.next_run).toLocaleString(undefined, {timeZone: schedule.timezone}) + ' (' + schedule.timezone + ')'));
    if (schedule.last_status) card.append(el('p', 'Last run: ' + (statusNames[schedule.last_status] || schedule.last_status) + (schedule.last_run ? ' · ' + new Date(schedule.last_run).toLocaleString() : '')));
    if (schedule.last_error) { const error = el('p', schedule.last_error, 'feedback'); error.classList.add('error'); card.append(error); }
    const actions = el('div', undefined, 'schedule-actions');
    for (const [action, label] of [[schedule.enabled ? 'pause' : 'resume', schedule.enabled ? 'Pause' : 'Resume'], ['run', 'Run now'], ['open', 'Open chat'], ['delete', 'Delete']]) {
      const button = el('button', label, 'text-button'); button.type = 'button';
      button.disabled = action === 'run' && !!state.active || ['resume', 'delete'].includes(action) && schedule.last_status === 'running';
      button.addEventListener('click', async () => {
        if (action === 'open') { $('settings-dialog').close(); selectChat(schedule.chat_id); return; }
        if (action === 'delete' && !confirm('Delete schedule “' + schedule.title + '”? Execution history is kept.')) return;
        await busy(button, 'Saving…', async () => {
          try { await api('schedules/' + schedule.id, 'POST', {action}); feedback('schedules-feedback', action === 'run' ? 'Execution started. The next occurrence stays unchanged.' : 'Schedule updated.'); await refresh(); }
          catch (error) { feedback('schedules-feedback', error.message, true); }
        });
      });
      actions.append(button);
    }
    card.append(actions); list.append(card);
  }
}
function initExtras() {
  for (const button of document.querySelectorAll('[data-close]')) button.addEventListener('click', () => $(button.dataset.close).close());
  $('show-settings').addEventListener('click', () => { setModelMenu(false); showSettings(); });
  $('model-menu-toggle').addEventListener('click', () => setModelMenu($('model-menu').hidden));
  $('close-model-menu').addEventListener('click', () => setModelMenu(false, true));
  document.addEventListener('click', event => {
    if (!$('model-menu').hidden && !$('model-menu').contains(event.target) && !$('model-menu-toggle').contains(event.target)) setModelMenu(false);
  });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && !$('model-menu').hidden) { event.preventDefault(); setModelMenu(false, true); }
  });
  document.addEventListener('focusin', event => {
    if (!$('model-menu').hidden && !$('model-menu').contains(event.target) && !$('model-menu-toggle').contains(event.target)) setModelMenu(false);
  });
  $('reasoning-select').addEventListener('change', async () => {
    try { await saveGeneration({reasoning: $('reasoning-select').value}, generationSource()); feedback('model-feedback', 'Reasoning saved · used for all chats'); }
    catch (error) { feedback('model-feedback', error.message, true); renderReasoning(); }
  });
  $('settings-form').addEventListener('submit', async event => {
    event.preventDefault();
    const settings = {};
    for (const field of generationFields) {
      const value = $('setting-' + field).value;
      if (field === 'system_prompt') settings[field] = value;
      else if (field === 'temperature') settings[field] = value === '' ? null : Number(value);
      else settings[field] = Number(value);
    }
    await busy($('save-settings'), 'Saving…', async () => {
      try { await saveGeneration(settings, settingsSource); $('settings-dialog').close(); }
      catch (error) { feedback('settings-feedback', error.message, true); }
    });
  });
  $('show-schedules').addEventListener('click', () => showSettings('schedules'));
  $('schedule-filter').addEventListener('change', renderSchedules);
  $('schedule-kind').addEventListener('change', scheduleFields); $('schedule-repeat').addEventListener('change', scheduleFields);
  $('schedule-form').addEventListener('submit', async event => {
    event.preventDefault();
    const kind = $('schedule-kind').value, repeat = $('schedule-repeat').value;
    const input = {chat_id: chatID || '', title: $('schedule-title').value, kind, repeat, timezone: $('schedule-timezone').value,
      cron: repeat === 'cron' ? $('schedule-cron').value : '', time: repeat === 'cron' ? '' : $('schedule-time').value,
      date: repeat === 'once' ? $('schedule-date').value : '', skill_id: kind === 'skill' ? $('schedule-skill').value : '',
      task: kind !== 'reminder' ? $('schedule-task').value : ''};
    await busy($('save-schedule'), 'Creating…', async () => {
      try {
        const fingerprint = JSON.stringify(input);
        if (pendingSchedule?.fingerprint !== fingerprint) pendingSchedule = {fingerprint, id: crypto.randomUUID()};
        await api('schedules', 'POST', {...input, request_id: pendingSchedule.id}); pendingSchedule = null; $('schedule-title').value = ''; $('schedule-task').value = ''; $('schedule-editor').open = false;
        feedback('schedules-feedback', 'Schedule created.'); await refresh();
      } catch (error) { feedback('schedules-feedback', error.message, true); }
    });
  });
}
