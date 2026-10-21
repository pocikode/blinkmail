# Blinkmail API

JSON HTTP API for Blinkmail. All routes are served by the Go application on the
same origin as the web UI.

Base URL: `https://<your-host>` (for example `http://127.0.0.1:8080` locally).

## Authentication

There are two distinct auth mechanisms:

1. **Browser sessions** use the `x-session-id` header. Obtain a token from
   `GET /api/session`; the server generates it with `crypto/rand` and stores it.
   Tokens are 64 lowercase hex characters. A token is only resumed if the server
   recognises it — arbitrary client-supplied strings are never registered as new
   sessions.

   Sessions organise which inboxes a browser sees. They are **not** a
   confidentiality boundary: inboxes are public by address.

2. **Inbound mail** (`POST /api/inbound`) uses a shared secret:
   `Authorization: Bearer <INBOUND_SECRET>`. Only the Cloudflare Email
   Worker holds this secret. It is never exposed by `/api/config` or any other
   public route, and the application refuses to start without it.

## Errors

Every error response uses this shape with an appropriate status code:

```json
{ "error": "Human-readable error message" }
```

| Status | Meaning |
| --- | --- |
| 400 | Validation failure: missing session header, bad local part, unsupported domain, malformed JSON, bad envelope |
| 401 | Missing or unknown session token; or inbound auth failure |
| 403 | Inbox not linked to the calling session |
| 409 | Random address allocation exhausted retries |
| 413 | Raw message exceeds the configured size limit |
| 422 | MIME could not be parsed |
| 500 | Internal error |
| 503 | Health check failed |

## Endpoints

### GET `/api/config`

Public application settings. Requires no session.

```json
{
  "appName": "Blinkmail",
  "mailDomain": "mail.example.com",
  "mailDomains": ["mail.example.com", "mail.example.org"]
}
```

`mailDomain` is the default (first configured) domain.

### GET `/api/session`

Creates a new anonymous session, or resumes an existing valid one.

Send no header for a new session:

```http
GET /api/session
```

Send a known token to resume it:

```http
GET /api/session
x-session-id: 4f0e...64 hex chars
```

Response `200`:

```json
{ "sessionId": "4f0e1a2b..." }
```

If the supplied token is malformed, the response is `401`. If it is well-formed
but unknown, a fresh token is issued instead of registering the client's string.

### GET `/api/inboxes`

Lists inboxes linked to the session.

```http
GET /api/inboxes
x-session-id: <token>
```

Response `200` (array, newest first):

```json
[
  { "address": "kopisenja47@mail.example.com", "created_at": "2026-10-21T04:59:12.109Z" }
]
```

Errors: `400` missing header, `401` unknown token.

### POST `/api/inboxes`

Creates or claims an address and links it to the session.

```http
POST /api/inboxes
x-session-id: <token>
Content-Type: application/json
```

Both fields are optional:

```json
{ "localPart": "kopisenja", "domain": "mail.example.com" }
```

- Omit `localPart` (or send `""`) to get a random address.
- Omit `domain` to use the default configured domain.
- A `localPart` that already exists is **claimed**: the existing inbox and its
  messages become visible to this session. This is intentional for a public
  service.

Validation:
- `localPart`: letters, digits, `.` `_` `-` `+`; ≤64 chars; no leading/trailing
  or consecutive dots; no whitespace or `@`.
- `domain`: must be syntactically valid **and** in `mailDomains`.

Response `201`:

```json
{ "address": "kopisenja@mail.example.com", "created_at": "2026-10-21T04:59:12.109Z" }
```

Errors: `400` validation, `409` random allocation exhausted, `500` storage.

Random generation retries up to 50 times on collision; the database PRIMARY KEY
is the arbiter, so concurrent requests cannot both claim the same new address.

### DELETE `/api/inboxes/{address}`

Unlinks the address from **this session only**. The inbox and all its messages
are preserved, and the address remains public.

```http
DELETE /api/inboxes/kopisenja%40mail.example.com
x-session-id: <token>
```

Response `200`:

```json
{ "ok": true }
```

Idempotent: unlinking an address the session never held still returns `200`.

### GET `/api/inboxes/{address}/messages`

Lists messages for an address after confirming it is linked to the session.

```http
GET /api/inboxes/kopisenja%40mail.example.com/messages
x-session-id: <token>
```

Response `200` (array, newest first):

```json
[
  {
    "id": "msg_fbaa9f286be5a05c0c1159964714f0be",
    "inbox_address": "kopisenja@mail.example.com",
    "from_address": "sender@gmail.com",
    "subject": "Hello Blinkmail",
    "body": "This is the plain text body.",
    "received_at": "2026-10-21T04:59:23.317Z"
  }
]
```

Errors: `400` invalid address, `403` inbox not in this session.

Bodies are plain text. When a message had no `text/plain` part, the `text/html`
part is stored as-is; the frontend renders it through text nodes, so it displays
as escaped text and cannot execute.

### POST `/api/inbound`

Receives raw MIME from the Email Worker. **Bearer-authenticated.**

```http
POST /api/inbound
Authorization: Bearer <INBOUND_SECRET>
Content-Type: message/rfc822
X-Envelope-From: sender@gmail.com
X-Envelope-To: kopisenja@mail.example.com

<raw MIME bytes>
```

Behaviour:
- Routing uses `X-Envelope-To`, never the MIME `To` header.
- `X-Envelope-From` may be the SMTP null reverse-path `<>` (bounces); the MIME
  `From` header is then used for display, falling back to `unknown`.
- The recipient's domain must be configured.
- Raw size is capped at `MAX_INBOUND_BYTES` (default 25 MiB).
- The inbox is created if missing, and both writes commit in one transaction
  **before** a success is returned.
- Duplicates are detected by a SHA-256 of envelope-from + envelope-to + raw
  bytes, enforced by a UNIQUE constraint. A repeat returns success with
  `duplicate: true`.

Response `200` (new):

```json
{ "ok": true, "messageId": "msg_fbaa...", "duplicate": false }
```

Response `200` (already committed):

```json
{ "ok": true, "messageId": "", "duplicate": true }
```

Errors: `401` bad secret, `400` bad envelope/domain/empty body, `413` too large,
`422` unparsable MIME, `500` storage failure.

Only `200` is treated as delivered. The Worker rejects or fails on anything else
so a lost message is never acknowledged.

### GET `/healthz`

Deployment health check.

```json
{ "status": "ok", "database": "ok", "messages": 3 }
```

Returns `503` with `"status":"unhealthy"` when the database is unreachable.

## Full flow example

```bash
BASE=http://127.0.0.1:8080

# 1. Get a session token.
SID=$(curl -s $BASE/api/session | jq -r .sessionId)

# 2. Create a random inbox.
ADDR=$(curl -s -X POST $BASE/api/inboxes \
  -H "x-session-id: $SID" \
  -H "Content-Type: application/json" \
  -d '{"domain":"mail.example.com"}' | jq -r .address)
echo "$ADDR"

# 3. Simulate an inbound delivery (normally the Email Worker does this).
curl -s -X POST $BASE/api/inbound \
  -H "Authorization: Bearer $INBOUND_SECRET" \
  -H "Content-Type: message/rfc822" \
  -H "X-Envelope-From: sender@gmail.com" \
  -H "X-Envelope-To: $ADDR" \
  --data-binary $'Subject: Test\n\nHello from Blinkmail'

# 4. Read messages.
curl -s "$BASE/api/inboxes/$(printf %s "$ADDR" | jq -sRr @uri)/messages" \
  -H "x-session-id: $SID" | jq

# 5. Remove the address from this browser (messages are kept).
curl -s -X DELETE "$BASE/api/inboxes/$(printf %s "$ADDR" | jq -sRr @uri)" \
  -H "x-session-id: $SID" | jq
```

## Session behaviour summary

| Situation | Result |
| --- | --- |
| New browser, no token | `GET /api/session` issues a fresh token |
| Reload with a stored token | Same token resumed |
| Token from another deployment | Malformed or unknown → new token issued |
| Two browsers | Separate sessions, but both can claim the same public address |
| Address removed then re-claimed | Same inbox and full history returned |
| Mail arrives before any browser claims it | Inbox auto-created; history readable after claiming |
