'use strict';

function ollamaReady() {
  return state?.config.provider !== 'ollama' || modelsState === 'ready' && availableModels.includes(state.config.model);
}
function ollamaPickerLabel() {
  if (modelsState === 'unreachable') return 'Connect Ollama';
  if (modelsState !== 'ready') return 'Checking Ollama…';
  return availableModels.length ? 'Choose model' : 'Set up Ollama';
}
function renderOllamaSetup() {
  if (!state) return;
  const visible = state.config.provider === 'ollama' && modelsState !== 'checking' && !ollamaReady();
  $('ollama-setup').hidden = !visible;
  if (!visible) return;
  const unreachable = modelsState === 'unreachable', hasModels = !unreachable && availableModels.length > 0;
  $('ollama-setup-title').textContent = unreachable ? 'Connect Ollama to get started' : hasModels ? 'Choose an installed model' : 'Download your first model';
  $('ollama-setup-description').textContent = unreachable
    ? 'LiteAgent cannot reach Ollama. Open it, or install it if needed. Check the address in Settings if Ollama runs on another computer.'
    : hasModels ? 'Your saved model is not in Ollama’s installed list. Choose one of the ' + availableModels.length + ' available models below.'
    : 'Ollama is connected, but its model list is empty. Download a model first, then select it in LiteAgent.';
  $('ollama-endpoint').textContent = state.config.base_url;
  $('ollama-install-steps').hidden = hasModels;
  $('choose-ollama').hidden = !hasModels;
  $('check-ollama').disabled = modelsLoading;
  $('check-ollama').textContent = modelsLoading ? 'Checking…' : 'Check again';
}
function initSetup() {
  $('check-ollama').addEventListener('click', loadModels);
  $('choose-ollama').addEventListener('click', event => { event.stopPropagation(); setModelMenu(true); });
  $('setup-api-provider').addEventListener('click', () => showSettings('connections'));
  $('copy-ollama-command').addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText($('ollama-pull-command').textContent);
      feedback('ollama-setup-feedback', 'Command copied. Run it in PowerShell or your terminal, then choose the installed model.');
    } catch { feedback('ollama-setup-feedback', 'Select and copy the command above.', true); }
  });
  $('quit-app').addEventListener('click', async () => {
    if (!confirm('Quit LiteAgent? This stops the server, cancels any active reply and pauses reminders until you reopen the application.')) return;
    serverStopping = true;
    $('quit-app').disabled = true;
    setModelMenu(false);
    updateControls();
    try {
      await api('shutdown', 'POST', {});
      $('connection').textContent = 'LiteAgent stopped';
      feedback('chat-feedback', 'LiteAgent has stopped. Close this tab and reopen the application when you need it.');
      $('send').disabled = true; $('stop').hidden = true; $('model-menu-toggle').disabled = true;
    } catch (error) {
      if (error.status) { serverStopping = false; $('quit-app').disabled = false; updateControls(); }
      feedback('chat-feedback', error.status ? error.message : 'The server may have stopped. Reopen LiteAgent to reconnect.', true);
    }
  });
}
