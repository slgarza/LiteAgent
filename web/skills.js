'use strict';
let inspectedSkill = null, resourcePage = null, resourceRequest = 0, resourceOffsets = [0];
function renderSkills() {
  if (!state.skills.length) $('skills-list').append(el('p', 'Import a SKILL.md package or write your own procedure.', 'empty-state'));
  for (const skill of state.skills) {
    const {card, actions} = managementCard(skill.name, skill.description, skill.disabled ? 'Disabled' : skill.compatibility?.status || 'Procedure');
    const source = skill.source || {};
    card.insertBefore(el('p', [skill.version ? 'v' + skill.version : '', (skill.files?.length || 1) + ((skill.files?.length || 1) === 1 ? ' file' : ' files'), source.kind || 'local', source.commit?.slice(0, 8)].filter(Boolean).join(' · '), 'skill-meta'), actions);
    if (skill.compatibility?.notes?.length) {
      const details = el('details', undefined, 'skill-compatibility'); details.append(el('summary', 'Compatibility & source'));
      const list = el('ul'); for (const note of skill.compatibility.notes) list.append(el('li', note)); details.append(list);
      if (source.url) details.append(el('p', source.url + (source.path ? ' · ' + source.path : ''), 'muted'));
      card.insertBefore(details, actions);
    }
    cardAction(actions, 'Use', () => {
      $('settings-dialog').close(); $('message').value = '/skill ' + skill.id + ' ' + $('message').value; renderCommands(); updateControls(); $('message').focus();
    }, !!skill.disabled);
    cardAction(actions, 'Files', () => inspectSkill(skill));
    cardAction(actions, 'Edit', () => editSkill(skill));
    cardAction(actions, 'Update', () => openSkillImport(skill));
    const download = el('a', 'Export ZIP', 'text-button'); download.href = '/api/skills/' + encodeURIComponent(skill.id) + '/export'; download.download = ''; actions.append(download);
    cardAction(actions, skill.disabled ? 'Enable' : 'Disable', button => managedAction(button, 'skills-feedback', async () => {
      await api('skills/' + skill.id + '/toggle', 'POST', {disabled: !skill.disabled, expected_revision: skill.revision});
      feedback('skills-feedback', skill.disabled ? 'Skill enabled. Resume schedules separately.' : 'Skill disabled and its schedules paused. Active work keeps its current revision.');
    }));
    cardAction(actions, 'Remove', button => {
      if (!confirm('Remove skill “' + skill.name + '”?')) return;
      managedAction(button, 'skills-feedback', async () => {
        await api('skills/' + skill.id, 'DELETE'); if (inspectedSkill?.id === skill.id) closeSkillInspector(); feedback('skills-feedback', 'Skill removed.');
      });
    });
    $('skills-list').append(card);
  }
}
function skillImportFields() {
  const kind = $('skill-import-kind').value;
  $('skill-zip-field').hidden = kind !== 'zip'; $('skill-folder-field').hidden = kind !== 'folder'; $('skill-github-fields').hidden = kind !== 'github';
  $('skill-zip').required = kind === 'zip'; $('skill-folder').required = kind === 'folder'; $('skill-github-url').required = kind === 'github';
}
function openSkillImport(skill = {}) {
  const form = $('skill-import-form'); form.reset(); form.dataset.id = skill.id || ''; form.dataset.revision = skill.revision || ''; form.hidden = false;
  $('skill-import-title').textContent = skill.id ? 'Update ' + skill.name : 'Import skill';
  $('submit-skill-import').textContent = skill.id ? 'Update package' : 'Import';
  if (skill.source?.kind === 'github') {
    $('skill-import-kind').value = 'github'; $('skill-github-url').value = skill.source.url;
    $('skill-github-ref').value = skill.source.ref || ''; $('skill-import-path').value = skill.source.path || '';
  }
  skillImportFields(); form.scrollIntoView({block: 'nearest'}); $('skill-import-kind').focus();
}
async function submitSkillImport(event) {
  event.preventDefault();
  const form = $('skill-import-form'), kind = $('skill-import-kind').value;
  const input = {kind, id: form.dataset.id, expected_revision: form.dataset.revision, path: $('skill-import-path').value.trim()};
  await managedAction($('submit-skill-import'), 'skills-feedback', async () => {
    if (kind === 'github') {
      input.url = $('skill-github-url').value.trim(); input.ref = $('skill-github-ref').value.trim(); await api('skills/import', 'POST', input);
    } else {
      const data = new FormData();
      if (kind === 'zip') {
        const file = $('skill-zip').files[0]; if (!file) throw new Error('Select a ZIP file.');
        if (file.size > 32 * 1024 * 1024) throw new Error('ZIP exceeds 32 MiB.'); data.append('archive', file);
      } else {
        const files = [...$('skill-folder').files];
        if (!files.length || files.length > 128) throw new Error('Select a skill folder with 1–128 files.');
        if (files.reduce((n, file) => n + file.size, 0) > 16 * 1024 * 1024) throw new Error('Package exceeds 16 MiB.');
        input.paths = files.map((file, i) => { data.append('file' + i, file); return file.webkitRelativePath || file.name; });
      }
      data.append('request', JSON.stringify(input));
      const response = await fetch('/api/skills/import', {method: 'POST', headers: {'X-LiteAgent': '1'}, body: data});
      const result = await response.json(); if (!response.ok) throw new Error(result.error || 'Import failed.');
    }
    form.hidden = true; form.reset(); feedback('skills-feedback', 'Package saved. Open Files to inspect it, then choose Use when needed.');
  });
}
function closeSkillInspector() {
  if (!$('skill-resource-content').readOnly && !confirm('Discard unsaved resource edits?')) return false;
  resourceRequest++; inspectedSkill = null; $('skill-inspector').hidden = true; return true;
}
function inspectSkill(skill) {
  if (inspectedSkill && !closeSkillInspector()) return;
  $('skill-resource-path').disabled = false; inspectedSkill = skill; resourceOffsets = [0]; $('skill-inspector').hidden = false;
  $('skill-inspector-title').textContent = skill.name + ' · Files';
  $('skill-resource-path').replaceChildren(...(skill.files || [{path: 'SKILL.md'}]).map(file => { const option = el('option', file.path); option.value = file.path; return option; }));
  $('skill-resource-path').value = 'SKILL.md'; loadSkillResource(); $('skill-inspector').scrollIntoView({block: 'nearest'});
}
function resourceURL(skill, path, offset) {
  return 'skills/' + encodeURIComponent(skill.id) + '/file?' + new URLSearchParams({path, offset, revision: skill.revision || ''});
}
async function loadSkillResource() {
  const request = ++resourceRequest, skill = inspectedSkill, path = $('skill-resource-path').value;
  if (!skill) return;
  $('skill-resource-content').readOnly = true; $('skill-resource-save').hidden = true; $('skill-resource-cancel').hidden = true; $('skill-resource-edit').hidden = false;
  $('skill-resource-edit').disabled = true; $('skill-resource-next').disabled = true; $('skill-resource-prev').disabled = true;
  $('skill-resource-content').value = ''; feedback('skill-resource-feedback', 'Loading…');
  try {
    const page = await api(resourceURL(skill, path, resourceOffsets.at(-1)));
    if (request !== resourceRequest) return;
    resourcePage = page; $('skill-resource-content').value = page.text ? page.content : 'Binary resource. Download it or ask the agent to copy it to a working folder.';
    $('skill-resource-info').textContent = page.size.toLocaleString() + ' bytes · ' + (page.text ? 'Text' : 'Binary') + ' · SHA-256 ' + page.sha256.slice(0, 12) + (page.truncated ? ' · More pages available' : '');
    $('skill-resource-next').disabled = !page.truncated; $('skill-resource-prev').disabled = resourceOffsets.length < 2;
    $('skill-resource-edit').disabled = !page.text || page.size > 65536;
    $('skill-resource-download').href = '/api/skills/' + encodeURIComponent(skill.id) + '/export?' + new URLSearchParams({path});
    feedback('skill-resource-feedback', '');
  } catch (error) { if (request === resourceRequest) feedback('skill-resource-feedback', error.message, true); }
}
async function editSkillResource() {
  const request = ++resourceRequest, skill = inspectedSkill, path = $('skill-resource-path').value;
  await busy($('skill-resource-edit'), 'Loading…', async () => {
    try {
      let offset = 0, content = '', page;
      do {
        page = await api(resourceURL(skill, path, offset));
        if (request !== resourceRequest) return;
        if (!page.text || page.size > 65536) throw new Error('Only text resources up to 64 KiB can be edited here.');
        content += page.content; offset = page.next_offset;
      } while (page.truncated);
      resourcePage = page; $('skill-resource-content').value = content; $('skill-resource-content').readOnly = false;
      $('skill-resource-path').disabled = true; $('skill-resource-prev').disabled = true; $('skill-resource-next').disabled = true;
      $('skill-resource-save').hidden = false; $('skill-resource-cancel').hidden = false; $('skill-resource-edit').hidden = true; $('skill-resource-content').focus();
      feedback('skill-resource-feedback', 'Edits create a new revision. Keep the YAML header in SKILL.md.');
    } catch (error) { if (request === resourceRequest) feedback('skill-resource-feedback', error.message, true); }
  });
}
$('import-skill').addEventListener('click', () => openSkillImport());
$('skill-import-kind').addEventListener('change', skillImportFields);
$('cancel-skill-import').addEventListener('click', () => { $('skill-import-form').hidden = true; });
$('skill-import-form').addEventListener('submit', submitSkillImport);
$('close-skill-inspector').addEventListener('click', closeSkillInspector);
$('skill-resource-path').addEventListener('change', () => { resourceOffsets = [0]; loadSkillResource(); });
$('skill-resource-prev').addEventListener('click', () => { resourceOffsets.pop(); loadSkillResource(); });
$('skill-resource-next').addEventListener('click', () => { resourceOffsets.push(resourcePage.next_offset); loadSkillResource(); });
$('skill-resource-edit').addEventListener('click', editSkillResource);
$('skill-resource-cancel').addEventListener('click', () => { $('skill-resource-path').disabled = false; resourceOffsets = [0]; loadSkillResource(); });
$('skill-resource-save').addEventListener('click', () => managedAction($('skill-resource-save'), 'skill-resource-feedback', async () => {
  const request = ++resourceRequest;
  const saved = await api('skills/' + inspectedSkill.id + '/file', 'PUT', {path: resourcePage.path, content: $('skill-resource-content').value, expected_revision: inspectedSkill.revision, expected_sha256: resourcePage.sha256});
  if (request !== resourceRequest) return;
  inspectedSkill = saved; $('skill-resource-path').disabled = false; resourceOffsets = [0]; await loadSkillResource(); feedback('skill-resource-feedback', 'File saved.');
}));
