/*
 * Frontend safety tests for Blinkmail.
 *
 * These prove the central security requirement from the plan: untrusted mail
 * content (sender, subject, body) and error text are rendered inertly, so
 * incoming HTML or script cannot execute in a reader's browser.
 *
 * Approach: the REAL shipped index.html is parsed into a linkedom DOM and the
 * REAL app.js is evaluated against it. app.js declares its rendering helpers
 * (renderMessages, showError, showEmptyState) as top-level functions, so after
 * evaluation they are reachable as globals. Each test calls those real
 * functions with attacker-controlled input and asserts on the resulting DOM.
 *
 * Why this catches regressions: linkedom's innerHTML setter parses markup into
 * real element nodes (it has no JS engine, so scripts don't run, but the nodes
 * exist). So if app.js were changed to render messages via innerHTML — as the
 * Tempik reference did — the payload would appear here as live
 * <script>/<img>/<a> elements and these assertions would fail. Rendering
 * through textContent/createElement leaves the same payload as inert text.
 *
 * Run with: npm test   (from the frontend/ directory)
 */
import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { parseHTML } from 'linkedom';

const ASSETS = new URL('../../internal/web/assets/', import.meta.url);
const HTML = readFileSync(new URL('index.html', ASSETS), 'utf8');
const APP_JS = readFileSync(new URL('app.js', ASSETS), 'utf8');

// Payloads designed to execute if they were ever parsed as HTML: a script tag,
// an onerror attribute, a javascript: URL, and an inline onclick.
const XSS = {
  from: '<img src=x onerror="window.__PWNED_FROM=1">attacker@evil.com',
  subject: '<script>window.__PWNED_SUBJ=1</script>Important',
  body:
    '<script>window.__PWNED_BODY=1</script>' +
    '<img src=x onerror="window.__PWNED_BODYIMG=1">' +
    '<a href="javascript:window.__PWNED_HREF=1">click</a>' +
    '<p onclick="window.__PWNED_CLICK=1">text</p>',
};

const POISON_GLOBALS = [
  '__PWNED_SUBJ', '__PWNED_BODY', '__PWNED_BODYIMG',
  '__PWNED_FROM', '__PWNED_HREF', '__PWNED_CLICK', '__PWNED_ERR',
];

let api; // the booted app's globals (document + app.js functions)

/**
 * Boot the real app.js against a fresh DOM built from the real index.html.
 *
 * Only the pieces app.js touches at load time are stubbed. We deliberately do
 * NOT drive the async network boot: linkedom's <select>.value emulation is
 * incomplete, and the security property under test lives in the synchronous
 * render functions, which we call directly below.
 */
function boot() {
  const { window, document } = parseHTML(HTML);

  const storage = {
    _m: new Map(),
    getItem(k) { return this._m.has(k) ? this._m.get(k) : null; },
    setItem(k, v) { this._m.set(k, String(v)); },
    removeItem(k) { this._m.delete(k); },
    clear() { this._m.clear(); },
  };

  window.localStorage = storage;
  window.setTimeout = setTimeout;
  window.clearTimeout = clearTimeout;
  window.isSecureContext = false;
  window.confirm = () => true;

  const define = (name, value) =>
    Object.defineProperty(globalThis, name, { value, writable: true, configurable: true });

  define('window', window);
  define('document', document);
  define('localStorage', storage);
  define('navigator', window.navigator || { clipboard: undefined });
  define('requestAnimationFrame', (cb) => setTimeout(cb, 0));
  // fetch returns a promise that never settles. app.js's init() kicks off an
  // async start() that begins with a fetch; parking it there means the
  // background chain never runs its success or error path, so it cannot mutate
  // #messageList and race the assertions below. The synchronous render
  // functions we test never touch fetch.
  define('fetch', () => new Promise(() => {}));

  // Evaluate the real client. app.js is strict-mode, so indirect eval scopes
  // its declarations and they do NOT leak to globalThis. We capture them
  // instead by appending an expression that references the real functions;
  // eval's completion value is that object. The captured functions are the
  // exact shipped ones — nothing is redefined here.
  const exported = (0, eval)(
    APP_JS + '\n;({ renderMessages, showError, showEmptyState, showToast });',
  );

  return { window, document, ...exported };
}

/** Count live elements that would indicate a payload was parsed as HTML. */
function dangerousNodes(root) {
  return {
    scripts: root.querySelectorAll('script').length,
    imgs: root.querySelectorAll('img').length,
    jsAnchors: root.querySelectorAll('a[href^="javascript:"]').length,
    onAttrs: [...root.querySelectorAll('*')].reduce(
      (n, el) => n + [...el.attributes].filter((a) => /^on/i.test(a.name)).length,
      0,
    ),
  };
}

const NO_DANGER = { scripts: 0, imgs: 0, jsAnchors: 0, onAttrs: 0 };

function assertNoPoisonRan(t) {
  for (const g of POISON_GLOBALS) {
    assert.equal(api.window[g], undefined, `${g} was set — a payload executed`);
  }
}

beforeEach(() => {
  // Fresh DOM + fresh globals per test so payloads cannot leak between tests.
  api = boot();
});

test('renderMessages renders untrusted mail content as inert text', (t) => {
  const messages = [
    {
      id: 'msg_1',
      inbox_address: 'victim@mail.example.com',
      from_address: XSS.from,
      subject: XSS.subject,
      body: XSS.body,
      received_at: '2026-10-21T06:00:00.000Z',
    },
  ];

  // Call the real rendering function used by loadMessages().
  api.renderMessages(messages);

  const list = api.document.getElementById('messageList');

  // Exactly one item rendered.
  assert.equal(list.querySelectorAll('.message-item').length, 1, 'expected one message item');

  // No live script/img/javascript-anchor/on* attribute may exist.
  assert.deepEqual(
    dangerousNodes(list),
    NO_DANGER,
    'message list contains live HTML nodes built from the payload',
  );

  // The payload must still be present, but as literal text — proving content
  // was neutralised, not dropped.
  const text = list.textContent;
  assert.ok(text.includes('<script>window.__PWNED_SUBJ=1</script>'), 'subject payload missing as text');
  assert.ok(text.includes('<img src=x onerror='), 'body img payload missing as text');
  assert.ok(text.includes('javascript:window.__PWNED_HREF=1'), 'anchor payload missing as text');

  assertNoPoisonRan(t);
});

test('renderMessages escapes a missing subject and body safely', () => {
  api.renderMessages([
    { id: 'm', inbox_address: 'v@mail.example.com', from_address: '', subject: '', body: '', received_at: '' },
  ]);
  const list = api.document.getElementById('messageList');
  assert.equal(list.querySelectorAll('.message-item').length, 1);
  assert.deepEqual(dangerousNodes(list), NO_DANGER);
});

test('showError renders a server error message as inert text', () => {
  // The reference interpolated err.message into innerHTML in its empty state.
  // Blinkmail routes it through textContent. Feed an XSS payload as the message.
  const payloadError = '<img src=x onerror="window.__PWNED_ERR=1"><script>window.__PWNED_ERR=1</script>boom';

  api.showError(payloadError);

  const banner = api.document.getElementById('errorBanner');
  const errorText = api.document.getElementById('errorText');

  assert.equal(banner.hidden, false, 'error banner should be visible');
  // Present verbatim as text...
  assert.ok(errorText.textContent.includes(payloadError), 'error message not shown as text');
  // ...and produced no live element in the banner.
  assert.deepEqual(dangerousNodes(banner), NO_DANGER, 'error banner contains live HTML nodes');

  assertNoPoisonRan();
});

test('showEmptyState renders titles/subtitles as inert text', () => {
  const payload = '<script>window.__PWNED_ERR=1</script><img src=x onerror="window.__PWNED_ERR=1">';

  api.showEmptyState('📬', payload, payload);

  const list = api.document.getElementById('messageList');
  assert.ok(list.querySelector('.empty-state'), 'empty state did not render');
  assert.deepEqual(dangerousNodes(list), NO_DANGER, 'empty state contains live HTML nodes');
  // Payload survived as literal text.
  assert.ok(list.textContent.includes('<script>'), 'empty-state payload not shown as text');

  assertNoPoisonRan();
});

test('showToast renders its message as inert text', () => {
  const payload = '<img src=x onerror="window.__PWNED_ERR=1">copied';

  api.showToast(payload);

  const container = api.document.getElementById('toastContainer');
  const toast = container.querySelector('.toast');
  assert.ok(toast, 'toast did not render');
  assert.deepEqual(dangerousNodes(container), NO_DANGER, 'toast contains live HTML nodes');
  assert.ok(toast.textContent.includes('<img src=x'), 'toast payload not shown as text');

  assertNoPoisonRan();
});
