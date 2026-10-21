/*
 * Blinkmail web client.
 *
 * Messages are rendered safely: everything untrusted goes through textContent
 * and createElement rather than innerHTML, so any HTML or script in an incoming
 * email is left as inert text instead of executing in the reader's browser.
 */
'use strict';

// Blinkmail-specific storage key, scoped so the session token cannot collide
// with another application on the same origin.
var SESSION_KEY = 'blinkmail_session_id';

var els = {
  appTitle: document.getElementById('appTitle'),
  appSubtitle: document.getElementById('appSubtitle'),
  inboxSelect: document.getElementById('inboxSelect'),
  copyBtn: document.getElementById('copyBtn'),
  refreshBtn: document.getElementById('refreshBtn'),
  newBtn: document.getElementById('newBtn'),
  deleteBtn: document.getElementById('deleteBtn'),
  newBox: document.getElementById('newBox'),
  localPartInput: document.getElementById('localPartInput'),
  domainSelect: document.getElementById('domainSelect'),
  createCustomBtn: document.getElementById('createCustomBtn'),
  createRandomBtn: document.getElementById('createRandomBtn'),
  currentInbox: document.getElementById('currentInbox'),
  messageCount: document.getElementById('messageCount'),
  messageList: document.getElementById('messageList'),
  errorBanner: document.getElementById('errorBanner'),
  errorText: document.getElementById('errorText'),
  errorDismiss: document.getElementById('errorDismiss'),
  toastContainer: document.getElementById('toastContainer'),
};

var appConfig = {
  appName: 'Blinkmail',
  mailDomain: 'example.com',
  mailDomains: ['example.com'],
  webHost: 'localhost:8080',
};

var sessionId = '';
var busy = false;

// ---- Small DOM helpers -----------------------------------------------------

// el creates an element with textContent only. Passing untrusted strings here
// can never produce markup, which is the whole point.
function el(tag, className, text) {
  var node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined && text !== null) node.textContent = String(text);
  return node;
}

// clear removes all children without using innerHTML = ''.
function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

function showEmptyState(icon, title, subtitle) {
  clear(els.messageList);
  var box = el('div', 'empty-state');
  box.appendChild(el('div', 'icon', icon));
  box.appendChild(el('div', 'title', title));
  box.appendChild(el('div', 'sub', subtitle));
  els.messageList.appendChild(box);
}

// ---- Errors and toasts -----------------------------------------------------

function showError(message) {
  els.errorText.textContent = message || 'Something went wrong.';
  els.errorBanner.hidden = false;
}

function clearError() {
  els.errorBanner.hidden = true;
  els.errorText.textContent = '';
}

function showToast(text) {
  var toast = el('div', 'toast', text);
  els.toastContainer.appendChild(toast);
  window.setTimeout(function () {
    toast.classList.add('fadeout');
    window.setTimeout(function () {
      if (toast.parentNode) toast.parentNode.removeChild(toast);
    }, 200);
  }, 1800);
}

// busyWhile disables the action buttons during a request so a slow or failing
// call cannot be triggered repeatedly.
function busyWhile(promise) {
  busy = true;
  setButtonsDisabled(true);
  return promise.finally(function () {
    busy = false;
    setButtonsDisabled(false);
  });
}

function setButtonsDisabled(disabled) {
  [els.copyBtn, els.refreshBtn, els.newBtn, els.deleteBtn,
   els.createCustomBtn, els.createRandomBtn].forEach(function (btn) {
    if (btn) btn.disabled = disabled;
  });
}

// ---- API access ------------------------------------------------------------

// ApiError carries the server's human-readable message plus its status, so a
// stale session can be detected and retried once.
function ApiError(message, status) {
  var err = new Error(message);
  err.name = 'ApiError';
  err.status = status;
  return err;
}

async function fetchJson(url, options) {
  var opts = options || {};
  var headers = { Accept: 'application/json' };
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  if (sessionId) headers['x-session-id'] = sessionId;

  var res;
  try {
    res = await fetch(url, {
      method: opts.method || 'GET',
      headers: headers,
      body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
    });
  } catch (networkErr) {
    throw ApiError('Cannot reach the Blinkmail server. Check your connection.', 0);
  }

  var payload = null;
  var text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch (parseErr) {
      payload = null;
    }
  }

  if (!res.ok) {
    // The API always answers errors as { "error": "..." }. Fall back to the
    // status line rather than dumping raw HTML into the UI.
    var message = payload && payload.error
      ? payload.error
      : 'Request failed with status ' + res.status;
    throw ApiError(message, res.status);
  }
  return payload;
}

// ---- Session handling ------------------------------------------------------

function storedSessionId() {
  try {
    return window.localStorage.getItem(SESSION_KEY) || '';
  } catch (storageErr) {
    // Private-browsing modes can throw on localStorage access. Blinkmail still
    // works without persistence, just without remembering the session.
    return '';
  }
}

function saveSessionId(token) {
  sessionId = token;
  try {
    window.localStorage.setItem(SESSION_KEY, token);
  } catch (storageErr) {
    // Non-fatal: the session stays valid for this page load only.
  }
}

function forgetSessionId() {
  sessionId = '';
  try {
    window.localStorage.removeItem(SESSION_KEY);
  } catch (storageErr) {
    // ignore
  }
}

async function ensureSession() {
  // The server only resumes tokens it issued; anything else gets a fresh one.
  var payload = await fetchJson('/api/session');
  if (!payload || !payload.sessionId) {
    throw ApiError('Server did not return a session id', 500);
  }
  saveSessionId(payload.sessionId);
}

// withFreshSessionOnAuthError retries an operation once after replacing a
// rejected session token. This is what makes a cleared or rotated database
// recover without the user having to wipe local storage by hand.
async function withFreshSessionOnAuthError(operation) {
  try {
    return await operation();
  } catch (err) {
    if (err && err.status === 401) {
      forgetSessionId();
      await ensureSession();
      return await operation();
    }
    throw err;
  }
}

// ---- Config ----------------------------------------------------------------

async function loadConfig() {
  var payload = await fetchJson('/api/config');
  if (payload) {
    appConfig = {
      appName: payload.appName || appConfig.appName,
      mailDomain: payload.mailDomain || appConfig.mailDomain,
      mailDomains: (payload.mailDomains && payload.mailDomains.length)
        ? payload.mailDomains
        : [payload.mailDomain || appConfig.mailDomain],
      webHost: payload.webHost || appConfig.webHost,
    };
  }

  document.title = appConfig.appName;
  els.appTitle.textContent = appConfig.appName;
  els.appSubtitle.textContent = 'Disposable inbox for ' + appConfig.mailDomain;

  clear(els.domainSelect);
  appConfig.mailDomains.forEach(function (domain) {
    var option = el('option', null, '@' + domain);
    option.value = domain;
    els.domainSelect.appendChild(option);
  });
  // With a single domain there is nothing to choose.
  els.domainSelect.style.display = appConfig.mailDomains.length <= 1 ? 'none' : '';

  els.localPartInput.placeholder =
    'username, or blank for random @' + appConfig.mailDomain;
}

// ---- Inboxes ---------------------------------------------------------------

async function loadInboxes(selectedAddress) {
  var inboxes = await fetchJson('/api/inboxes');
  if (!Array.isArray(inboxes)) inboxes = [];

  clear(els.inboxSelect);

  if (inboxes.length === 0) {
    var none = el('option', null, 'No inboxes yet');
    none.value = '';
    els.inboxSelect.appendChild(none);
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
    showEmptyState('📬', 'No inboxes yet',
      'Click New to create a disposable email address.');
    setInboxControlsEnabled(false);
    return;
  }

  inboxes.forEach(function (inbox) {
    var option = el('option', null, inbox.address);
    option.value = inbox.address;
    els.inboxSelect.appendChild(option);
  });

  var addresses = inboxes.map(function (inbox) { return inbox.address; });
  els.inboxSelect.value = (selectedAddress && addresses.indexOf(selectedAddress) >= 0)
    ? selectedAddress
    : addresses[0];

  setInboxControlsEnabled(true);
  await loadMessages();
}

function setInboxControlsEnabled(enabled) {
  [els.copyBtn, els.deleteBtn].forEach(function (btn) {
    if (btn) btn.disabled = !enabled;
  });
}

async function loadMessages() {
  var address = els.inboxSelect.value;
  if (!address) {
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
    showEmptyState('📬', 'No inbox selected', 'Create or select an address first.');
    return;
  }

  els.currentInbox.textContent = address;
  els.messageList.setAttribute('aria-busy', 'true');

  try {
    var messages = await fetchJson(
      '/api/inboxes/' + encodeURIComponent(address) + '/messages');
    if (!Array.isArray(messages)) messages = [];
    renderMessages(messages);
  } finally {
    els.messageList.setAttribute('aria-busy', 'false');
  }
}

// renderMessages builds the message list with DOM nodes only.
//
// from_address, subject and body all come from strangers' email, so they are
// assigned to textContent. HTML in a body therefore displays as literal text
// instead of being parsed — that is the intended rendering for HTML-only mail.
function renderMessages(messages) {
  els.messageCount.textContent = messages.length + ' messages';
  clear(els.messageList);

  if (messages.length === 0) {
    showEmptyState('✉️', 'Inbox empty',
      'Emails sent to this address will appear here. Click Refresh after sending.');
    return;
  }

  messages.forEach(function (msg) {
    var item = el('div', 'message-item');

    var meta = el('div', 'message-meta');
    meta.appendChild(document.createTextNode('From: ' + (msg.from_address || 'unknown')));
    meta.appendChild(document.createTextNode(' • '));
    meta.appendChild(document.createTextNode(formatTimestamp(msg.received_at)));

    item.appendChild(meta);
    item.appendChild(el('strong', null, msg.subject || '(no subject)'));
    item.appendChild(el('p', null, msg.body === undefined || msg.body === null ? '' : msg.body));

    els.messageList.appendChild(item);
  });
}

// formatTimestamp renders a stored RFC 3339 UTC timestamp in local time.
// Invalid or missing values fall back to the raw string rather than showing
// "Invalid Date".
function formatTimestamp(value) {
  if (!value) return '';
  var date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  try {
    return date.toLocaleString();
  } catch (localeErr) {
    return date.toISOString();
  }
}

// ---- Actions ---------------------------------------------------------------

function toggleNewBox() {
  var hidden = els.newBox.classList.toggle('hidden');
  els.newBtn.setAttribute('aria-expanded', hidden ? 'false' : 'true');
  if (!hidden) els.localPartInput.focus();
}

async function copyAddress() {
  var address = els.inboxSelect.value;
  if (!address) return;

  // navigator.clipboard needs a secure context; fall back to a hidden textarea
  // so copying still works over plain HTTP on a LAN or localhost test.
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(address);
    } else {
      var scratch = document.createElement('textarea');
      scratch.value = address;
      scratch.setAttribute('readonly', '');
      scratch.style.position = 'fixed';
      scratch.style.opacity = '0';
      document.body.appendChild(scratch);
      scratch.select();
      var copied = document.execCommand('copy');
      document.body.removeChild(scratch);
      if (!copied) throw new Error('execCommand copy failed');
    }
    showToast('📋 Copied ' + address);
  } catch (err) {
    showError('Could not copy to the clipboard. The address is: ' + address);
  }
}

async function deleteInbox() {
  var address = els.inboxSelect.value;
  if (!address) return;

  // Deleting only unlinks the address from this browser. The wording must not
  // promise the messages are gone, because they are not.
  var ok = window.confirm(
    'Remove ' + address + ' from this browser?\n\n' +
    'Messages are kept on the server and the address stays public. ' +
    'Anyone who knows it can still read them.'
  );
  if (!ok) return;

  await busyWhile((async function () {
    await withFreshSessionOnAuthError(async function () {
      await fetchJson('/api/inboxes/' + encodeURIComponent(address), { method: 'DELETE' });
    });
    await loadInboxes();
    showToast('Removed ' + address + ' from this browser');
  })());
}

async function createInbox(localPart) {
  var domain = els.domainSelect.value || appConfig.mailDomain;
  var body = { domain: domain };
  if (localPart) body.localPart = localPart;

  await busyWhile((async function () {
    var inbox = await withFreshSessionOnAuthError(function () {
      return fetchJson('/api/inboxes', { method: 'POST', body: body });
    });
    els.localPartInput.value = '';
    await loadInboxes(inbox && inbox.address);
    if (inbox && inbox.address) showToast('Using ' + inbox.address);
  })());
}

// ---- Wiring ----------------------------------------------------------------

function handleError(err, fallback) {
  var message = (err && err.message) ? err.message : fallback;
  showError(message);
  // Also log for diagnosis; the banner is the user-facing channel.
  console.error(err);
}

function bind(name, handler) {
  var target = els[name];
  if (!target) return;
  target.addEventListener('click', function (event) {
    event.preventDefault();
    if (busy) return;
    clearError();
    handler().catch(function (err) { handleError(err, 'That action failed.'); });
  });
}

function init() {
  sessionId = storedSessionId();

  bind('copyBtn', copyAddress);
  bind('refreshBtn', loadMessages);
  bind('newBtn', async function () { toggleNewBox(); });
  bind('deleteBtn', deleteInbox);
  bind('createCustomBtn', function () {
    return createInbox(els.localPartInput.value.trim());
  });
  bind('createRandomBtn', function () { return createInbox(''); });

  els.errorDismiss.addEventListener('click', clearError);

  els.inboxSelect.addEventListener('change', function () {
    clearError();
    loadMessages().catch(function (err) { handleError(err, 'Could not load messages.'); });
  });

  // Enter in the username field creates that address.
  els.localPartInput.addEventListener('keydown', function (event) {
    if (event.key !== 'Enter') return;
    event.preventDefault();
    if (busy) return;
    clearError();
    createInbox(els.localPartInput.value.trim())
      .catch(function (err) { handleError(err, 'Could not create the address.'); });
  });

  start();
}

async function start() {
  els.messageList.setAttribute('aria-busy', 'true');
  try {
    await loadConfig();
    // A stored token that the server no longer recognises is replaced here
    // rather than failing the whole page.
    await ensureSession();
    await loadInboxes();
    setInboxControlsEnabled(Boolean(els.inboxSelect.value));
  } catch (err) {
    handleError(err, 'Could not load Blinkmail.');
    showEmptyState('⚠️', 'Connection error',
      'Could not reach the Blinkmail server. Reload the page to try again.');
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
  } finally {
    els.messageList.setAttribute('aria-busy', 'false');
  }
}

init();
