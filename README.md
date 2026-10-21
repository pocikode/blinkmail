# Blinkmail

Disposable email addresses, served from your own domain. Give out a throwaway
address, read the mail in a browser, move on.

Blinkmail is a Go HTTP server backed by SQLite, deployed in Docker on a VPS,
with Cloudflare Email Routing forwarding inbound mail through a small Email
Worker relay.

## Read this first: addresses are public

Every address on a Blinkmail instance is readable by anyone who knows it. A
browser session is a list of addresses, not a lock on them — any session can
claim an address and read its full history, and inbound mail creates an inbox
before anyone has claimed it.

So:

- **Never** use a Blinkmail address for passwords, banking, 2FA recovery, or
  anything private.
- Removing an address from the UI unlinks it from *your browser only*. The inbox
  and its messages stay on the server, and the address stays public.
- Messages are kept until an operator deletes them. There is no automatic
  expiry. See [Retention and cleanup](#retention-and-cleanup).

The web UI states this too, so users are not surprised.

## How it works

```
  Sender                Cloudflare                        Your VPS
    │                        │                                │
    │  SMTP to @yourdomain   │                                │
    ├───────────────────────>│                                │
    │                        │  Email Routing catch-all       │
    │                        │        → Email Worker          │
    │                        │                                │
    │                        │  POST /api/inbound             │
    │                        │  Authorization: Bearer <secret>│
    │                        │  X-Envelope-From / To          │
    │                        │  body = raw MIME               │
    │                        ├───────────────────────────────>│
    │                        │                                │  parse MIME
    │                        │                                │  store in SQLite
    │                        │            200 ok              │  (commit first)
    │                        │<───────────────────────────────┤
    │                        │                                │
    │                        │                                │
  Browser ──────────────────── HTTPS ─────────────────────────>│  web UI + JSON API
```

1. Cloudflare Email Routing accepts mail for your domain and hands it to the
   Email Worker.
2. The Worker forwards the **raw MIME** bytes to the application's
   `POST /api/inbound`, with a bearer secret and the SMTP envelope metadata in
   headers. It does no parsing.
3. The application validates the request, parses the MIME in Go, and stores the
   subject and text body in SQLite — creating the inbox if it does not exist.
   The inbox creation and the message insert commit together, and only then is
   success acknowledged.
4. The browser reads messages over the JSON API.

## Repository layout

```
cmd/blinkmail/          Main entrypoint, server wiring, graceful shutdown, -healthcheck
internal/config/        Environment configuration and startup validation
internal/store/         SQLite persistence: migrations, queries, dedupe, transactions
internal/mail/          MIME parsing via go-message (charset, multipart, encodings)
internal/address/       Address validation and crypto/rand random generation
internal/api/           HTTP handlers, routing, sessions, inbound, health
internal/web/           Embedded frontend assets and their handler
relay/                  Cloudflare Email Worker (worker.js, wrangler.toml, tests)
internal/store/migrations/  Embedded SQL migrations applied at startup
internal/web/assets/    Embedded frontend (index.html, app.js, styles.css)
API.md                  Full API reference
NOTICE                  Third-party attribution
```

## Quick start (local development)

Requires Go 1.27+.

```bash
# 1. Configure. Use placeholder domains for local testing.
cp .env.example .env
# Generate a real secret even locally — startup refuses a weak one:
openssl rand -hex 32

# 2. Run against a local database.
BLINKMAIL_MAIL_DOMAINS="mail.example.com" \
BLINKMAIL_WEB_HOST="localhost:8080" \
BLINKMAIL_DB_PATH="./data/blinkmail.db" \
BLINKMAIL_LISTEN_ADDR=":8080" \
BLINKMAIL_INBOUND_SECRET="$(openssl rand -hex 32)" \
  go run ./cmd/blinkmail
```

Open <http://localhost:8080>, click **New → Random**, then deliver a test
message:

```bash
curl -X POST localhost:8080/api/inbound \
  -H "Authorization: Bearer $BLINKMAIL_INBOUND_SECRET" \
  -H "Content-Type: message/rfc822" \
  -H "X-Envelope-From: sender@gmail.com" \
  -H "X-Envelope-To: <the address from the UI>@mail.example.com" \
  --data-binary $'Subject: Test\n\nHello from Blinkmail'
```

Click **Refresh** in the UI and the message appears.

## Docker deployment

### 1. Build and run with Compose

```bash
cp .env.example .env
$EDITOR .env            # set real domains, web host, and a strong secret
docker compose up -d --build
docker compose logs -f blinkmail
```

Compose publishes the port on **`127.0.0.1:8080`** only. Put a TLS-terminating
reverse proxy in front of it; do not expose 8080 directly.

### 2. Verify

```bash
docker compose ps                          # health status should be "healthy"
curl -s localhost:8080/healthz             # {"status":"ok","database":"ok",...}
docker compose exec blinkmail /usr/local/bin/blinkmail -healthcheck   # exit 0
```

The image is distroless (no shell, no curl), so the health check re-invokes the
binary with `-healthcheck`, which probes the local `/healthz` and exits non-zero
on failure.

### 3. Data persistence

The SQLite database lives in a named volume mounted at `/data`. The image seeds
that directory owned by uid 65532 (the distroless `nonroot` user), so the runtime
user can create the database and its WAL files without a shell `chown`.

```bash
docker compose down          # container removed, data kept
docker compose up -d         # data still there
docker compose down -v       # ⚠️ DESTROYS the volume and all mail
```

### Manual Docker (without Compose)

```bash
docker build -t blinkmail .
docker volume create blinkmail-data
docker run -d --name blinkmail \
  --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -v blinkmail-data:/data \
  -e BLINKMAIL_MAIL_DOMAINS="mail.example.com" \
  -e BLINKMAIL_WEB_HOST="mail.example.com" \
  -e BLINKMAIL_INBOUND_SECRET="$(openssl rand -hex 32)" \
  blinkmail
```

## Configuration

All configuration is environment variables. The application **refuses to start**
if required values are missing or invalid — in particular it will not start an
unauthenticated mail-ingestion endpoint.

| Variable | Default | Description |
| --- | --- | --- |
| `BLINKMAIL_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `BLINKMAIL_DB_PATH` | `/data/blinkmail.db` | SQLite file path |
| `BLINKMAIL_APP_NAME` | `Blinkmail` | Name shown in the UI |
| `BLINKMAIL_MAIL_DOMAINS` | *(required)* | Comma-separated domains; first is the default |
| `BLINKMAIL_WEB_HOST` | `localhost:8080` | Public host shown to users |
| `BLINKMAIL_INBOUND_SECRET` | *(required, ≥32 chars)* | Bearer secret for `/api/inbound` |
| `BLINKMAIL_MAX_INBOUND_BYTES` | `26214400` (25 MiB) | Raw message size cap |
| `BLINKMAIL_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

Startup validation rejects: a missing or malformed domain list, duplicate
domains, an empty database path, a missing inbound secret, and a secret shorter
than 32 characters.

## Cloudflare Email Routing (operator steps)

These steps require a Cloudflare account, a domain with Cloudflare DNS, and
`wrangler`. They are **not** automated: deploying the Worker does not create mail
routing rules, and Email Routing must be enabled explicitly.

### 1. Deploy the relay Worker

```bash
cd relay
npm install                                  # devDependencies: wrangler only
$EDITOR wrangler.toml                        # set INBOUND_URL and MAX_RAW_BYTES
npx wrangler secret put INBOUND_SECRET       # paste the SAME value as
                                             # BLINKMAIL_INBOUND_SECRET
npx wrangler deploy
```

Set `INBOUND_URL` to your public HTTPS endpoint, e.g.
`https://mail.example.com/api/inbound`. `MAX_RAW_BYTES` should equal
`BLINKMAIL_MAX_INBOUND_BYTES` so both sides agree on the cap.

Never put `INBOUND_SECRET` in `wrangler.toml` — it would be committed and
visible in the Cloudflare dashboard config. Use `wrangler secret put`.

### 2. Enable Email Routing for your domain

In the Cloudflare dashboard: **Email → Email Routing → Get started** for each
domain you configured. This creates the required MX and SPF records
automatically.

Verify from the CLI:

```bash
npx wrangler email routing settings example.com      # Enabled: true
```

### 3. Point the catch-all rule at the Worker

Still in **Email Routing → Routing rules**, create a **Catch-all** rule:

- Destination type: **Send to a Worker**
- Worker: `blinkmail-relay`

Or via CLI:

```bash
npx wrangler email routing rules list example.com
# confirm: Catch-all rule — enabled, action: worker:blinkmail-relay
```

Without this rule, mail reaches Cloudflare but never reaches your Worker.

### 4. Test

Send mail to any address at your domain, e.g. `anything@mail.example.com`, then
open the web UI, click **New**, type that exact username, and **Create**. The
inbox is public, so claiming it shows the message that already arrived.

Watch Worker logs during testing:

```bash
npx wrangler tail blinkmail-relay
docker compose logs -f blinkmail
```

## HTTPS reverse proxy

The container listens on plain HTTP at `:8080` and is published on loopback.
Terminate TLS in front of it. A minimal Caddyfile:

```
mail.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Or nginx:

```nginx
server {
    listen 443 ssl http2;
    server_name mail.example.com;

    ssl_certificate     /etc/letsencrypt/live/mail.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mail.example.com/privkey.pem;

    # Must exceed the largest message you accept, or nginx truncates it.
    client_max_body_size 26m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 150s;
    }
}
```

The `client_max_body_size` (nginx) or equivalent request-size limit (Caddy,
Traefik) must be at least `BLINKMAIL_MAX_INBOUND_BYTES`, or the proxy rejects
large messages before the application sees them.

`INBOUND_URL` on the Worker must be the **public HTTPS** URL, since the Worker
runs on Cloudflare's network and cannot reach `127.0.0.1` on your VPS.

## Delivery semantics

Delivery failures are handled explicitly, so the semantics are worth stating
plainly.

**A relay that swallows errors loses mail silently.** If the Worker's `email()`
handler wraps everything in a `try/catch` that logs the error and returns
normally, Cloudflare interprets the normal return as successful delivery, so a
parse failure, a database error, or an oversized message is acknowledged and
then discarded, with no bounce and no retry.

**Blinkmail reports failures honestly:**

- The application commits the inbox and the message in one transaction and
  returns `200` only after that commit succeeds. A duplicate delivery (detected by
  hashing envelope-from + envelope-to + raw bytes against a UNIQUE constraint)
  also returns `200`, so the Worker does not retry forever.
- The Worker treats a non-200 response as failure and distinguishes two cases:
  - **Permanent** (`message.setReject()` → sender bounces, no retry): oversized
    message, missing envelope recipient, or a 4xx from the application (bad
    secret, invalid envelope, unsupported domain).
  - **Transient** (throw → Cloudflare may retry): the VPS is unreachable, a
    network error occurred, or the application returned 5xx.

**There is no durable relay queue.** If the VPS is down or unreachable when a
message arrives, delivery depends entirely on Cloudflare's own retry policy.
Prolonged downtime will lose mail. A queue (Durable Object, KV, or a Worker
retry backlog) is explicitly out of scope for this version.

**Attachment handling:** attachments are not stored or downloadable. Only the
subject and a text body are kept. HTML-only messages are stored as text and
rendered inertly.

## Retention and cleanup

Messages are retained indefinitely. Blinkmail has no automatic expiry and no
deletion API — retention is an operator responsibility.

### Removing a specific inbox

Removing an address in the UI does **not** delete anything. To delete an inbox
and its mail, use SQL directly. Foreign keys are enabled, so delete the messages
and session links first:

```bash
docker compose exec blinkmail /usr/local/bin/blinkmail -healthcheck   # confirm alive

# There is no sqlite3 binary in the distroless image; run it from a helper
# container against the same volume:
docker run --rm -v blinkmail-data:/data alpine \
  sh -c 'apk add --no-cache sqlite >/dev/null && sqlite3 /data/blinkmail.db \
    "PRAGMA foreign_keys=ON;
     DELETE FROM messages WHERE inbox_address = '"'"'spam@mail.example.com'"'"';
     DELETE FROM session_inboxes WHERE inbox_address = '"'"'spam@mail.example.com'"'"';
     DELETE FROM inboxes WHERE address = '"'"'spam@mail.example.com'"'"';"'
```

### Removing old messages

```sql
-- Everything older than 30 days (timestamps are RFC 3339 UTC):
DELETE FROM messages
WHERE received_at < strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-30 days');
```

### Reclaiming space

SQLite does not return freed pages to the filesystem until you vacuum. With WAL
mode enabled, `VACUUM` requires roughly as much free disk as the database itself,
and takes a write lock for its duration:

```sql
PRAGMA wal_checkpoint(TRUNCATE);   -- fold the WAL back into the main file
VACUUM;
```

Prefer running `VACUUM` during low traffic. Check sizes first:

```bash
docker run --rm -v blinkmail-data:/data alpine ls -lh /data
```

### Backups

Do not copy the database file while the server is writing — with WAL mode, a
cold copy can miss committed transactions that are still in the `-wal` file.
Use SQLite's own online backup instead:

```bash
docker run --rm -v blinkmail-data:/data -v "$PWD/backups:/backup" alpine \
  sh -c 'apk add --no-cache sqlite >/dev/null && \
         sqlite3 /data/blinkmail.db ".backup '"'"'/backup/blinkmail-$(date +%F).db'"'"'"'
```

`.backup` produces a consistent single-file snapshot and is safe while the
application runs. Restore by stopping the container and replacing the file:

```bash
docker compose stop blinkmail
docker run --rm -v blinkmail-data:/data -v "$PWD/backups:/backup" alpine \
  cp /backup/blinkmail-2026-10-21.db /data/blinkmail.db
docker compose start blinkmail
```

Migrations run on every startup and are idempotent, so a restored database is
upgraded automatically if it predates the current schema.

### WAL mode notes

WAL leaves `blinkmail.db-wal` and `blinkmail.db-shm` beside the main file. That
is normal. All three files must be backed up together, or use `.backup`, which
collapses them into one consistent file. Deleting `-wal` while the server is
running can lose committed data.

## Security notes

- The inbound secret is compared in constant time and is never returned by any
  public route or written to logs.
- Session tokens come from `crypto/rand` and are validated against the database;
  a client cannot register an arbitrary token, and random addresses are generated
  from secure randomness so they cannot be guessed or enumerated.
- All SQL uses bound parameters; no user input is interpolated into queries.
- Untrusted mail content (sender, subject, body) and error messages are rendered
  through text nodes only, never `innerHTML`, so incoming HTML or script cannot
  execute in a reader's browser.
- The container runs as `nonroot` (uid 65532) with `no-new-privileges`.

## Development

```bash
go build ./...
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux go build -o /tmp/blinkmail-linux ./cmd/blinkmail
cd relay && node --test
```

## Troubleshooting

**`refusing to start an unauthenticated inbound mail endpoint`**
`BLINKMAIL_INBOUND_SECRET` is unset or shorter than 32 characters. Generate one
with `openssl rand -hex 32` and set it in `.env`.

**`BLINKMAIL_MAIL_DOMAINS must list at least one domain`**
Set a comma-separated list of real domains, e.g. `mail.example.com,mail.example.org`.

**Worker returns 401 / messages are rejected permanently with "rejected message (401)"**
The Worker's `INBOUND_SECRET` does not match the application's
`BLINKMAIL_INBOUND_SECRET`. Re-run `npx wrangler secret put INBOUND_SECRET`.

**Worker throws "Relay failed to reach ..."**
`INBOUND_URL` is wrong, the VPS is down, or TLS is failing. The Worker cannot
reach `127.0.0.1` — use the public HTTPS URL. Check `docker compose logs` and
your reverse proxy logs.

**Mail accepted by Cloudflare but nothing appears**
The catch-all routing rule is missing or points at the wrong Worker. Verify with
`npx wrangler email routing rules list example.com` and watch
`npx wrangler tail blinkmail-relay` while sending a test message.

**Large messages fail with 413**
Raise `BLINKMAIL_MAX_INBOUND_BYTES` and the Worker's `MAX_RAW_BYTES` together,
and raise your reverse proxy's request-size limit to match.

**Messages appear with the wrong timestamp**
Timestamps are stored as RFC 3339 UTC and rendered in your local timezone by the
browser. A shift suggests an incorrect system clock on the VPS — check
`timedatectl` and enable NTP.

**Database is locked errors under load**
WAL mode serialises writers; `busy_timeout` is 5 s. Sustained contention usually
means the volume is on slow storage. Move it to local SSD rather than network
storage.

## License

MIT. See [LICENSE](LICENSE). Third-party dependencies are listed in
[NOTICE](NOTICE).
