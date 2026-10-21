/*
 * Blinkmail Email Worker relay.
 *
 * Cloudflare Email Routing invokes email() for every message routed to this
 * Worker. The Worker does NOT parse mail; it forwards the raw MIME bytes to the
 * Blinkmail application's authenticated POST /api/inbound endpoint, which does
 * the parsing and storage.
 *
 * Design notes on failure handling:
 *   - This Worker distinguishes a permanent rejection (message.setReject) from
 *     a transient failure (throw, which Cloudflare may retry) and never ACKs a
 *     message the app did not commit. See README "Delivery semantics".
 *   - The envelope recipient is forwarded verbatim; the app, not the MIME To
 *     header, decides routing.
 *   - Oversized messages are rejected before forwarding using rawSize, so the
 *     app is never asked to buffer more than its limit.
 *
 * This is a plain ES module so Node's built-in test runner can import it and
 * exercise the handler with a mocked event and a stubbed global fetch.
 */

/** Raw MIME size ceiling, matching the application default (25 MiB). */
export const MAX_RAW_BYTES = 25 * 1024 * 1024;

/**
 * Validate the Worker configuration. A missing secret would forward mail to an
 * endpoint that cannot authenticate it, so refuse rather than send.
 *
 * @param {{INBOUND_URL?: string, API_KEY?: string}} env
 */
function validateEnv(env) {
  if (!env) {
    throw new Error('Worker environment bindings are missing');
  }
  if (!env.INBOUND_URL) {
    throw new Error('INBOUND_URL binding is not set');
  }
  if (!env.API_KEY) {
    throw new Error('API_KEY binding is not set');
  }
}

/**
 * Reject a message permanently and return normally.
 *
 * Cloudflare treats message.setReject() as a permanent SMTP failure: the sender
 * is bounced and the message is not retried. Use this only for conditions that
 * will never succeed on retry (oversized, no recipient). Returning afterwards
 * lets Cloudflare record the rejection instead of also erroring the invocation.
 *
 * @param {{setReject?: (reason: string) => void}} message
 * @param {string} reason
 */
function rejectPermanent(message, reason) {
  if (message && typeof message.setReject === 'function') {
    message.setReject(reason);
    return;
  }
  // Without a setReject binding there is no way to bounce; throw so the
  // invocation is at least marked failed rather than silently ACKed.
  throw new Error(reason);
}

/**
 * Fail a message transiently by throwing.
 *
 * A thrown error makes Cloudflare treat delivery as failed and retry it. Use
 * this for conditions that may succeed later: the VPS is unreachable, returned
 * 5xx, or rejected our credentials (an operator misconfiguration). We do not
 * claim guaranteed retries or durable queueing; Cloudflare's own retry policy
 * applies.
 *
 * @param {string} reason
 */
function failTransient(reason) {
  throw new Error(reason);
}

/**
 * Handle one inbound email.
 *
 * @param {{
 *   from: string,
 *   to: string,
 *   raw: ReadableStream<Uint8Array>,
 *   rawSize?: number,
 *   setReject?: (reason: string) => void,
 * }} message
 * @param {{INBOUND_URL: string, API_KEY: string, MAX_RAW_BYTES?: string|number}} env
 * @param {{waitUntil?: (p: Promise<unknown>) => void}} [_ctx]
 * @returns {Promise<void>}
 */
export async function handleEmail(message, env, _ctx) {
  validateEnv(env);

  const limit = env.MAX_RAW_BYTES ? Number(env.MAX_RAW_BYTES) : MAX_RAW_BYTES;

  // Reject oversized mail before forwarding, using the size Cloudflare reports.
  // Buffering 100 MiB in a Worker only to be refused downstream wastes CPU and
  // memory and could hit Worker limits. Oversize is permanent: reject, don't
  // retry.
  if (typeof message.rawSize === 'number' && message.rawSize > limit) {
    rejectPermanent(message, `Message too large: ${message.rawSize} bytes exceeds ${limit}`);
    return;
  }

  const envelopeTo = (message.to || '').toLowerCase();
  const envelopeFrom = (message.from || '').toLowerCase();

  if (!envelopeTo) {
    // No recipient means nothing to route on; permanent, the app would 400 too.
    rejectPermanent(message, 'Missing envelope recipient');
    return;
  }

  // Read the raw stream fully so it can be sent as one body. The application
  // re-checks the byte limit authoritatively; this is a first-line filter.
  const rawBytes = await readAll(message.raw, limit);
  if (rawBytes.byteLength > limit) {
    rejectPermanent(message, `Message too large: ${rawBytes.byteLength} bytes exceeds ${limit}`);
    return;
  }

  const url = env.INBOUND_URL;

  let response;
  try {
    response = await fetch(url, {
      method: 'POST',
      headers: {
        // Managed API key, created in the Blinkmail UI and stored as a Wrangler
        // secret — never in wrangler.toml.
        Authorization: `Bearer ${env.API_KEY}`,
        'Content-Type': 'message/rfc822',
        // Envelope metadata drives routing and dedupe on the app side.
        'X-Envelope-From': envelopeFrom,
        'X-Envelope-To': envelopeTo,
        'Content-Length': String(rawBytes.byteLength),
      },
      body: rawBytes,
    });
  } catch (networkErr) {
    // Network failure reaching the VPS may be transient (VPS down, TLS blip),
    // so throw to let Cloudflare retry rather than ACKing undelivered mail.
    failTransient(`Relay failed to reach ${url}: ${networkErr && networkErr.message}`);
    return; // unreachable; failTransient throws
  }

  // The app returns 200 for both a fresh store and an already-committed
  // duplicate. Classify everything else so retries happen only where useful.
  if (response.status === 200) {
    return; // committed: safe to ACK
  }

  const detail = await safeText(response);

  // 4xx (bad auth, invalid envelope, oversized at the app) will not succeed on
  // retry, so reject permanently instead of looping.
  if (response.status >= 400 && response.status < 500) {
    rejectPermanent(message, `Inbound endpoint rejected message (${response.status}): ${detail}`);
    return;
  }

  // 5xx or an unexpected status: transient, let Cloudflare retry.
  failTransient(`Inbound endpoint returned ${response.status}: ${detail}`);
}

/**
 * Read a stream to completion, stopping early if it exceeds limit bytes.
 *
 * @param {ReadableStream<Uint8Array>|undefined} stream
 * @param {number} limit
 * @returns {Promise<Uint8Array>}
 */
async function readAll(stream, limit) {
  if (!stream) {
    throw new Error('message.raw stream is missing');
  }

  // Prefer the platform's convenience reader when present (some test doubles
  // and polyfills expose arrayBuffer directly).
  if (typeof stream.arrayBuffer === 'function') {
    const buf = await stream.arrayBuffer();
    const bytes = new Uint8Array(buf);
    if (bytes.byteLength > limit) {
      // Signal oversize by returning a slice one past the cap; the caller
      // compares length against limit.
      return bytes.subarray(0, limit + 1);
    }
    return bytes;
  }

  const reader = stream.getReader();
  const chunks = [];
  let total = 0;
  let overflow = false;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (!value) continue;
    total += value.byteLength;
    chunks.push(value);
    if (total > limit) {
      // Stop as soon as the cap is passed; no need to buffer the whole thing.
      overflow = true;
      try { await reader.cancel(); } catch (_) { /* ignore */ }
      break;
    }
  }

  if (overflow) {
    // Return a length that exceeds limit so the caller rejects; avoid
    // concatenating a possibly huge body.
    return new Uint8Array(limit + 1);
  }

  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}

/**
 * Read a response body as text, never throwing.
 * @param {{text: () => Promise<string>}} response
 * @returns {Promise<string>}
 */
async function safeText(response) {
  try {
    const text = await response.text();
    return (text || '').slice(0, 200);
  } catch (_) {
    return '<no body>';
  }
}

/**
 * Default Worker export. Cloudflare calls email() for routed messages.
 * A fetch() handler is included so deploying the Worker to a route does not
 * error on HTTP probes; it only reports status.
 */
export default {
  async email(message, env, ctx) {
    await handleEmail(message, env, ctx);
  },

  async fetch() {
    return new Response(
      'Blinkmail Email Worker relay. This Worker only handles inbound email.',
      { status: 200, headers: { 'Content-Type': 'text/plain; charset=utf-8' } },
    );
  },
};
