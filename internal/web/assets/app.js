/*
 * Blinkmail web client.
 *
 * Untrusted text (sender, subject, plain-text bodies, error text) is rendered
 * through textContent and createElement, never innerHTML, so markup in a mail
 * body cannot execute in the reader's browser.
 *
 * An HTML mail part is different: it has to be rendered as HTML to be readable.
 * It is never injected into this document. Each message gets its own sandboxed
 * <iframe> whose contents are a string we assemble ourselves (see "HTML mail"
 * below): the mail is scrubbed, the frame cannot run script, and a CSP inside
 * it blocks everything except images and inline CSS. If the browser cannot
 * parse the mail, it falls back to showing the source as text.
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
  loginDomain: document.getElementById('loginDomain'),
  appView: document.getElementById('appView'),
  appTitle: document.getElementById('appTitle'),
  appSubtitle: document.getElementById('appSubtitle'),
  settingsBtn: document.getElementById('settingsBtn'),
  settingsPanel: document.getElementById('settingsPanel'),
  settingsBackdrop: document.getElementById('settingsBackdrop'),
  settingsClose: document.getElementById('settingsClose'),
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
  inboxList: document.getElementById('inboxList'),
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
  appName: 'BlinkMail',
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
    throw ApiError('Cannot reach the BlinkMail server. Check your connection.', 0);
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
  els.settingsBackdrop.hidden = true;
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

// openSettings shows the drawer over the app. The backdrop is a separate node
// so the panel can animate independently and clicks outside it are easy to
// distinguish from clicks inside it.
function openSettings() {
  els.settingsPanel.hidden = false;
  els.settingsBackdrop.hidden = false;
  els.settingsBtn.setAttribute('aria-expanded', 'true');
  // Focus the drawer itself, not a button inside it: the first Tab then lands
  // on Close, and a mouse user does not get a focus ring on a button they did
  // not press.
  els.settingsPanel.focus();
  // Key metadata is only loaded when the panel is opened, so a browser that
  // never visits settings makes no extra requests.
  loadApiKeys().catch(function (err) {
    showApiKeyStatus((err && err.message) || 'Could not load API keys.', true);
  });
}

function closeSettings() {
  els.settingsPanel.hidden = true;
  els.settingsBackdrop.hidden = true;
  els.settingsBtn.setAttribute('aria-expanded', 'false');
  // Closing drops the one-time plaintext from the DOM.
  els.apiKeyReveal.hidden = true;
  els.apiKeyValue.value = '';
  els.settingsBtn.focus();
}

function toggleSettings() {
  if (els.settingsPanel.hidden) {
    openSettings();
  } else {
    closeSettings();
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
  els.appSubtitle.textContent = appConfig.mailDomain;
  els.loginDomain.textContent = appConfig.mailDomain;

  clear(els.domainSelect);
  appConfig.mailDomains.forEach(function (domain) {
    var option = el('option', null, '@' + domain);
    option.value = domain;
    els.domainSelect.appendChild(option);
  });
  // With a single domain there is nothing to choose.
  els.domainSelect.style.display = appConfig.mailDomains.length <= 1 ? 'none' : '';

  els.localPartInput.placeholder = 'username, or blank for random';
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
    renderInboxList([]);
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
    showEmptyState('✉', 'No inboxes yet',
      'Create a disposable address to start receiving mail.');
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

  renderInboxList(addresses);
  setInboxControlsEnabled(true);
  await loadMessages();
}

// renderInboxList draws the sidebar list. Clicking a row sets the hidden
// select and reloads, so the select stays the only source of truth for which
// address is current — the list is presentation, not state.
function renderInboxList(addresses) {
  clear(els.inboxList);

  if (addresses.length === 0) {
    els.inboxList.appendChild(el('p', 'inbox-empty',
      'No addresses yet. Create one with New.'));
    return;
  }

  addresses.forEach(function (address) {
    var row = el('button', 'inbox-item');
    row.type = 'button';
    row.setAttribute('data-address', address);

    // Split at the last @ so only the local part can truncate: the domain is
    // the part a reader uses to recognise the address, so it is never cut.
    var at = address.lastIndexOf('@');
    var label = el('span', 'inbox-address');
    label.appendChild(el('span', 'inbox-local', at > 0 ? address.slice(0, at) : address));
    if (at > 0) label.appendChild(el('span', 'inbox-domain', address.slice(at)));
    row.appendChild(label);

    if (address === els.inboxSelect.value) row.setAttribute('aria-current', 'true');
    row.addEventListener('click', function () {
      if (els.inboxSelect.value === address) return;
      els.inboxSelect.value = address;
      clearError();
      loadMessages().catch(function (err) { handleError(err, 'Could not load messages.'); });
    });

    els.inboxList.appendChild(row);
  });
}

// syncActiveInbox moves the aria-current marker without re-rendering the list,
// which would otherwise rebuild (and re-announce) every row on each change.
function syncActiveInbox(address) {
  var rows = els.inboxList.querySelectorAll('.inbox-item');
  for (var i = 0; i < rows.length; i++) {
    var isCurrent = rows[i].getAttribute('data-address') === address;
    if (isCurrent) {
      rows[i].setAttribute('aria-current', 'true');
    } else {
      rows[i].removeAttribute('aria-current');
    }
  }
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
  syncActiveInbox(address);
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

// ---- HTML mail -------------------------------------------------------------
//
// An HTML part is rendered, but never in this document: each message gets its
// own <iframe> whose content is a string assembled here. Three independent
// layers keep it inert:
//
//   1. the markup is scrubbed (script-bearing elements and event handlers
//      removed) before it goes anywhere near a parser;
//   2. the frame is sandboxed WITHOUT allow-scripts, so no script in it can
//      ever run, and it cannot touch this page's DOM or storage;
//   3. a Content-Security-Policy inside the frame blocks every subresource
//      except images and inline styles.
//
// Layer 2 alone is sufficient, which is why a scrubber bug is not a code
// execution bug here.

// FRAME_CSP is the policy the frame's own document declares.
var FRAME_CSP = [
  "default-src 'none'",
  'img-src data: cid: http: https:',
  "style-src 'unsafe-inline'",
  'font-src data:',
  'media-src data:',
  "form-action 'none'",
  "base-uri 'none'",
].join('; ');

// FRAME_CSS keeps mail legible on the app's dark surface and stops wide tables
// and images from forcing a horizontal scrollbar.
var FRAME_CSS = [
  ':root { color-scheme: dark; }',
  'html, body { margin: 0; padding: 0; background: transparent; }',
  'body { padding: 2px 2px 4px; font-family: ui-sans-serif, system-ui, ' +
    '-apple-system, "Segoe UI", Roboto, sans-serif; font-size: 13px; ' +
    'line-height: 1.62; color: #c9cdd6; overflow-wrap: anywhere; }',
  'img { max-width: 100%; height: auto; }',
  'table { max-width: 100%; border-collapse: collapse; }',
  'pre, code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; ' +
    'white-space: pre-wrap; overflow-wrap: anywhere; }',
  'blockquote { margin: 0 0 0 12px; padding-left: 10px; ' +
    'border-left: 2px solid #2b2e35; color: #9aa1ad; }',
  'a { color: #ff8a5c; }',
  'hr { border: 0; border-top: 1px solid #2b2e35; }',
].join('\n');

// DROP_TAGS are removed together with their contents: script-bearing or
// navigation-bearing elements that legitimate mail does not need, and that
// would only add attack surface inside the frame.
var DROP_TAGS = {
  script: true, noscript: true, iframe: true, frame: true, frameset: true,
  object: true, embed: true, applet: true, param: true, link: true,
  meta: true, base: true, template: true,
  input: true, select: true, option: true, optgroup: true, textarea: true,
};

// UNWRAP_TAGS are replaced by their contents rather than deleted: real mail
// puts the important words ("Reset password", "Confirm subscription") inside a
// form and a button, so dropping the element must not drop the text.
var UNWRAP_TAGS = { form: true, button: true };

// DROP_ATTRS are removed by name wherever they appear. on* is handled
// separately because it is a prefix.
var DROP_ATTRS = {
  srcdoc: true, formaction: true, action: true, ping: true, srcset: true,
  dynsrc: true, lowsrc: true, background: true, 'http-equiv': true,
  'xlink:href': true, xmlns: true,
};

// URL_ATTRS are attributes whose value is a URL and must pass isSafeUrl.
var URL_ATTRS = { href: true, src: true, cite: true, poster: true, data: true };

var SAFE_STYLE = /expression\s*\(|javascript\s*:|vbscript\s*:|-moz-binding|behavior\s*:/i;

// parseMailFragment parses HTML into a detached container so it can be walked
// and rewritten. innerHTML on a node that is not in the document never runs
// script and never fetches resources, which is why it is safe for hostile
// input. Returns null when the platform has no parser, so callers can fall
// back to plain text.
function parseMailFragment(html) {
  try {
    var box = document.createElement('div');
    box.innerHTML = String(html);
    return box;
  } catch (err) {
    return null;
  }
}

// compactUrl lowercases a URL and strips the whitespace and control characters
// that browsers ignore, so "java\nscript:alert(1)" cannot slip past a check.
function compactUrl(value) {
  return String(value).replace(/[\u0000-\u0020\u00a0]+/g, '').toLowerCase();
}

// isSafeUrl allows only the schemes mail legitimately links to. Anything else
// (javascript:, vbscript:, data:text/html, blob:, relative paths that would
// resolve against the frame) is refused.
function isSafeUrl(tagName, attrName, value) {
  var url = compactUrl(value);
  if (url === '' || url.charAt(0) === '#') return true;
  if (url.indexOf('http://') === 0 || url.indexOf('https://') === 0) return true;
  if (attrName === 'href' && tagName === 'a' &&
      (url.indexOf('mailto:') === 0 || url.indexOf('tel:') === 0)) {
    return true;
  }
  if (attrName === 'src' && tagName === 'img' &&
      (url.indexOf('data:image/') === 0 || url.indexOf('cid:') === 0)) {
    return true;
  }
  return false;
}

// unwrapNode replaces an element with its children, keeping the text but
// removing the element itself.
function unwrapNode(node) {
  var parent = node.parentNode;
  if (!parent) return;
  while (node.firstChild) parent.insertBefore(node.firstChild, node);
  parent.removeChild(node);
}

// scrubNode rewrites one element in place. It runs before the fragment is
// serialised into the frame, so the frame's document is already clean.
function scrubNode(node) {
  var tag = String(node.tagName || '').toLowerCase();

  if (DROP_TAGS[tag]) {
    if (node.parentNode) node.parentNode.removeChild(node);
    return;
  }
  if (UNWRAP_TAGS[tag]) {
    unwrapNode(node);
    return;
  }

  var attrs = [];
  for (var i = 0; i < node.attributes.length; i++) {
    attrs.push(node.attributes[i].name);
  }

  for (var j = 0; j < attrs.length; j++) {
    var name = attrs[j];
    var lower = String(name).toLowerCase();
    var value = node.getAttribute(name);

    if (lower.indexOf('on') === 0 && lower.length > 2) {
      node.removeAttribute(name);
      continue;
    }
    if (DROP_ATTRS[lower]) {
      node.removeAttribute(name);
      continue;
    }
    if (lower === 'style' && SAFE_STYLE.test(String(value))) {
      node.removeAttribute(name);
      continue;
    }
    if (URL_ATTRS[lower] && !isSafeUrl(tag, lower, value)) {
      node.removeAttribute(name);
    }
  }

  // A surviving link opens in a new tab: it must never replace the message
  // with someone else's page.
  if (tag === 'a' && node.getAttribute('href')) {
    node.setAttribute('target', '_blank');
    node.setAttribute('rel', 'noopener noreferrer nofollow');
  }
}

// scrubFragment walks the parsed fragment depth-first, removing dropped
// elements and attributes. The walk is done on a snapshot of the children
// because scrubbing can detach nodes.
function scrubFragment(root) {
  var nodes = root.querySelectorAll('*');
  for (var i = 0; i < nodes.length; i++) {
    // A node inside a dropped parent is already gone from the tree; scrubbing
    // it again is harmless but pointless, so skip detached nodes.
    if (!root.contains(nodes[i])) continue;
    scrubNode(nodes[i]);
  }
}

// sanitizeMailHtml returns the scrubbed markup, or null when the platform has
// no HTML parser and the caller must fall back to text.
function sanitizeMailHtml(html) {
  var box = parseMailFragment(html);
  if (!box) return null;
  scrubFragment(box);
  return box.innerHTML;
}

// escapeHtml is the last-resort fallback: if the mail cannot be parsed, it is
// shown as literal text inside the frame rather than being trusted.
function escapeHtml(text) {
  return String(text).replace(/[&<>"']/g, function (c) {
    switch (c) {
      case '&': return '&amp;';
      case '<': return '&lt;';
      case '>': return '&gt;';
      case '"': return '&quot;';
      default: return '&#39;';
    }
  });
}

// mailFrameDocument assembles the complete document for one message. The CSP
// meta comes first so it governs everything the mail itself contributes.
function mailFrameDocument(html) {
  var clean = sanitizeMailHtml(html);
  var body = clean === null ? '<pre>' + escapeHtml(html) + '</pre>' : clean;
  return '<!doctype html><html><head><meta charset="utf-8">' +
    '<meta http-equiv="Content-Security-Policy" content="' + FRAME_CSP + '">' +
    '<base target="_blank">' +
    '<style>' + FRAME_CSS + '</style>' +
    '</head><body>' + body + '</body></html>';
}

// htmlToText extracts the readable text of an HTML part for the collapsed
// preview, so a closed message never needs a frame (and never fetches a remote
// image) just to show two lines.
function htmlToText(html) {
  var box = parseMailFragment(html);
  if (!box) return '';
  scrubFragment(box);
  var drop = box.querySelectorAll('script, style, head, title');
  for (var i = 0; i < drop.length; i++) {
    if (drop[i].parentNode) drop[i].parentNode.removeChild(drop[i]);
  }
  var text = box.textContent || '';
  return text.replace(/\s+/g, ' ').trim();
}

// MAIL_FRAME_MIN keeps a one-line message from collapsing into a sliver.
var MAIL_FRAME_MIN = 48;
// MAIL_FRAME_SHARE is the largest fraction of the viewport a single message
// body may occupy before it starts scrolling internally.
var MAIL_FRAME_SHARE = 0.62;

// measureMailFrame returns the height the mail actually needs.
//
// A document's scrollHeight is never smaller than its viewport, so measuring
// while the frame already has a height can only ever grow it (and a short
// message would stay stuck at the fallback height). The frame is collapsed for
// the measurement instead: layout is synchronous, so the value is read before
// anything is painted and no flicker is visible.
function measureMailFrame(frame, doc) {
  var previous = frame.style.height;
  frame.style.height = '0px';

  var body = doc.body;
  var content = Math.max(
    doc.documentElement.scrollHeight || 0,
    body ? (body.scrollHeight || 0) : 0,
    // A body sized to 100% reports nothing useful; its box does.
    body && body.getBoundingClientRect
      ? (body.getBoundingClientRect().height || 0)
      : 0);

  frame.style.height = previous;
  return content;
}

// fitMailFrame sizes the frame to its content, up to a share of the viewport.
// Beyond that the frame keeps its capped height and the mail scrolls inside
// it, which is what makes a long newsletter readable without pushing the rest
// of the inbox off screen.
function fitMailFrame(frame) {
  var doc = null;
  try {
    doc = frame.contentDocument;
  } catch (err) {
    doc = null;
  }
  // Without same-origin access there is nothing to measure; the CSS height
  // stands and the frame still scrolls.
  if (!doc || !doc.documentElement) return;

  var content = measureMailFrame(frame, doc);
  if (!content) return;

  var viewport = window.innerHeight || 800;
  var cap = Math.max(MAIL_FRAME_MIN, Math.round(viewport * MAIL_FRAME_SHARE));
  // Rounded up: a fractional height can leave a one-pixel scrollbar on short
  // mail that would otherwise fit exactly.
  var height = Math.min(Math.max(Math.ceil(content), MAIL_FRAME_MIN), cap);

  frame.style.height = height + 'px';
  frame.classList.toggle('is-capped', content > cap);
}

// createMailFrame builds the sandboxed frame for one HTML body.
//
// allow-same-origin is needed only to measure the content so the frame can fit
// short mail; it grants no scripting power on its own (the combination with
// allow-scripts is what is dangerous, and allow-scripts is never set).
// allow-popups lets a link the reader clicks open in a new tab instead of
// replacing the message.
function createMailFrame(html, title) {
  var frame = document.createElement('iframe');
  frame.className = 'message-html-frame';
  frame.setAttribute('sandbox', 'allow-same-origin allow-popups allow-popups-to-escape-sandbox');
  frame.setAttribute('referrerpolicy', 'no-referrer');
  frame.setAttribute('title', title ? 'Message body: ' + title : 'Message body');
  frame.setAttribute('srcdoc', mailFrameDocument(html));

  frame.addEventListener('load', function () {
    fitMailFrame(frame);
    // Images arrive after the load event and change the height; re-measuring
    // once they settle keeps the frame from clipping a picture.
    var doc = null;
    try {
      doc = frame.contentDocument;
    } catch (err) {
      doc = null;
    }
    if (doc && doc.images) {
      for (var i = 0; i < doc.images.length; i++) {
        doc.images[i].addEventListener('load', function () { fitMailFrame(frame); });
        doc.images[i].addEventListener('error', function () { fitMailFrame(frame); });
      }
    }
    window.setTimeout(function () { fitMailFrame(frame); }, 300);
  });

  return frame;
}

// refitMailFrames re-measures every frame on screen after a resize, since the
// viewport share that caps them has changed.
function refitMailFrames() {
  var hosts = document.querySelectorAll('.message-html');
  for (var i = 0; i < hosts.length; i++) {
    // A hidden host has no layout, so measuring it would only return 0.
    if (hosts[i].hidden || !hosts[i].firstChild) continue;
    fitMailFrame(hosts[i].firstChild);
  }
}

// renderMessages builds the message list with DOM nodes only.
//
// from_address, subject and plain-text bodies come from strangers' email, so
// they are assigned to textContent. An HTML body is rendered as HTML, but only
// inside the sandboxed frame described above, and only once the message is
// opened — a collapsed row shows a text preview so listing an inbox never
// loads remote images.
function renderMessages(messages) {
  els.messageCount.textContent = messages.length +
    (messages.length === 1 ? ' message' : ' messages');
  clear(els.messageList);

  if (messages.length === 0) {
    showEmptyState('✉', 'Inbox empty',
      'Mail sent to this address lands here. Refresh after sending.');
    return;
  }

  messages.forEach(function (msg) {
    var item = el('article', 'message-item');
    // Rows expand in place rather than opening a route: there is no history to
    // preserve, and the full body is already in the DOM.
    item.setAttribute('tabindex', '0');
    item.setAttribute('role', 'button');
    item.setAttribute('aria-expanded', 'false');

    var top = el('div', 'message-top');
    top.appendChild(el('span',
      'message-from' + (msg.from_address ? '' : ' is-empty'),
      msg.from_address || 'unknown sender'));
    top.appendChild(el('time', 'message-time', formatListTime(msg.received_at)));

    var subject = el('h3',
      'message-subject' + (msg.subject ? '' : ' is-empty'),
      msg.subject || '(no subject)');

    var html = typeof msg.body_html === 'string' ? msg.body_html.trim() : '';
    var text = msg.body === undefined || msg.body === null ? '' : String(msg.body).trim();

    // The visible paragraph is always plain text: either the plain part, or a
    // text preview of the HTML part. An HTML body therefore needs no frame —
    // and fetches no remote image — until it is opened.
    var preview = html ? htmlToText(html) : text;
    if (!preview) preview = text;
    var body = el('p', 'message-body', preview);

    item.appendChild(top);
    item.appendChild(subject);
    item.appendChild(body);

    if (html) {
      var frameHost = el('div', 'message-html');
      frameHost.hidden = true;
      // Kept as a property, not an attribute: it is the raw mail and must not
      // be serialisable back into the page.
      frameHost.mailHtml = html;
      frameHost.mailSubject = msg.subject || '(no subject)';
      item.appendChild(frameHost);
    }

    item.addEventListener('click', function () { toggleMessage(item); });
    item.addEventListener('keydown', function (event) {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      toggleMessage(item);
    });

    els.messageList.appendChild(item);
  });
}

// toggleMessage expands one message. Collapsed bodies are clamped by CSS, so
// the text is always present for search, copy and screen readers.
//
// For an HTML body the frame is built on first expand and then reused, so
// opening and closing a message is free after the first time and a closed
// message never holds a live document.
function toggleMessage(item) {
  var open = item.classList.toggle('is-open');
  item.setAttribute('aria-expanded', open ? 'true' : 'false');

  var host = item.querySelector('.message-html');
  if (!host) return;

  var preview = item.querySelector('.message-body');
  if (open) {
    if (!host.firstChild && host.mailHtml) {
      host.appendChild(createMailFrame(host.mailHtml, host.mailSubject));
    }
    host.hidden = false;
    if (preview) preview.hidden = true;
    // A frame built while its host was hidden could not be measured, so it is
    // measured again now that it has layout.
    if (host.firstChild) fitMailFrame(host.firstChild);
  } else {
    host.hidden = true;
    if (preview) preview.hidden = false;
  }
}

// formatListTime keeps the list column narrow: the time for today, the word
// for yesterday, the date for anything older.
function formatListTime(value) {
  if (!value) return '';
  var date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);

  var now = new Date();
  var startOfToday = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  var t = date.getTime();
  if (t >= startOfToday.getTime()) {
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }
  if (t >= startOfToday.getTime() - 86400000) return 'Yesterday';
  return date.toLocaleDateString([], { day: '2-digit', month: 'short' });
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
  bind('settingsClose', async function () { closeSettings(); });
  els.settingsBackdrop.addEventListener('click', closeSettings);
  // Escape closes the drawer from anywhere inside it.
  document.addEventListener('keydown', function (event) {
    if (event.key === 'Escape' && !els.settingsPanel.hidden) closeSettings();
  });
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

  // Mail frames are capped at a share of the viewport, so a resize changes how
  // tall each one may be.
  window.addEventListener('resize', refitMailFrames);

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
    showLoginError((err && err.message) || 'Could not reach the BlinkMail server.');
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
    handleError(err, 'Could not load BlinkMail.');
    // A 401 has already swapped in the login view; there is no app shell left to
    // fill with an empty state.
    if (!authState) return;
    showEmptyState('⚠️', 'Connection error',
      'Could not reach the BlinkMail server. Reload the page to try again.');
    els.currentInbox.textContent = 'No inbox selected';
    els.messageCount.textContent = '0 messages';
  } finally {
    els.messageList.setAttribute('aria-busy', 'false');
  }
}

init();
