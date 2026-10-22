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
  loginView: document.getElementById('loginView'),
  loginForm: document.getElementById('loginForm'),
  passwordInput: document.getElementById('passwordInput'),
  loginBtn: document.getElementById('loginBtn'),
  loginError: document.getElementById('loginError'),
  appView: document.getElementById('appView'),
  appTitle: document.getElementById('appTitle'),
  appSubtitle: document.getElementById('appSubtitle'),
  settingsBtn: document.getElementById('settingsBtn'),
  settingsPanel: document.getElementById('settingsPanel'),
  logoutBtn: document.getElementById('logoutBtn'),
  passwordForm: document.getElementById('passwordForm'),
  currentPasswordInput: document.getElementById('currentPasswordInput'),
  newPasswordInput: document.getElementById('newPasswordInput'),
  passwordSubmitBtn: document.getElementById('passwordSubmitBtn'),
  passwordStatus: document.getElementById('passwordStatus'),
  apiKeyForm: document.getElementById('apiKeyForm'),
  apiKeyName: document.getElementById('apiKeyName'),
  apiKeyPermInbound: document.getElementById('apiKeyPermInbound'),
  apiKeyPermRead: document.getElementById('apiKeyPermRead'),
  apiKeyCreateBtn: document.getElementById('apiKeyCreateBtn'),
  apiKeyStatus: document.getElementById('apiKeyStatus'),
  apiKeyList: document.getElementById('apiKeyList'),
  apiKeyReveal: document.getElementById('apiKeyReveal'),
  apiKeyValue: document.getElementById('apiKeyValue'),
  apiKeyCopyBtn: document.getElementById('apiKeyCopyBtn'),
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
};

var sessionId = '';
var busy = false;

// authState mirrors the server's view: true only while a valid login cookie is
// held. It is set from /api/auth/status and cleared on any 401, so the UI never
// shows the app on a guess.
var authState = false;

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

// showLoginError and clearLoginError drive the login card's own alert area,
// separate from the app's banner: while logged out the banner is not visible.
function showLoginError(message) {
  els.loginError.textContent = message || 'Could not sign in.';
  els.loginError.hidden = false;
}

function clearLoginError() {
  els.loginError.textContent = '';
  els.loginError.hidden = true;
}

// showPasswordStatus reports the outcome of a password change next to its form,
// again via textContent only.
function showPasswordStatus(message, isError) {
  els.passwordStatus.textContent = message || '';
  els.passwordStatus.classList.toggle('is-error', Boolean(isError));
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
  var method = opts.method || 'GET';
  var headers = { Accept: 'application/json' };
  // State-changing requests must declare JSON: the server rejects anything else
  // with 415, which is what stops a cross-site form from forging a request. The
  // header is set from the method rather than from the presence of a body, so a
  // bodyless POST (logout) is still labelled correctly.
  if (method !== 'GET' && method !== 'HEAD') {
    headers['Content-Type'] = 'application/json';
  }
  if (sessionId) headers['x-session-id'] = sessionId;

  var res;
  try {
    res = await fetch(url, {
      method: method,
      headers: headers,
      // The auth cookie is HttpOnly, so the browser attaches it automatically;
      // same-origin is the default and sends it.
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

// ---- Authentication --------------------------------------------------------

// checkAuth asks the server whether this browser holds a valid login cookie.
async function checkAuth() {
  var payload = await fetchJson('/api/auth/status');
  authState = Boolean(payload && payload.authenticated);
  return authState;
}

// showLoginView hides the app and focuses the password field, so keyboard and
// screen-reader users land where the next action is.
function showLoginView(message) {
  authState = false;
  els.appView.hidden = true;
  els.settingsPanel.hidden = true;
  els.settingsBtn.setAttribute('aria-expanded', 'false');
  // Never leave a revealed plaintext key on screen after signing out.
  els.apiKeyReveal.hidden = true;
  els.apiKeyValue.value = '';
  els.loginView.hidden = false;
  els.passwordInput.value = '';
  clearLoginError();
  if (message) showLoginError(message);
  els.passwordInput.focus();
}

// showAppView swaps the login card for the app shell.
function showAppView() {
  authState = true;
  els.loginView.hidden = true;
  els.appView.hidden = false;
  clearLoginError();
}

// submitLogin posts the password and, on success, loads the app.
async function submitLogin() {
  var password = els.passwordInput.value;
  if (!password) {
    showLoginError('Enter your password.');
    els.passwordInput.focus();
    return;
  }

  els.loginBtn.disabled = true;
  clearLoginError();
  try {
    await fetchJson('/api/login', { method: 'POST', body: { password: password } });
    els.passwordInput.value = '';
    await startApp();
  } catch (err) {
    if (err && err.status === 401) {
      showLoginError('Invalid password.');
    } else if (err && err.status === 429) {
      showLoginError('Too many failed attempts. Wait a few minutes and try again.');
    } else {
      showLoginError((err && err.message) || 'Could not sign in.');
    }
    els.passwordInput.focus();
  } finally {
    els.loginBtn.disabled = false;
  }
}

// submitLogout revokes the session server-side, then returns to the login view.
async function submitLogout() {
  try {
    await fetchJson('/api/logout', { method: 'POST' });
  } catch (err) {
    // A 401 means the session was already gone, which is the desired end state.
    if (!err || err.status !== 401) {
      showError((err && err.message) || 'Could not sign out.');
      return;
    }
  }
  forgetSessionId();
  showLoginView('Signed out.');
  showToast('Signed out');
}

// submitPasswordChange changes the password. Success keeps this browser signed
// in; every other browser is revoked by the server.
async function submitPasswordChange() {
  var current = els.currentPasswordInput.value;
  var next = els.newPasswordInput.value;

  if (!current) {
    showPasswordStatus('Enter your current password.', true);
    els.currentPasswordInput.focus();
    return;
  }
  if (!next || next.length < 8) {
    showPasswordStatus('The new password must be at least 8 characters.', true);
    els.newPasswordInput.focus();
    return;
  }

  els.passwordSubmitBtn.disabled = true;
  showPasswordStatus('', false);
  try {
    await fetchJson('/api/password', {
      method: 'POST',
      body: { currentPassword: current, newPassword: next },
    });
    els.currentPasswordInput.value = '';
    els.newPasswordInput.value = '';
    showPasswordStatus('Password updated. Other browsers were signed out.', false);
    showToast('Password updated');
  } catch (err) {
    if (err && err.status === 401 && err.message === 'Authentication required') {
      // The gate refused the request: the login cookie is gone. A wrong current
      // password is also a 401, but carries a different message, so it is
      // handled below as a field error.
      forgetSessionId();
      showLoginView('Your session has expired. Please sign in again.');
      return;
    }
    if (err && err.status === 401) {
      showPasswordStatus('Current password is incorrect.', true);
    } else {
      showPasswordStatus((err && err.message) || 'Could not update the password.', true);
    }
  } finally {
    els.passwordSubmitBtn.disabled = false;
  }
}

// toggleSettings shows or hides the settings panel.
function toggleSettings() {
  var hidden = els.settingsPanel.hidden;
  els.settingsPanel.hidden = !hidden;
  els.settingsBtn.setAttribute('aria-expanded', hidden ? 'true' : 'false');
  if (hidden) {
    els.currentPasswordInput.focus();
    // Key metadata is only loaded when the panel is opened, so a browser that
    // never visits settings makes no extra requests.
    loadApiKeys().catch(function (err) {
      showApiKeyStatus((err && err.message) || 'Could not load API keys.', true);
    });
  } else {
    // Closing the panel drops the one-time plaintext from the DOM.
    els.apiKeyReveal.hidden = true;
    els.apiKeyValue.value = '';
  }
}

// ---- API keys --------------------------------------------------------------

// showApiKeyStatus reports the outcome of a key action next to the form, via
// textContent only.
function showApiKeyStatus(message, isError) {
  els.apiKeyStatus.textContent = message || '';
  els.apiKeyStatus.classList.toggle('is-error', Boolean(isError));
}

// renderApiKeys lists key metadata. Names are user input, so every field is
// assigned with textContent — never as markup.
function renderApiKeys(keys) {
  clear(els.apiKeyList);

  if (!keys || keys.length === 0) {
    els.apiKeyList.appendChild(el('p', 'api-key-empty',
      'No API keys yet. Create one to connect the Email Worker or an API client.'));
    return;
  }

  keys.forEach(function (key) {
    var item = el('div', 'api-key-item');

    var meta = el('div', 'api-key-meta');
    meta.appendChild(el('div', 'api-key-name', key.name));
    var detail = el('div');
    detail.appendChild(document.createTextNode('Prefix: '));
    detail.appendChild(el('span', 'api-key-prefix', key.prefix + '…'));
    detail.appendChild(document.createTextNode(
      ' • Created ' + formatTimestamp(key.created_at)));
    detail.appendChild(document.createTextNode(
      ' • Last used ' + (key.last_used_at ? formatTimestamp(key.last_used_at) : 'never')));
    var perms = Array.isArray(key.permissions) ? key.permissions : [];
    detail.appendChild(document.createTextNode(
      ' • Permissions: ' + (perms.length ? perms.join(', ') : 'none')));
    meta.appendChild(detail);

    var remove = el('button', 'danger', 'Revoke');
    remove.type = 'button';
    remove.setAttribute('aria-label', 'Revoke API key ' + key.name);
    remove.addEventListener('click', function () {
      revokeApiKey(key.id, key.name).catch(function (err) {
        showApiKeyStatus((err && err.message) || 'Could not revoke the key.', true);
      });
    });

    item.appendChild(meta);
    item.appendChild(remove);
    els.apiKeyList.appendChild(item);
  });
}

// loadApiKeys fetches key metadata. The plaintext is never part of this
// response; only creation returns it.
async function loadApiKeys() {
  var keys = await fetchJson('/api/keys');
  renderApiKeys(Array.isArray(keys) ? keys : []);
}

// submitCreateApiKey mints a key and reveals the plaintext exactly once.
async function submitCreateApiKey() {
  var name = els.apiKeyName.value.trim();
  if (!name) {
    showApiKeyStatus('Enter a name for the key.', true);
    els.apiKeyName.focus();
    return;
  }

  // The API rejects an empty permission list, so catch it here where the form
  // can point at the checkboxes instead of round-tripping a 400.
  var permissions = [];
  if (els.apiKeyPermInbound.checked) permissions.push('inbound');
  if (els.apiKeyPermRead.checked) permissions.push('read');
  if (permissions.length === 0) {
    showApiKeyStatus('Pick at least one permission.', true);
    return;
  }

  els.apiKeyCreateBtn.disabled = true;
  showApiKeyStatus('', false);
  try {
    var created = await fetchJson('/api/keys', {
      method: 'POST',
      body: { name: name, permissions: permissions },
    });
    els.apiKeyName.value = '';
    els.apiKeyPermInbound.checked = true;
    els.apiKeyPermRead.checked = true;
    showCreatedApiKey(created && created.key);
    await loadApiKeys();
    showApiKeyStatus('Key created. Copy it now — it is not stored anywhere.', false);
    showToast('API key created');
  } catch (err) {
    showApiKeyStatus((err && err.message) || 'Could not create the key.', true);
  } finally {
    els.apiKeyCreateBtn.disabled = false;
  }
}

// showCreatedApiKey puts the one-time plaintext into the read-only field and
// makes the warning visible. It is deliberately not logged or stored.
function showCreatedApiKey(key) {
  els.apiKeyValue.value = key || '';
  els.apiKeyReveal.hidden = !key;
  if (key) {
    els.apiKeyValue.focus();
    els.apiKeyValue.select();
  }
}

// copyApiKey copies the revealed key with the same secure-context fallback the
// inbox address copy uses.
async function copyApiKey() {
  var key = els.apiKeyValue.value;
  if (!key) return;

  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(key);
    } else {
      els.apiKeyValue.select();
      var copied = document.execCommand('copy');
      if (!copied) throw new Error('execCommand copy failed');
    }
    showToast('📋 API key copied');
  } catch (err) {
    showApiKeyStatus('Could not copy the key. Select it and copy manually.', true);
  }
}

// revokeApiKey deletes a key after an explicit confirmation. Deletion is
// immediate and cannot be undone: the plaintext is not recoverable.
async function revokeApiKey(id, name) {
  var ok = window.confirm(
    'Revoke the API key "' + name + '"?\n\n' +
    'Anything still using it — including the Email Worker — stops working ' +
    'immediately. This cannot be undone.'
  );
  if (!ok) return;

  await fetchJson('/api/keys/' + encodeURIComponent(id), { method: 'DELETE' });
  await loadApiKeys();
  showApiKeyStatus('Key revoked.', false);
  showToast('API key revoked');
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
    await fetchJson('/api/inboxes/' + encodeURIComponent(address), { method: 'DELETE' });
    await loadInboxes();
    showToast('Removed ' + address + ' from this browser');
  })());
}

async function createInbox(localPart) {
  var domain = els.domainSelect.value || appConfig.mailDomain;
  var body = { domain: domain };
  if (localPart) body.localPart = localPart;

  await busyWhile((async function () {
    var inbox = await fetchJson('/api/inboxes', { method: 'POST', body: body });
    els.localPartInput.value = '';
    await loadInboxes(inbox && inbox.address);
    if (inbox && inbox.address) showToast('Using ' + inbox.address);
  })());
}

// ---- Wiring ----------------------------------------------------------------

function handleError(err, fallback) {
  // A 401 from the gate means the login cookie is gone; no retry can fix that,
  // so the only useful response is the login view.
  if (err && err.status === 401) {
    forgetSessionId();
    showLoginView('Your session has expired. Please sign in again.');
    return;
  }
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

  // Authentication first: the login form and the app shell are wired before any
  // network call, so the first paint is usable whichever way the boot check goes.
  els.loginForm.addEventListener('submit', function (event) {
    event.preventDefault();
    submitLogin().catch(function (err) {
      showLoginError((err && err.message) || 'Could not sign in.');
    });
  });
  // Enter submits naturally inside a form; explicit handling covers browsers
  // that do not, and keeps focus in the field.
  els.passwordInput.addEventListener('keydown', function (event) {
    if (event.key !== 'Enter') return;
    event.preventDefault();
    submitLogin().catch(function (err) {
      showLoginError((err && err.message) || 'Could not sign in.');
    });
  });

  bind('settingsBtn', async function () { toggleSettings(); });
  bind('logoutBtn', submitLogout);
  els.passwordForm.addEventListener('submit', function (event) {
    event.preventDefault();
    submitPasswordChange().catch(function (err) {
      showPasswordStatus((err && err.message) || 'Could not update the password.', true);
    });
  });

  // API key management. Only a browser login can reach these routes; a key
  // cannot mint or revoke keys.
  els.apiKeyForm.addEventListener('submit', function (event) {
    event.preventDefault();
    submitCreateApiKey().catch(function (err) {
      showApiKeyStatus((err && err.message) || 'Could not create the key.', true);
    });
  });
  bind('apiKeyCopyBtn', copyApiKey);

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

  boot();
}

// boot decides which view to show. Nothing app-related is fetched before the
// server confirms the login cookie, so an unauthenticated visitor never
// triggers a protected request.
async function boot() {
  try {
    if (await checkAuth()) {
      await startApp();
      return;
    }
  } catch (err) {
    // An unreachable server leaves us on the login view with a clear message
    // rather than a blank page.
    els.loginView.hidden = false;
    els.appView.hidden = true;
    showLoginError((err && err.message) || 'Could not reach the Blinkmail server.');
    els.passwordInput.focus();
    return;
  }
  showLoginView('');
}

// startApp loads the application for an authenticated browser. Called after a
// successful login and on boot when the cookie is already valid.
async function startApp() {
  showAppView();
  clearError();
  els.messageList.setAttribute('aria-busy', 'true');
  try {
    await loadConfig();
    // The server only resumes inbox tokens it issued; anything else gets a
    // fresh one.
    await ensureSession();
    await loadInboxes();
    setInboxControlsEnabled(Boolean(els.inboxSelect.value));
  } catch (err) {
    handleError(err, 'Could not load Blinkmail.');
    // A 401 has already swapped in the login view; there is no app shell left to
    // fill with an empty state.
    if (!authState) return;
    showEmptyState('⚠️', 'Connection error',
      'Could not reach the Blinkmail server. Reload the page to try again.');
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
  } finally {
    els.messageList.setAttribute('aria-busy', 'false');
  }
}

init();
