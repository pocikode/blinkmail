/*
 * Tests for the Blinkmail Email Worker relay using Node's built-in test runner.
 *
 * No test framework or network is required: fetch and the email event are
 * mocked. Run with `node --test` from the relay directory.
 */
import { test, describe, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';

import { handleEmail, MAX_RAW_BYTES } from '../worker.js';

const SECRET = 'relay-test-secret-value-at-least-32-chars';
const URL = 'https://mail.example.com/api/inbound';
const ENV = { INBOUND_URL: URL, INBOUND_SECRET: SECRET };

/** Build a fake ReadableStream from a string. */
function rawStream(text) {
  const bytes = new TextEncoder().encode(text);
  return {
    async *reader() {
      yield bytes;
    },
    getReader() {
      const it = this.reader();
      return {
        read: () => it.next().then((r) => (r.done ? { done: true, value: undefined } : { done: false, value: r.value })),
        cancel: async () => {},
      };
    },
  };
}

/** Build a fake email event. */
function makeEvent({ from = 'sender@gmail.com', to = 'user@mail.example.com', raw = 'Subject: hi\n\nbody', rawSize, rejects = [] }) {
  return {
    from,
    to,
    raw: rawStream(raw),
    rawSize: rawSize === undefined ? new TextEncoder().encode(raw).length : rawSize,
    setReject(reason) { rejects.push(reason); },
    _rejects: rejects,
  };
}

/** Install a mock global fetch returning a canned response. */
function mockFetch(response, capture) {
  const original = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    if (capture) capture.push({ url, init });
    return response;
  };
  return () => { globalThis.fetch = original; };
}

/** A minimal Response-like object. */
function res(status, body = '') {
  return { status, text: async () => body };
}

describe('relay worker', () => {
  let restore;
  afterEach(() => { if (restore) restore(); restore = null; });

  test('forwards raw MIME with bearer auth and envelope headers', async () => {
    const calls = [];
    restore = mockFetch(res(200, '{"ok":true}'), calls);
    const event = makeEvent({ from: 'Alice <alice@example.org>', to: 'Box@Mail.Example.com' });

    await handleEmail(event, ENV);

    assert.equal(calls.length, 1);
    const { url, init } = calls[0];
    assert.equal(url, URL);
    assert.equal(init.method, 'POST');
    assert.equal(init.headers.Authorization, `Bearer ${SECRET}`);
    assert.equal(init.headers['Content-Type'], 'message/rfc822');
    // Envelope recipient is lowercased and used verbatim (not the MIME To).
    assert.equal(init.headers['X-Envelope-To'], 'box@mail.example.com');
    assert.equal(init.headers['X-Envelope-From'], 'alice <alice@example.org>');
    assert.equal(init.headers['Content-Length'], String(init.body.byteLength));
    // Body is exactly the raw bytes.
    assert.equal(new TextDecoder().decode(init.body), 'Subject: hi\n\nbody');
    assert.equal(event._rejects.length, 0);
  });

  test('treats a 200 duplicate acknowledgement as success', async () => {
    const calls = [];
    restore = mockFetch(res(200, '{"ok":true,"duplicate":true}'), calls);
    const event = makeEvent({});

    await handleEmail(event, ENV); // resolves without throwing

    assert.equal(calls.length, 1);
    assert.equal(event._rejects.length, 0);
  });

  test('rejects permanently when reported rawSize exceeds the limit', async () => {
    restore = mockFetch(res(200), []);
    const event = makeEvent({ rawSize: MAX_RAW_BYTES + 1 });

    await handleEmail(event, ENV); // permanent rejection returns normally

    assert.equal(event._rejects.length, 1);
    assert.match(event._rejects[0], /too large/i);
  });

  test('rejects permanently when the actual stream exceeds the limit', async () => {
    restore = mockFetch(res(200), []);
    // rawSize lies (says small) but the stream is big, with a small env limit.
    const big = 'Subject: x\n\n' + 'A'.repeat(5000);
    const event = makeEvent({ raw: big, rawSize: 10 });

    await handleEmail(event, { ...ENV, MAX_RAW_BYTES: '1024' });

    assert.equal(event._rejects.length, 1);
    assert.match(event._rejects[0], /too large/i);
  });

  test('rejects permanently on a missing envelope recipient', async () => {
    restore = mockFetch(res(200), []);
    const event = makeEvent({ to: '' });

    await handleEmail(event, ENV);

    assert.equal(event._rejects.length, 1);
    assert.match(event._rejects[0], /recipient/i);
  });

  test('throws (transient) when the upstream is unreachable', async () => {
    restore = mockFetch(Promise.reject(new Error('ECONNREFUSED')), []);
    const event = makeEvent({});

    await assert.rejects(() => handleEmail(event, ENV), /failed to reach/i);
    // A network failure must not be recorded as a permanent reject.
    assert.equal(event._rejects.length, 0);
  });

  test('throws (transient) on a 5xx upstream response', async () => {
    restore = mockFetch(res(503, 'service unavailable'), []);
    const event = makeEvent({});

    await assert.rejects(() => handleEmail(event, ENV), /returned 503/);
    assert.equal(event._rejects.length, 0);
  });

  test('rejects permanently on a 4xx upstream response', async () => {
    restore = mockFetch(res(401, '{"error":"Unauthorized"}'), []);
    const event = makeEvent({});

    await handleEmail(event, ENV); // 4xx will not succeed on retry

    assert.equal(event._rejects.length, 1);
    assert.match(event._rejects[0], /rejected message \(401\)/);
  });

  test('fails fast when INBOUND_SECRET is not configured', async () => {
    const event = makeEvent({});
    await assert.rejects(
      () => handleEmail(event, { INBOUND_URL: URL }),
      /INBOUND_SECRET binding is not set/,
    );
  });

  test('fails fast when INBOUND_URL is not configured', async () => {
    const event = makeEvent({});
    await assert.rejects(
      () => handleEmail(event, { INBOUND_SECRET: SECRET }),
      /INBOUND_URL binding is not set/,
    );
  });
});
