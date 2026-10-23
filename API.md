# Blinkmail API

JSON HTTP API for Blinkmail. All routes are served by the Go application on the
same origin as the web UI.

Base URL: `https://<your-host>` (for example `http://127.0.0.1:8080` locally).

## Authentication

There are three distinct auth mechanisms. Every `/api/` route requires the
login cookie **or** a managed API key except `/api/login`, `/api/auth/status` and
`/api/config`; `/api/v1/` is the exception in the other direction, being
API-key-only.

1. **Web UI login** uses a password-only admin session cookie. See
   [Web UI login](#web-ui-login) below.

2. **Browser sessions** use the `x-session-id` header. Obtain a token from
   `GET /api/session`; the server generates it with `crypto/rand` and stores it.
   Tokens are 64 lowercase hex characters. A token is only resumed if the server
   recognises it — arbitrary client-supplied strings are never registered as new
   sessions.

   Sessions organise which inboxes a browser sees. They are **not** a
   confidentiality boundary: inboxes are public by address. The login cookie and
   the `x-session-id` are independent: logging in does not create an inbox
   session, and rotating the inbox session does not log you out.

3. **Managed API keys** authenticate with `Authorization: Bearer bm_...`.
   Create and revoke them in the web UI (**Settings → API keys**), or with the
   `/api/keys` routes below. The Cloudflare Email Worker uses one to call
   `POST /api/inbound`; API clients use one for the JSON API. Each key is
   scoped: a relay key only needs `inbound`, an integration only `read`.

   A key is `bm_` followed by 32 random bytes as hex (67 characters). Only its
   SHA-256 hash and a 12-character display prefix are stored; the plaintext is
   returned **once**, from the create call, and never again. There is no KDF
   because a 256-bit random key has nothing to stretch.

   An API-key caller does **not** send `x-session-id`: each key has a stable
   synthetic inbox session (`key_<key id>`), created lazily on first use. That
   keeps inbox ownership per key instead of global. Cookie-authenticated
   browsers keep the explicit `x-session-id` behaviour.

   A key cannot manage keys: the `/api/keys` routes reject key-authenticated
   callers with `403`, so a leaked key cannot mint a replacement.

#### Permissions

Each key carries a set of permissions. There are exactly two:

| Permission | Unlocks |
| --- | --- |
| `inbound` | `POST /api/inbound` — deliver mail |
| `read` | the read surface: `/api/v1/...` and the `/api/inboxes*` / `/api/session` routes |

A key with both is "everything a key can do". A key with neither cannot be
created: an empty list is a `400`, and omitting `permissions` when creating a key
defaults to `inbound,read` (which is also what keys created before scoping
carry). The order is canonical — `inbound` before `read`, duplicates removed — so
`["read","read","inbound"]` is stored as `inbound,read`.

A request that needs a permission the key lacks is `403`:

```json
{ "error": "API key lacks the \"read\" permission" }
```

Give the relay Worker only `inbound` and an integration only `read`: a leaked
read-only key then cannot inject mail. Cookie callers (the web UI) hold every
permission — scoping exists to limit keys, not the UI.

`/api/v1/` is **key-only**. A cookie caller there gets
`403 {"error":"API key required"}`: it is the external integration contract and
the UI must never depend on it.

### Web UI login

There is a **single admin account and no username** — the password is the whole
credential. It is seeded once, on first start, from `INITIAL_PASSWORD`:

| Situation | Behaviour |
| --- | --- |
| Fresh database, `INITIAL_PASSWORD` set | Hashed and stored; logged as `admin password initialised from INITIAL_PASSWORD` |
| Fresh database, `INITIAL_PASSWORD` empty | Startup fails: `INITIAL_PASSWORD must be set on first start: it seeds the admin password` |
| Password already stored | `INITIAL_PASSWORD` is ignored (debug log); change it from the UI |

The password is stored as a PBKDF2-HMAC-SHA256 hash — 32-byte random salt,
600 000 iterations, 32-byte key — encoded as
`pbkdf2-sha256$<iter>$<salt-hex>$<key-hex>`. Verification is constant time.
The plaintext password is never logged and never returned by any route.

**Cookie.** A successful login sets `blinkmail_auth`:

| Attribute | Value |
| --- | --- |
| `Path` | `/` |
| `HttpOnly` | yes — unreadable from JavaScript |
| `SameSite` | `Lax` |
| `Secure` | set when the request is TLS |
| `Max-Age` | `2592000` (30 days) |

The token is 32 random bytes as hex. The session row lives in `auth_sessions`
with the same 30-day expiry, so a cookie can be revoked server-side at any time
by deleting the row — logout and password change both do exactly that.

> **Behind a TLS-terminating reverse proxy** the request the app sees is plain
> HTTP, so the cookie will not carry `Secure`. Either forward the original scheme
> and run the app on a TLS listener, or accept it for a loopback-bound
> deployment. See the README's HTTPS section.

**CSRF.** `SameSite=Lax` keeps the cookie off cross-site requests, and every
state-changing route additionally rejects a `Content-Type` that is not
`application/json` with `415`. A cross-site form cannot set that header without a
CORS preflight, so forged state changes are blocked on two independent grounds.

**Login rate limiting.** Failed logins are counted **per client IP**: 5 failures
in 15 minutes, then `429`. A successful login clears the counter. The counter is
in memory, per process, and **resets when the server restarts** — it exists to
blunt online guessing, not as a durable audit trail. Only the transport peer
address is used; `X-Forwarded-For` is ignored, so behind a reverse proxy all
clients share one bucket. Consequence: a password change from the UI revokes
every other session.

### Protected routes

Everything under `/api/` requires a valid `blinkmail_auth` cookie or an API key:

| Route | Public? |
| --- | --- |
| `GET /api/auth/status` | public |
| `POST /api/login` | public |
| `GET /api/config` | public |
| `GET /healthz` | public |
| `GET /api/v1/...` | **API key only**, needs `read` |
| `POST /api/inbound` | cookie **or** API key, needs `inbound` |
| `GET/POST/DELETE /api/keys...` | **cookie only** — an API key gets `403` |
| all other `/api/` routes | cookie **or** API key, needs `read` for a key |

A missing, malformed, unknown or expired credential is:

```json
{ "error": "Authentication required" }
```

with status `401`. Which part failed is never revealed. A state-changing request
whose `Content-Type` is not `application/json` is `415` before authentication is
even considered — except `POST /api/inbound`, which carries `message/rfc822`.

## Errors

Every error response uses this shape with an appropriate status code:

```json
{ "error": "Human-readable error message" }
```

| Status | Meaning |
| --- | --- |
| 400 | Validation failure: missing session header, bad local part, unsupported domain, malformed JSON, bad envelope, new password too short |
| 401 | Missing/invalid cookie or API key; unknown session token; wrong password |
| 403 | Inbox not linked to the calling session; a key missing the required permission; a cookie caller on `/api/v1/` (`API key required`); API key used on `/api/keys` |
| 409 | Random address allocation exhausted retries |
| 413 | Raw message exceeds the configured size limit |
| 415 | State-changing request without an `application/json` Content-Type |
| 422 | MIME could not be parsed |
| 429 | Too many failed login attempts from this IP |
| 500 | Internal error |
| 503 | Health check failed |

## Endpoints

### GET `/api/auth/status`

Public. Reports whether this browser holds a valid login cookie. The web client
calls it on boot to decide between the login form and the app.

```http
GET /api/auth/status
```

```json
{ "authenticated": false }
```

`true` only when the cookie names an `auth_sessions` row whose expiry is still in
the future.

### POST `/api/login`

Public. Password only — there is no username.

```http
POST /api/login
Content-Type: application/json

{ "password": "correct-horse-battery-staple" }
```

Success `200`, plus a `Set-Cookie: blinkmail_auth=…` header:

```json
{ "ok": true }
```

Wrong password `401`:

```json
{ "error": "Invalid password" }
```

Errors: `400` malformed JSON, `401` wrong password, `415` non-JSON content type,
`429` rate limited (5 failures per IP per 15 minutes), `500` the stored hash
could not be read or is corrupt.

### POST `/api/logout`

Requires the cookie. Deletes the session row and expires the cookie.

```http
POST /api/logout
Content-Type: application/json
```

```json
{ "ok": true }
```

Idempotent in effect: a second logout without a valid cookie is `401`, and
logging out again from the UI still lands on the login screen.

### POST `/api/password`

Requires the cookie. Changes the admin password and **revokes every other login
session**, keeping the caller's own session so the UI stays signed in.

```http
POST /api/password
Content-Type: application/json

{ "currentPassword": "old-password", "newPassword": "new-password" }
```

```json
{ "ok": true }
```

The current password is required even though the caller is already
authenticated: it is the difference between a stolen cookie reading mail and a
stolen cookie taking over the account.

Errors: `400` new password shorter than 8 characters, `401` wrong current
password (or the cookie is gone), `415` non-JSON content type, `500` storage.

### GET `/api/config`

Public application settings. Requires no login or session — the client needs
these values to render the login screen.

```json
{
  "appName": "Blinkmail",
  "mailDomain": "mail.example.com",
  "mailDomains": ["mail.example.com", "mail.example.org"]
}
```

`mailDomain` is the default (first configured) domain.

### GET `/api/session`

Creates a new anonymous session, or resumes an existing valid one. Requires the
login cookie; the returned token is the per-browser inbox session, separate from
the login session.

Send no header for a new session:

```http
GET /api/session
Cookie: blinkmail_auth=<token>
```

Send a known token to resume it:

```http
GET /api/session
Cookie: blinkmail_auth=<token>
x-session-id: 4f0e...64 hex chars
```

Response `200`:

```json
{ "sessionId": "4f0e1a2b..." }
```

If the supplied token is malformed, the response is `401`. If it is well-formed
but unknown, a fresh token is issued instead of registering the client's string.

### GET `/api/inboxes`

Lists inboxes linked to the session. Requires the login cookie.

```http
GET /api/inboxes
Cookie: blinkmail_auth=<token>
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
Cookie: blinkmail_auth=<token>
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
Cookie: blinkmail_auth=<token>
x-session-id: <token>
Content-Type: application/json
```

The `Content-Type` is required even without a body: it is the CSRF signal. A
`DELETE` without it is `415`.

Response `200`:

```json
{ "ok": true }
```

Idempotent: unlinking an address the session never held still returns `200`.

### GET `/api/inboxes/{address}/messages`

Lists messages for an address after confirming it is linked to the session.

```http
GET /api/inboxes/kopisenja%40mail.example.com/messages
Cookie: blinkmail_auth=<token>
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
    "body_html": "<p>This is the <b>HTML</b> body.</p>",
    "received_at": "2026-10-21T04:59:23.317Z"
  }
]
```

Errors: `400` invalid address, `403` inbox not in this session.

`body` is the plain-text body: the `text/plain` part when the message had one,
otherwise the `text/html` source, so a client that only reads `body` behaves as
it always has. `body_html` carries the `text/html` part and is **omitted** when
the message had none — it is present for both HTML-only messages and
multipart/alternative ones.

The web client renders `body_html` as HTML, inside a sandboxed `<iframe>` that
cannot run script; `body` is what it shows for plain-text mail.

### POST `/api/inbound`

Receives raw MIME from the Email Worker. **Bearer-authenticated with a managed
API key** — the same gate as every other `/api/` route.

```http
POST /api/inbound
Authorization: Bearer bm_<key>
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

Errors: `401` missing/unknown/revoked key, `403` key without the `inbound`
permission, `400` bad envelope/domain/empty body, `413` too large, `422`
unparsable MIME, `500` storage failure.

Only `200` is treated as delivered. The Worker rejects or fails on anything else
so a lost message is never acknowledged.

### GET `/api/keys`

Lists API key metadata. **Cookie only**: an API-key caller gets `403`.

```http
GET /api/keys
Cookie: blinkmail_auth=<token>
```

Response `200` (array, newest first):

```json
[
  {
    "id": "6f1c...",
    "name": "Email Worker",
    "prefix": "bm_9a1b2c3d4",
    "created_at": "2026-10-21T06:00:00.000Z",
    "last_used_at": "2026-10-21T06:05:00.000Z",
    "permissions": ["inbound", "read"]
  }
]
```

`last_used_at` is `null` until the key is first used. `permissions` is a JSON
array (the stored column is a comma-separated string). The plaintext and the
hash are never part of this response.

Errors: `401` not authenticated, `403` authenticated with an API key, `500`
storage failure.

### POST `/api/keys`

Creates a key and returns the plaintext **exactly once**. **Cookie only.**

```http
POST /api/keys
Content-Type: application/json
Cookie: blinkmail_auth=<token>

{ "name": "Email Worker", "permissions": ["inbound"] }
```

`name` is trimmed, required and at most 64 characters.

`permissions` is optional and defaults to `["inbound", "read"]` when omitted, so
older clients and scripts keep working. When present it must be a non-empty
array of `inbound` and/or `read`; an explicit `[]` is a `400`, not a default, and
an unknown name such as `"write"` is a `400`. Duplicates are removed and the
stored order is always `inbound` before `read`.

Response `201`:

```json
{
  "id": "6f1c...",
  "name": "Email Worker",
  "key": "bm_<64 hex characters>",
  "permissions": ["inbound"]
}
```

Copy `key` immediately: it is not stored and cannot be retrieved again. Only a
SHA-256 of it and the 12-character display prefix are kept.

Errors: `400` missing/blank/too-long name, empty or unknown permissions, `401`,
`403` API-key caller, `415` non-JSON content type, `500` storage.

### DELETE `/api/keys/{id}`

Revokes a key. The key stops authenticating immediately. **Cookie only.**

```http
DELETE /api/keys/6f1c...
Cookie: blinkmail_auth=<token>
```

```json
{ "ok": true }
```

Errors: `401`, `403` API-key caller, `404` unknown id, `500` storage.

### GET `/healthz`

Deployment health check.

```json
{ "status": "ok", "database": "ok", "messages": 3 }
```

Returns `503` with `"status":"unhealthy"` when the database is unreachable.

## External read API `/api/v1/`

The integration contract: **API key only**, and the key needs the `read`
permission. A cookie caller is `403 {"error":"API key required"}`. Unlike the
`/api/inboxes*` routes, no `x-session-id` is involved and no address has to be
claimed first — that is the point: an application can poll an address it never
created.

### GET `/api/v1/inboxes`

Every inbox that has received at least one message, newest activity first.
Inboxes with no messages are omitted.

```bash
curl -s https://<your-host>/api/v1/inboxes \
  -H "Authorization: Bearer bm_<key>"
```

```json
[
  {
    "address": "test@relix.web.id",
    "created_at": "2026-10-21T18:15:48.768Z",
    "message_count": 3,
    "last_received_at": "2026-10-21T18:15:48.768Z"
  }
]
```

`last_received_at` is always set here, since an inbox without mail is not listed.
The response is `[]`, never `null`, when no mail exists.

### GET `/api/v1/inboxes/{address}/messages`

Messages for one address, **oldest first** (`received_at ASC`). Paging with the
last message's `received_at` as `since` therefore never skips a message.

```bash
curl -s "https://<your-host>/api/v1/inboxes/test%40relix.web.id/messages?since=2026-10-21T18:15:48.768Z&limit=100" \
  -H "Authorization: Bearer bm_<key>"
```

Query parameters:

| Parameter | Rule |
| --- | --- |
| `since` | optional RFC 3339 timestamp; only messages received **after** it. Invalid → `400 Invalid since timestamp, want RFC 3339` |
| `limit` | optional; default `100`, maximum `500` (larger values are clamped). `< 1` or non-numeric → `400` |

Response `200` (array, oldest first; the usual `store.Message` shape):

```json
[
  {
    "id": "msg_fbaa9f286be5a05c0c1159964714f0be",
    "inbox_address": "test@relix.web.id",
    "from_address": "sender@gmail.com",
    "subject": "Hello Blinkmail",
    "body": "This is the plain text body.",
    "body_html": "<p>This is the <b>HTML</b> body.</p>",
    "received_at": "2026-10-21T18:15:48.768Z"
  }
]
```

An address that has **never received mail** is `200 []`, **not** `404`: inboxes
are created lazily on first delivery, so "not found" would be the normal state
of a fresh address. Errors: `400` invalid address / `since` / `limit`, `401`
missing or bad key, `403` key without `read` or a cookie caller, `500` storage.

Poll loop, in one request each time:

```bash
# First call: everything.
curl -s "https://<your-host>/api/v1/inboxes/test%40relix.web.id/messages" \
  -H "Authorization: Bearer $READ_KEY" | jq

# Later calls: pass the last received_at you saw.
curl -s "https://<your-host>/api/v1/inboxes/test%40relix.web.id/messages?since=$LAST" \
  -H "Authorization: Bearer $READ_KEY" | jq
```

## Full flow example

```bash
BASE=http://127.0.0.1:8080
JAR=$(mktemp)   # curl stores the HttpOnly blinkmail_auth cookie here

# 1. Log in. The cookie is written to the jar and sent automatically below.
curl -s -c "$JAR" -X POST $BASE/api/login \
  -H "Content-Type: application/json" \
  -d '{"password":"'"$INITIAL_PASSWORD"'"}' | jq

# 2. Get an inbox session token.
SID=$(curl -s -b "$JAR" $BASE/api/session | jq -r .sessionId)

# 3. Create a random inbox.
ADDR=$(curl -s -b "$JAR" -X POST $BASE/api/inboxes \
  -H "x-session-id: $SID" \
  -H "Content-Type: application/json" \
  -d '{"domain":"mail.example.com"}' | jq -r .address)
echo "$ADDR"

# 4. Create an API key for the relay and use it for a simulated delivery.
API_KEY=$(curl -s -b "$JAR" -X POST $BASE/api/keys \
  -H "Content-Type: application/json" \
  -d '{"name":"Email Worker","permissions":["inbound"]}' | jq -r .key)

curl -s -X POST $BASE/api/inbound \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: message/rfc822" \
  -H "X-Envelope-From: sender@gmail.com" \
  -H "X-Envelope-To: $ADDR" \
  --data-binary $'Subject: Test\n\nHello from Blinkmail'

# 5. Read messages. The browser route needs the session link...
curl -s -b "$JAR" \
  "$BASE/api/inboxes/$(printf %s "$ADDR" | jq -sRr @uri)/messages" \
  -H "x-session-id: $SID" | jq

# ...or read them with a read-only key, without any session at all.
READ_KEY=$(curl -s -b "$JAR" -X POST $BASE/api/keys \
  -H "Content-Type: application/json" \
  -d '{"name":"Integration","permissions":["read"]}' | jq -r .key)
curl -s "$BASE/api/v1/inboxes/$(printf %s "$ADDR" | jq -sRr @uri)/messages" \
  -H "Authorization: Bearer $READ_KEY" | jq

# 6. Remove the address from this browser (messages are kept).
curl -s -b "$JAR" -X DELETE "$BASE/api/inboxes/$(printf %s "$ADDR" | jq -sRr @uri)" \
  -H "x-session-id: $SID" \
  -H "Content-Type: application/json" | jq

# 7. Sign out: revokes the session row and expires the cookie.
curl -s -b "$JAR" -X POST $BASE/api/logout \
  -H "Content-Type: application/json" | jq
```

## Login behaviour summary

| Situation | Result |
| --- | --- |
| Fresh database, no `INITIAL_PASSWORD` | Startup fails with an actionable error |
| Fresh database, `INITIAL_PASSWORD` set | Password hashed and stored; env ignored from then on |
| Password already stored, env changed | Env ignored (debug log); UI password unchanged |
| No cookie | Protected routes answer `401 Authentication required` |
| Wrong password | `401 Invalid password`; counted per IP |
| 5 failures in 15 minutes | `429` until the window passes; resets on restart |
| Correct password | `200` plus the `blinkmail_auth` cookie (30 days) |
| Logout | Row deleted, cookie expired; the old cookie is `401` |
| Password change | Other sessions revoked; the caller stays signed in |
| Expired cookie | `401`; the UI returns to the login form |

## API key behaviour summary

| Situation | Result |
| --- | --- |
| Create a key | `201` with the plaintext; shown once and never again |
| List keys | Metadata only — id, name, prefix, created, last used, permissions |
| Unknown/malformed/revoked key | `401 Authentication required` |
| Valid key on `/api/inboxes` | Works with no `x-session-id`; inboxes are per key; needs `read` |
| Valid key on `/api/inbound` | Accepted, same gate as any other route; needs `inbound` |
| Key without the needed permission | `403 API key lacks the "<permission>" permission` |
| Cookie on `/api/v1/...` | `403 API key required`; `/api/v1` is key-only |
| Key on `/api/keys` | `403`; a key cannot mint or revoke keys |
| Successful key request | `last_used_at` updated best effort, never fatal |

## Session behaviour summary

| Situation | Result |
| --- | --- |
| Logged-in browser, no token | `GET /api/session` issues a fresh token |
| API key, no token | Stable synthetic session `key_<id>`, created lazily |
| Reload with a stored token | Same token resumed |
| Token from another deployment | Malformed or unknown → new token issued |
| Two browsers | Separate sessions, but both can claim the same public address |
| Address removed then re-claimed | Same inbox and full history returned |
| Mail arrives before any browser claims it | Inbox auto-created; history readable after claiming |
