/*
 * Frontend safety tests for Blinkmail.
 *
 * These prove the central security requirements of the reader:
 *
 *  1. untrusted text (sender, subject, plain-text body, error text) is rendered
 *     inertly, so incoming HTML or script cannot execute in a reader's browser;
 *  2. an HTML mail part IS rendered as markup, but only inside a sandboxed
 *     frame that cannot run script, cannot reach the app's document, and whose
 *     content is scrubbed before it is placed there.
 *
 * Approach: the REAL shipped index.html is parsed into a linkedom DOM and the
 * REAL app.js is evaluated against it. app.js declares its rendering helpers
 * (renderMessages, showError, showEmptyState, ...) as top-level functions, so
 * after evaluation they are reachable as globals. Each test calls those real
 * functions with attacker-controlled input and asserts on the resulting DOM.
 *
 * Why this catches regressions: linkedom's innerHTML setter parses markup into
 * real element nodes (it has no JS engine, so scripts don't run, but the nodes
 * exist). So if app.js were changed to render messages via innerHTML, the
 * payload would appear here as live
 * <script>/<img>/<a> elements and these assertions would fail. Rendering
 * through textContent/createElement leaves the same payload as inert text.
 *
 * The HTML-mail tests inspect the frame document string (the iframe's srcdoc)
 * that the real renderer produced, which is exactly what a browser would load.
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
  window.innerHeight = 900;
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
    APP_JS +
      '\n;({ renderMessages, renderApiKeys, showError, showEmptyState, showToast,' +
      ' toggleMessage, createMailFrame, mailFrameDocument, sanitizeMailHtml,' +
      ' htmlToText, fitMailFrame, refitMailFrames });',
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

test('renderApiKeys renders key names as inert text', () => {
  // Key names are operator input, so they take the same textContent path as
  // mail. The one-time plaintext is never part of this render at all.
  api.renderApiKeys([
    {
      id: 'k1',
      name: XSS.subject,
      prefix: 'bm_9a1b2c3d4',
      created_at: '2026-10-21T06:00:00.000Z',
      last_used_at: null,
    },
  ]);

  const list = api.document.getElementById('apiKeyList');
  assert.equal(list.querySelectorAll('.api-key-item').length, 1, 'expected one key item');
  assert.deepEqual(dangerousNodes(list), NO_DANGER, 'key list contains live HTML nodes');
  assert.ok(
    list.textContent.includes('<script>window.__PWNED_SUBJ=1</script>'),
    'key name payload missing as text',
  );
  assert.ok(list.textContent.includes('bm_9a1b2c3d4'), 'prefix missing');
  assert.ok(list.textContent.includes('never'), 'unused key should read as never used');

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

// ---------------------------------------------------------------------------
// HTML mail
//
// An HTML part must be rendered as markup (that is the point), but only inside
// a frame that cannot run script. These tests drive the real renderer and then
// inspect the frame document string it produced.
// ---------------------------------------------------------------------------

const HTML_BODY =
  '<div style="font-family:Arial">' +
  '<h1>Hello</h1><p>Real <b>markup</b> renders here.</p>' +
  '<script>window.__PWNED_HTML=1</script>' +
  '<img src="https://tracker.example/pixel.gif" onerror="window.__PWNED_HTML=1">' +
  '<a href="javascript:window.__PWNED_HTML=1">bad link</a>' +
  '<a href="https://example.com/real">real link</a>' +
  '<iframe src="https://evil.example/x"></iframe>' +
  '<form action="https://evil.example"><input name="x">' +
  '<button>Confirm subscription</button></form>' +
  '</div>';

function htmlMessage(html) {
  return {
    id: 'msg_html',
    inbox_address: 'victim@mail.example.com',
    from_address: 'news@example.org',
    subject: 'Weekly digest',
    body: 'text fallback',
    body_html: html,
    received_at: '2026-10-21T06:00:00.000Z',
  };
}

test('an HTML body is rendered as markup, inside a scriptless sandboxed frame', () => {
  api.renderMessages([htmlMessage(HTML_BODY)]);

  const list = api.document.getElementById('messageList');
  const item = list.querySelector('.message-item');
  assert.ok(item, 'message did not render');

  // Collapsed: a text preview only. No frame exists yet, so listing an inbox
  // never loads a remote image or builds a document per message.
  assert.equal(item.querySelectorAll('iframe').length, 0, 'frame built before expanding');
  const preview = item.querySelector('.message-body');
  assert.ok(preview.textContent.includes('Real markup renders here'), 'preview missing mail text');
  assert.ok(!preview.textContent.includes('<b>'), 'preview leaked raw markup');

  api.toggleMessage(item);

  const frame = item.querySelector('iframe');
  assert.ok(frame, 'expanding an HTML message did not create a frame');

  // The sandbox must never grant scripting.
  const sandbox = frame.getAttribute('sandbox') || '';
  assert.ok(!/allow-scripts/.test(sandbox), 'sandbox allows scripts: ' + sandbox);

  const doc = frame.getAttribute('srcdoc') || '';

  // The frame declares its own policy: nothing loads except images and CSS.
  assert.ok(doc.includes('Content-Security-Policy'), 'frame document has no CSP');
  assert.ok(doc.includes("default-src 'none'"), 'CSP does not deny by default');

  // Scrubbed: no script, no handlers, no javascript: URL, no nested frame or
  // form that could navigate or submit somewhere.
  assert.ok(!/<script/i.test(doc), 'script tag survived scrubbing');
  assert.ok(!/onerror/i.test(doc), 'event handler survived scrubbing');
  assert.ok(!/javascript:/i.test(doc), 'javascript: URL survived scrubbing');
  assert.ok(!/<iframe/i.test(doc), 'nested iframe survived scrubbing');
  assert.ok(!/<form/i.test(doc), 'form survived scrubbing');
  assert.ok(!/<input/i.test(doc), 'form control survived scrubbing');

  // ...while the actual markup is preserved, which is what was asked for.
  assert.ok(doc.includes('<b>markup</b>'), 'HTML body was not rendered as markup');
  assert.ok(doc.includes('https://example.com/real'), 'safe link was dropped');
  assert.ok(doc.includes('target="_blank"'), 'link not forced into a new tab');

  // The app's own document stayed inert: nothing from the mail was parsed into
  // it, so no live script/img/on* exists there.
  assert.deepEqual(dangerousNodes(list), NO_DANGER, 'mail leaked into the app document');
  assert.equal(api.window.__PWNED_HTML, undefined, 'mail script executed');
});

test('collapsing an HTML message hides the frame and restores the preview', () => {
  api.renderMessages([htmlMessage('<p>hello</p>')]);
  const item = api.document.getElementById('messageList').querySelector('.message-item');
  const preview = item.querySelector('.message-body');

  api.toggleMessage(item);
  const frame = item.querySelector('iframe');
  assert.equal(frame.parentNode.hidden, false, 'frame host still hidden after expanding');
  assert.equal(preview.hidden, true, 'preview not hidden behind the frame');

  api.toggleMessage(item);
  assert.equal(frame.parentNode.hidden, true, 'frame host not hidden after collapsing');
  assert.equal(preview.hidden, false, 'preview not restored after collapsing');
  // Re-expanding reuses the frame instead of rebuilding (and reloading) it.
  api.toggleMessage(item);
  assert.equal(item.querySelectorAll('iframe').length, 1, 'frame was rebuilt');
});

test('a long HTML body is capped and scrolls inside its frame', () => {
  // 900px viewport * 0.62 share = 558px cap.
  const stub = (contentHeight) => {
    const toggles = {};
    return {
      style: {},
      classList: {
        toggle(name, on) { toggles[name] = on; },
      },
      toggles,
      contentDocument: {
        documentElement: { scrollHeight: contentHeight },
        body: { scrollHeight: contentHeight },
      },
    };
  };

  const short = stub(120);
  api.fitMailFrame(short);
  assert.equal(short.style.height, '120px', 'short mail should fit its content');
  assert.equal(short.toggles['is-capped'], false, 'short mail should not be capped');

  const long = stub(4000);
  api.fitMailFrame(long);
  assert.equal(long.style.height, '558px', 'long mail should stop at the viewport cap');
  assert.equal(long.toggles['is-capped'], true, 'long mail should scroll inside the frame');

  // A one-line message must not become a sliver.
  const tiny = stub(4);
  api.fitMailFrame(tiny);
  assert.equal(tiny.style.height, '48px', 'tiny mail should use the minimum height');
});

test('htmlToText produces a one-line preview and never runs or keeps markup', () => {
  const text = api.htmlToText(
    '<style>p{color:red}</style><p>First line</p>\n\n<p>Second   line</p>' +
      '<script>window.__PWNED_HTML=1</script>',
  );
  assert.ok(text.includes('First line'), 'preview lost the body text');
  assert.ok(text.includes('Second line'), 'preview lost the second paragraph');
  assert.ok(!text.includes('<'), 'preview kept markup');
  assert.ok(!text.includes('color:red'), 'preview kept stylesheet text');
  assert.equal(api.window.__PWNED_HTML, undefined, 'preview executed mail script');
});

test('sanitizeMailHtml keeps formatting and drops every executable construct', () => {
  const clean = api.sanitizeMailHtml(HTML_BODY);
  assert.ok(typeof clean === 'string', 'sanitizer returned no markup');

  assert.ok(clean.includes('<h1>Hello</h1>'), 'formatting was stripped');
  assert.ok(clean.includes('<b>markup</b>'), 'formatting was stripped');
  assert.ok(!/<script/i.test(clean), 'script survived');
  assert.ok(!/onerror/i.test(clean), 'handler survived');
  assert.ok(!/javascript:/i.test(clean), 'javascript: URL survived');
  assert.ok(!/<form|<input|<button|<iframe/i.test(clean), 'dangerous element survived');
  // The words inside a form/button are the point of a call-to-action mail, so
  // the element is unwrapped rather than dropped with its text.
  assert.ok(clean.includes('Confirm subscription'), 'call-to-action text was dropped');

  // Whitespace and control characters inside a scheme must not defeat the check.
  const smuggled = api.sanitizeMailHtml('<a href="java\n\tscript:alert(1)">x</a>');
  assert.ok(!/javascript:/i.test(smuggled.replace(/\s/g, '')), 'obfuscated javascript: URL survived');

  // A relative URL would resolve against the frame and is refused too.
  const relative = api.sanitizeMailHtml('<img src="/api/keys">');
  assert.ok(!relative.includes('/api/keys'), 'relative URL survived');
});

test('a message with no HTML part still renders as plain text', () => {
  api.renderMessages([
    {
      id: 'm_plain',
      inbox_address: 'v@mail.example.com',
      from_address: 'a@example.org',
      subject: 'Plain',
      body: 'line one\nline two',
      received_at: '2026-10-21T06:00:00.000Z',
    },
  ]);

  const item = api.document.getElementById('messageList').querySelector('.message-item');
  assert.equal(item.querySelectorAll('iframe').length, 0, 'plain text got a frame');
  assert.equal(item.querySelector('.message-html'), null, 'plain text got a frame host');
  assert.ok(item.querySelector('.message-body').textContent.includes('line two'), 'body missing');
});
