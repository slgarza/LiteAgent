'use strict';
const notificationPrefsKey = 'liteagent-notification-settings', notificationSeenKey = 'liteagent-alerts-seen';
let notificationPrefs = readNotificationPrefs(), notificationAudio = null, lastChimeAt = 0;
let seenAlerts = new Set(), alertsInitialized = false;
function readNotificationPrefs() {
  let saved = {};
  try { saved = JSON.parse(localStorage.getItem(notificationPrefsKey)) || {}; } catch { /* Storage is optional. */ }
  return {sound: saved.sound !== false, desktop: saved.desktop !== false, replies: saved.replies !== false};
}
function saveNotificationPrefs() {
  try { localStorage.setItem(notificationPrefsKey, JSON.stringify(notificationPrefs)); } catch { /* Keep in-memory preferences. */ }
  renderNotificationSettings();
}
function notificationPermission() {
  return 'Notification' in window && window.isSecureContext ? Notification.permission : 'unsupported';
}
function renderNotificationSettings() {
  const permission = notificationPermission();
  const enabled = permission === 'granted' && notificationPrefs.desktop;
  $('notification-permission').textContent = enabled ? 'Enabled' : permission === 'denied' ? 'Blocked' : permission === 'unsupported' ? 'Unavailable' : 'Off';
  $('notification-permission-help').textContent = {
    granted: enabled ? 'Alerts appear on your desktop. Click one to open its conversation.' : 'Browser permission is granted. Turn alerts on whenever you want.',
    default: 'Enable alerts, then choose Allow in the browser permission prompt.',
    denied: 'Notifications are blocked. Allow notifications for this site in your browser’s site settings, then return here.',
    unsupported: 'Desktop notifications are unavailable in this browser. In-app alerts and sound still work.',
  }[permission];
  $('enable-notifications').textContent = enabled ? 'Turn off desktop alerts' : 'Enable desktop alerts';
  $('enable-notifications').disabled = permission === 'denied' || permission === 'unsupported';
  $('notification-sound').checked = notificationPrefs.sound;
  $('notification-replies').checked = notificationPrefs.replies;
  $('notification-sound-help').textContent = !notificationPrefs.sound ? 'Sound is muted. Visual alerts remain enabled.'
    : notificationAudio?.state === 'running' ? 'A short, gentle chime for new alerts.' : 'Click Test notification to check sound. Your browser requires an interaction before playing audio.';
}
function unlockNotificationSound() {
  if (!notificationPrefs.sound) return;
  try {
    const Audio = window.AudioContext || window.webkitAudioContext;
    if (!Audio) return;
    if (!notificationAudio || notificationAudio.state === 'closed') notificationAudio = new Audio();
    if (notificationAudio.state !== 'running') notificationAudio.resume().then(renderNotificationSettings).catch(() => {});
  } catch { /* Visual alerts still work if audio is unavailable. */ }
}
async function playNotificationSound(force = false) {
  if (!notificationPrefs.sound || !notificationAudio) return false;
  try {
    if (notificationAudio.state !== 'running') {
      await Promise.race([notificationAudio.resume(), new Promise(resolve => setTimeout(resolve, 500))]);
    }
    if (!notificationPrefs.sound || notificationAudio.state !== 'running') return false;
    if (!force && Date.now() - lastChimeAt < 1200) return false;
    lastChimeAt = Date.now();
    const start = notificationAudio.currentTime;
    for (const [frequency, delay] of [[660, 0], [880, 0.16]]) {
      const oscillator = notificationAudio.createOscillator(), gain = notificationAudio.createGain();
      oscillator.type = 'sine'; oscillator.frequency.value = frequency;
      gain.gain.setValueAtTime(0.0001, start + delay);
      gain.gain.exponentialRampToValueAtTime(0.12, start + delay + 0.02);
      gain.gain.exponentialRampToValueAtTime(0.0001, start + delay + 0.35);
      oscillator.connect(gain); gain.connect(notificationAudio.destination);
      oscillator.onended = () => { oscillator.disconnect(); gain.disconnect(); };
      oscillator.start(start + delay); oscillator.stop(start + delay + 0.36);
    }
    return true;
  } catch { return false; }
}
function openNotificationChat(notice) {
  window.focus();
  for (const dialog of document.querySelectorAll('dialog[open]')) dialog.close();
  if (notice.chat_id) selectChat(notice.chat_id);
}
function showNotificationToast(notice) {
  const host = $('notification-toasts');
  const card = el('article', undefined, 'notification-toast');
  const heading = el('div', undefined, 'card-heading');
  heading.append(el('strong', notice.title || 'LiteAgent'));
  const close = el('button', '×', 'text-button'); close.type = 'button'; close.setAttribute('aria-label', 'Dismiss notification'); heading.append(close);
  card.append(heading, el('p', notice.text || 'You have an update.', 'notification-body'));
  let timer;
  const dismiss = () => {
    clearTimeout(timer); card.remove();
    if (!host.childElementCount && host.hidePopover && host.matches(':popover-open')) host.hidePopover();
  };
  card.dismissNotification = dismiss;
  close.addEventListener('click', dismiss);
  if (notice.chat_id) {
    const open = el('button', 'Open conversation', 'text-button'); open.type = 'button';
    open.addEventListener('click', () => { dismiss(); openNotificationChat(notice); }); card.append(open);
  }
  host.append(card);
  while (host.childElementCount > 3) host.firstElementChild.dismissNotification();
  if (host.showPopover && !host.matches(':popover-open')) host.showPopover();
  timer = setTimeout(dismiss, 20000);
  card.addEventListener('mouseenter', () => clearTimeout(timer));
  card.addEventListener('focusin', () => clearTimeout(timer));
  card.addEventListener('mouseleave', () => { clearTimeout(timer); timer = setTimeout(dismiss, 20000); });
}
function showDesktopNotification(notice) {
  if (!notificationPrefs.desktop || notificationPermission() !== 'granted') return false;
  try {
    const notification = new Notification('LiteAgent · ' + (notice.title || 'Update'), {body: notice.text, tag: notice.id, silent: true});
    notification.onclick = () => { openNotificationChat(notice); notification.close(); };
    return true;
  } catch {
    feedback('notification-feedback', 'This browser could not display a desktop alert. In-app alerts still work.', true);
    return false;
  }
}
async function notifyUpdates(notices) {
  const consume = () => {
    try {
      const stored = JSON.parse(localStorage.getItem(notificationSeenKey));
      if (Array.isArray(stored)) { for (const id of stored) seenAlerts.add(id); alertsInitialized = true; }
    } catch { /* Use this tab's in-memory history. */ }
    const pending = [];
    for (const notice of [...notices].reverse()) {
      if (!notice.id) continue;
      if (alertsInitialized && !seenAlerts.has(notice.id) && (notice.kind !== 'reply' || notificationPrefs.replies && (document.hidden || !document.hasFocus()))) pending.push(notice);
      seenAlerts.add(notice.id);
    }
    alertsInitialized = true;
    seenAlerts = new Set([...seenAlerts].slice(-400));
    try { localStorage.setItem(notificationSeenKey, JSON.stringify([...seenAlerts])); } catch { /* Storage is optional. */ }
    // Mark the whole batch first, then announce only its latest updates with one chime.
    for (const notice of pending.slice(-3)) { showNotificationToast(notice); showDesktopNotification(notice); }
    if (pending.length) void playNotificationSound();
  };
  try {
    if (navigator.locks) await navigator.locks.request('liteagent-alert-delivery', consume);
    else consume();
  } catch { /* A browser notification failure must not interrupt chat polling. */ }
}
async function toggleDesktopNotifications() {
  unlockNotificationSound();
  const permission = notificationPermission();
  if (permission === 'granted') {
    notificationPrefs.desktop = !notificationPrefs.desktop;
  } else if (permission === 'default') {
    try { notificationPrefs.desktop = (await Notification.requestPermission()) === 'granted'; }
    catch { feedback('notification-feedback', 'The browser could not request notification permission. Check its site settings.', true); }
  }
  saveNotificationPrefs();
}
function initNotifications() {
  renderNotificationSettings();
  document.addEventListener('pointerdown', unlockNotificationSound, {passive: true});
  document.addEventListener('keydown', unlockNotificationSound, {passive: true});
  window.addEventListener('focus', renderNotificationSettings);
  window.addEventListener('storage', event => {
    if (event.key === notificationPrefsKey) { notificationPrefs = readNotificationPrefs(); renderNotificationSettings(); }
  });
  $('enable-notifications').addEventListener('click', toggleDesktopNotifications);
  $('notification-sound').addEventListener('change', () => {
    notificationPrefs.sound = $('notification-sound').checked; unlockNotificationSound(); saveNotificationPrefs();
  });
  $('notification-replies').addEventListener('change', () => { notificationPrefs.replies = $('notification-replies').checked; saveNotificationPrefs(); });
  $('test-notification').addEventListener('click', async () => {
    unlockNotificationSound();
    const notice = {id: 'liteagent-test', title: 'Notifications are ready', text: 'This is how LiteAgent will let you know about a reminder or a finished task.'};
    showNotificationToast(notice);
    const desktop = showDesktopNotification(notice), sound = await playNotificationSound(true);
    feedback('notification-feedback', 'In-app alert shown.' + (desktop ? ' Desktop alert sent.' : ' Enable desktop alerts for notifications outside the tab.')
      + (sound ? ' Sound played.' : notificationPrefs.sound ? ' Interact with this page and check that the tab is not muted to hear sound.' : ' Sound is muted.'));
    renderNotificationSettings();
  });
}
