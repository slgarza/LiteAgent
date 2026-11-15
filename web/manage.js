'use strict';
let managementSignature = '', paletteIndex = 0, paletteMatches = [];
let connectionModels = [], connectionModelRequest = 0, connectionModelAbort = null, connectionModelTimer = null, connectionManualModel = false;
function selectSettingsTab(tab) {
  for (const button of document.querySelectorAll('[data-settings-tab]')) {
    const selected = button.dataset.settingsTab === tab;
    button.setAttribute('aria-current', String(selected));
    $('panel-' + button.dataset.settingsTab).hidden = !selected;
    if (selected && $('settings-dialog').open) button.scrollIntoView({block: 'nearest', inline: 'nearest'});
  }
  if (tab === 'schedules') prepareSchedules();
  if (tab === 'notifications') renderNotificationSettings();
}
function openCommandUI(command) {
  if (!state) return false;
  if (command === '/help') { openPalette(); return true; }
  if (command === '/config') { showSettings(); return true; }
  if (command === '/skills') { showSettings('skills'); return true; }
  if (command === '/notifications') { showSettings('notifications'); return true; }
  if (command === '/models' || command === '/onboard') { showSettings('connections'); return true; }
  if (['/cron', '/crons', '/schedules'].includes(command)) { $('show-schedules').click(); return true; }
  if (command === '/status') { renderStatus(); $('status-dialog').showModal(); return true; }
  return false;
}
function openPalette() {
  $('command-search').value = ''; paletteIndex = 0; renderPalette(); $('commands-dialog').showModal(); $('command-search').focus();
}
function renderPalette() {
  const query = $('command-search').value.toLowerCase().trim();
  paletteMatches = commands.filter(([name, detail]) => (name + ' ' + detail).toLowerCase().includes(query));
  paletteIndex = Math.min(paletteIndex, Math.max(0, paletteMatches.length - 1));
  $('palette-list').replaceChildren(...paletteMatches.map(([name, detail], index) => {
    const button = el('button', undefined, 'palette-item'); button.type = 'button'; button.id = 'palette-' + index;
    button.setAttribute('role', 'option'); button.setAttribute('aria-selected', String(index === paletteIndex));
    button.append(el('code', name), el('span', detail), el('span', '↵', 'palette-enter'));
    button.addEventListener('click', () => choosePalette(index)); return button;
  }));
  if (!paletteMatches.length) $('palette-list').append(el('p', 'No matching commands.', 'empty'));
  if (paletteMatches.length) $('command-search').setAttribute('aria-activedescendant', 'palette-' + paletteIndex);
  else $('command-search').removeAttribute('aria-activedescendant');
}
function choosePalette(index) {
  const name = paletteMatches[index]?.[0]; if (!name) return;
  $('commands-dialog').close();
  if (name === '/help') { openPalette(); return; }
  if (openCommandUI(name)) return;
  if (name === '/new') { selectChat(null); return; }
  $('message').value = name + ' '; renderCommands(); updateControls(); $('message').focus();
}
function renderStatus() {
  if (!state) return;
  const values = [
    ['Activity', state.active ? 'Running · ' + state.active : 'Idle'], ['Provider', state.config.provider],
    ['Model', state.config.model], ['Reasoning', currentReasoning()], ['Skills', String(state.skills.length)],
    ['MCP servers', String((state.mcp_servers || []).filter(s => s.enabled).length) + ' enabled'],
    ['Schedules', String((state.schedules || []).filter(s => s.enabled).length) + ' active'], ['Time zone', state.config.timezone],
  ];
  $('status-content').replaceChildren(...values.map(([label, value]) => {
    const row = el('div', undefined, 'status-row'); row.append(el('span', label), el('strong', value)); return row;
  }));
}
function managementCard(name, description, badge) {
  const card = el('article', undefined, 'management-card');
  const heading = el('div', undefined, 'card-heading'); heading.append(el('strong', name));
  if (badge) heading.append(el('span', badge, 'badge'));
  card.append(heading, el('p', description, 'muted'));
  const actions = el('div', undefined, 'card-actions'); card.append(actions);
  return {card, actions};
}
function cardAction(actions, label, work, disabled = false) {
  const button = el('button', label, 'text-button'); button.type = 'button'; button.disabled = disabled;
  button.addEventListener('click', () => work(button)); actions.append(button);
}
async function managedAction(button, feedbackID, work) {
  await busy(button, 'Working…', async () => {
    try { await work(); await refresh(); }
    catch (error) { feedback(feedbackID, error.message, true); }
  });
}
function renderManagement() {
  if (!state || !$('settings-dialog').open && !document.querySelector('[data-settings-tab][aria-current="true"]')) return;
  const signature = JSON.stringify([state.connections, state.skills, state.mcp_servers, state.config.connection_id, state.active]);
  if (signature === managementSignature) return;
  managementSignature = signature;
  for (const id of ['connections-list', 'skills-list', 'mcp-list']) $(id).replaceChildren();
  for (const connection of state.connections || []) {
    const active = state.config.connection_id === connection.id;
    const {card, actions} = managementCard(connection.name, connection.model + ' · ' + connection.base_url, active ? 'In use' : connection.key_set ? 'Key saved' : connection.provider);
    cardAction(actions, 'Edit', () => editConnection(connection));
    cardAction(actions, 'Test', button => managedAction(button, 'connections-feedback', async () => {
      const result = await api('connections/' + connection.id, 'POST', {action: 'test'});
      feedback('connections-feedback', 'Connected · ' + result.models.length + ' models listed.');
    }));
    cardAction(actions, 'Use', button => managedAction(button, 'connections-feedback', async () => {
      await api('connections/' + connection.id, 'POST', {action: 'activate'}); feedback('connections-feedback', connection.name + ' is now in use.');
    }), active || !!state.active);
    cardAction(actions, 'Remove', button => {
      if (!confirm('Remove connection “' + connection.name + '” and its saved key?')) return;
      managedAction(button, 'connections-feedback', async () => { await api('connections/' + connection.id, 'DELETE'); feedback('connections-feedback', 'Connection removed.'); });
    }, active || !!state.active);
    $('connections-list').append(card);
  }
  if (!state.skills.length) $('skills-list').append(el('p', 'No skills yet. Add a procedure with steps and verification.', 'empty-state'));
  for (const skill of state.skills) {
    const {card, actions} = managementCard(skill.name, skill.description || 'Saved procedure');
    cardAction(actions, 'Edit', () => editSkill(skill));
    cardAction(actions, 'Remove', button => {
      if (!confirm('Remove skill “' + skill.name + '”?')) return;
      managedAction(button, 'skills-feedback', async () => { await api('skills/' + skill.id, 'DELETE'); feedback('skills-feedback', 'Skill removed.'); });
    });
    $('skills-list').append(card);
  }
  if (!state.mcp_servers?.length) $('mcp-list').append(el('p', 'No servers connected. Add a Streamable HTTP endpoint to make its tools available.', 'empty-state'));
  for (const server of state.mcp_servers || []) {
    const {card, actions} = managementCard(server.name, server.url + (server.token_set ? ' · Token saved' : ''), server.enabled ? 'Enabled' : 'Disabled');
    cardAction(actions, 'Edit', () => editMCP(server));
    cardAction(actions, 'Test & tools', button => managedAction(button, 'mcp-feedback', async () => {
      const result = await api('mcp/' + server.id + '/test', 'POST', {});
      let details = card.querySelector('.discovered-tools');
      if (!details) { details = el('p', undefined, 'discovered-tools'); card.append(details); }
      details.textContent = result.tools.length ? result.tools.join(' · ') : 'Connected. This server exposes no tools.';
      feedback('mcp-feedback', server.name + ': connected · ' + result.tools.length + ' tools.');
    }));
    cardAction(actions, server.enabled ? 'Disable' : 'Enable', button => managedAction(button, 'mcp-feedback', async () => {
      await api('mcp', 'PUT', {...server, enabled: !server.enabled}); feedback('mcp-feedback', server.enabled ? 'Server disabled.' : 'Server enabled.');
    }), !!state.active);
    cardAction(actions, 'Remove', button => {
      if (!confirm('Remove MCP server “' + server.name + '” and its saved token?')) return;
      managedAction(button, 'mcp-feedback', async () => { await api('mcp/' + server.id, 'DELETE'); feedback('mcp-feedback', 'Server removed.'); });
    }, !!state.active);
    $('mcp-list').append(card);
  }
}
function editConnection(connection = {}) {
  $('connection-form').hidden = false; $('connection-form').dataset.id = connection.id || '';
  $('connection-editor-title').textContent = connection.id ? 'Edit connection' : 'New connection';
  $('connection-name').value = connection.name || ''; $('connection-provider').value = connection.provider || 'ollama';
  $('connection-url').value = connection.base_url || 'http://127.0.0.1:11434'; $('connection-model').value = connection.model || '';
  $('connection-key').value = ''; $('connection-key').placeholder = connection.key_set ? 'Key saved · leave blank to keep it' : 'API key (optional for local providers)';
  $('connection-clear-key').checked = false; $('connection-activate').checked = !connection.id || state.config.connection_id === connection.id;
  connectionManualModel = false;
  resetConnectionModels(); loadConnectionModels();
  $('connection-name').focus(); $('connection-form').scrollIntoView({block: 'nearest'});
}
function connectionModelSource() {
  return {id: $('connection-form').dataset.id, provider: $('connection-provider').value,
    base_url: $('connection-url').value.trim().replace(/\/+$/, ''), api_key: $('connection-key').value,
    clear_api_key: $('connection-clear-key').checked};
}
function cancelConnectionModels() {
  clearTimeout(connectionModelTimer); connectionModelTimer = null;
  connectionModelRequest++; connectionModelAbort?.abort(); connectionModelAbort = null;
  $('connection-fetch-models').disabled = false;
  $('connection-fetch-models').textContent = 'Fetch models';
  $('connection-fetch-models').removeAttribute('aria-busy');
}
function renderConnectionModels() {
  const source = connectionModelSource(), current = $('connection-model').value;
  const saved = [...new Set([...(state.connections || []), state.config]
    .filter(c => c.provider === source.provider && c.base_url.replace(/\/+$/, '') === source.base_url)
    .map(c => c.model).filter(Boolean))];
  if (current && !connectionManualModel && !saved.includes(current)) saved.unshift(current);
  const select = $('connection-model-select');
  const placeholder = el('option', 'Choose an available model…'); placeholder.value = ''; placeholder.disabled = true;
  select.replaceChildren(placeholder);
  for (const [label, models] of [['Available from provider', connectionModels], ['Previously used', saved.filter(id => !connectionModels.includes(id))]]) {
    if (!models.length) continue;
    const group = el('optgroup'); group.label = label;
    for (const id of models) {
      const profile = modelProfile(id, source.provider);
      const option = el('option', profile.name !== id ? profile.name + ' · ' + id : id); option.value = id; option.title = id; group.append(option);
    }
    select.append(group);
  }
  const custom = el('option', 'Enter model ID manually…'); custom.value = '__manual__'; select.append(custom);
  select.value = connectionManualModel ? '__manual__' : current;
  $('connection-custom-model').hidden = !connectionManualModel;
  $('connection-model').required = connectionManualModel;
  select.required = !connectionManualModel;
}
function resetConnectionModels(clearModel = false) {
  cancelConnectionModels(); connectionModels = [];
  if (clearModel) { $('connection-model').value = ''; connectionManualModel = false; }
  renderConnectionModels();
  feedback('connection-model-feedback', 'Fetch models from this provider, or choose a previously used model.');
}
async function loadConnectionModels() {
  cancelConnectionModels();
  const source = connectionModelSource();
  let url;
  try { url = new URL(source.base_url); }
  catch { feedback('connection-model-feedback', 'Enter the provider URL to load its models.'); return; }
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
    feedback('connection-model-feedback', 'Enter an HTTP or HTTPS provider URL without credentials or query parameters.', true); return;
  }
  const savedKey = !source.clear_api_key && state.connections?.some(c => c.id === source.id && c.provider === source.provider && c.base_url === source.base_url && c.key_set);
  if (['api.openai.com', 'api.anthropic.com'].includes(url.hostname) && ((!source.api_key || source.clear_api_key) && !savedKey)) {
    feedback('connection-model-feedback', 'Add your API key to load the models available to your account.'); return;
  }
  const request = ++connectionModelRequest;
  const controller = new AbortController(); connectionModelAbort = controller;
  $('connection-fetch-models').disabled = true; $('connection-fetch-models').textContent = 'Loading…';
  $('connection-fetch-models').setAttribute('aria-busy', 'true');
  feedback('connection-model-feedback', 'Loading models from your provider…');
  try {
    const result = await api('connections/models', 'POST', source, controller.signal);
    if (request !== connectionModelRequest) return;
    connectionModels = [...new Set(result.models.filter(id => typeof id === 'string' && id.trim() && id.length <= 200))];
    if (!connectionManualModel && !$('connection-model').value && (['ollama', 'anthropic'].includes(source.provider) || connectionModels.length === 1)) {
      $('connection-model').value = connectionModels[0] || '';
    }
    renderConnectionModels();
    feedback('connection-model-feedback', connectionModels.length
      ? connectionModels.length + ' models loaded. Choose one from the list.'
      : source.provider === 'ollama' ? 'No installed models found. Install a model in Ollama, then click Fetch models.'
      : 'This provider returned no models. Check your access or choose Enter model ID manually.');
  } catch (error) {
    if (request !== connectionModelRequest || error.name === 'AbortError') return;
    feedback('connection-model-feedback', 'Could not load models. ' + error.message + ' Check the URL and API key, then retry. You can also enter a model ID manually.', true);
  } finally {
    if (request === connectionModelRequest) cancelConnectionModels();
  }
}
function editSkill(skill = {}) {
  $('skill-form').hidden = false; $('skill-form').dataset.id = skill.id || '';
  $('skill-editor-title').textContent = skill.id ? 'Edit skill' : 'New skill';
  $('skill-name').value = skill.name || ''; $('skill-description').value = skill.description || ''; $('skill-instructions').value = skill.instructions || '';
  $('skill-name').focus(); $('skill-form').scrollIntoView({block: 'nearest'});
}
function editMCP(server = {}) {
  $('mcp-form').hidden = false; $('mcp-form').dataset.id = server.id || '';
  $('mcp-editor-title').textContent = server.id ? 'Edit MCP server' : 'New MCP server';
  $('mcp-name').value = server.name || ''; $('mcp-url').value = server.url || ''; $('mcp-timeout').value = server.timeout_sec || 30;
  $('mcp-token').value = ''; $('mcp-token').placeholder = server.token_set ? 'Token saved · leave blank to keep it' : 'Optional bearer token';
  $('mcp-enabled').checked = server.enabled ?? true; $('mcp-clear-token').checked = false;
  $('mcp-name').focus(); $('mcp-form').scrollIntoView({block: 'nearest'});
}
function initManagement() {
  $('open-settings').addEventListener('click', () => showSettings());
  for (const button of document.querySelectorAll('[data-settings-tab]')) button.addEventListener('click', () => selectSettingsTab(button.dataset.settingsTab));
  $('about-model').addEventListener('click', () => {
    if (!state) return; setModelMenu(false); renderModelDescription(); $('model-id-detail').textContent = state.config.model + ' · ' + state.config.provider; $('model-dialog').showModal();
  });
  $('command-search').addEventListener('input', () => { paletteIndex = 0; renderPalette(); });
  $('command-search').addEventListener('keydown', event => {
    if (event.key === 'Enter') { event.preventDefault(); choosePalette(paletteIndex); }
    if ((event.key === 'ArrowDown' || event.key === 'ArrowUp') && paletteMatches.length) {
      event.preventDefault(); paletteIndex = (paletteIndex + (event.key === 'ArrowDown' ? 1 : -1) + paletteMatches.length) % paletteMatches.length;
      renderPalette(); $('palette-' + paletteIndex)?.scrollIntoView({block: 'nearest'});
    }
  });
  document.addEventListener('keydown', event => {
    if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
      event.preventDefault(); if (!document.querySelector('dialog[open]')) openPalette();
    }
  });
  for (const dialog of document.querySelectorAll('dialog')) dialog.addEventListener('click', event => {
    if (event.target !== dialog) return;
    const rect = dialog.getBoundingClientRect();
    if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
  });
  $('settings-dialog').addEventListener('close', () => {
    // A queued close event may arrive after Settings has already reopened.
    if ($('settings-dialog').open) return;
    cancelConnectionModels(); $('connection-form').hidden = true; $('connection-key').value = ''; $('mcp-token').value = '';
  });
  $('new-connection').addEventListener('click', () => editConnection());
  $('cancel-connection').addEventListener('click', () => { cancelConnectionModels(); $('connection-form').hidden = true; $('connection-key').value = ''; });
  $('connection-provider').addEventListener('change', () => {
    const provider = $('connection-provider').value;
    $('connection-url').value = {ollama: 'http://127.0.0.1:11434', openai: 'https://api.openai.com/v1', 'openai-responses': 'https://api.openai.com/v1', anthropic: 'https://api.anthropic.com'}[provider];
    $('connection-key').value = ''; $('connection-clear-key').checked = false;
    resetConnectionModels(true); loadConnectionModels();
  });
  $('connection-url').addEventListener('input', () => resetConnectionModels(true));
  $('connection-url').addEventListener('change', loadConnectionModels);
  $('connection-key').addEventListener('input', () => {
    if ($('connection-key').value) $('connection-clear-key').checked = false;
    resetConnectionModels(); connectionModelTimer = setTimeout(loadConnectionModels, 350);
  });
  $('connection-clear-key').addEventListener('change', () => {
    if ($('connection-clear-key').checked) $('connection-key').value = '';
    resetConnectionModels(); loadConnectionModels();
  });
  $('connection-fetch-models').addEventListener('click', loadConnectionModels);
  $('connection-model-select').addEventListener('change', () => {
    const value = $('connection-model-select').value;
    connectionManualModel = value === '__manual__'; $('connection-model').value = connectionManualModel ? '' : value;
    renderConnectionModels();
    if (connectionManualModel) $('connection-model').focus();
  });
  $('connection-form').addEventListener('submit', async event => {
    event.preventDefault();
    const input = {id: $('connection-form').dataset.id, name: $('connection-name').value, provider: $('connection-provider').value,
      base_url: $('connection-url').value, model: $('connection-model').value, api_key: $('connection-key').value,
      clear_api_key: $('connection-clear-key').checked, activate: $('connection-activate').checked};
    await managedAction($('save-connection'), 'connections-feedback', async () => {
      await api('connections', 'PUT', input); cancelConnectionModels(); $('connection-key').value = ''; $('connection-form').hidden = true; feedback('connections-feedback', 'Connection saved.');
    });
  });
  $('new-skill').addEventListener('click', () => editSkill());
  $('cancel-skill').addEventListener('click', () => { $('skill-form').hidden = true; });
  $('skill-form').addEventListener('submit', async event => {
    event.preventDefault();
    await managedAction($('save-skill'), 'skills-feedback', async () => {
      await api('skills', 'PUT', {id: $('skill-form').dataset.id, name: $('skill-name').value, description: $('skill-description').value, instructions: $('skill-instructions').value});
      $('skill-form').hidden = true; feedback('skills-feedback', 'Skill saved.');
    });
  });
  $('new-mcp').addEventListener('click', () => editMCP());
  $('cancel-mcp').addEventListener('click', () => { $('mcp-form').hidden = true; $('mcp-token').value = ''; });
  $('mcp-form').addEventListener('submit', async event => {
    event.preventDefault();
    await managedAction($('save-mcp'), 'mcp-feedback', async () => {
      await api('mcp', 'PUT', {id: $('mcp-form').dataset.id, name: $('mcp-name').value, url: $('mcp-url').value,
        timeout_sec: Number($('mcp-timeout').value), token: $('mcp-token').value, clear_token: $('mcp-clear-token').checked, enabled: $('mcp-enabled').checked});
      $('mcp-token').value = ''; $('mcp-form').hidden = true; feedback('mcp-feedback', 'MCP server saved. Use Test & tools to check it.');
    });
  });
}
