-- 002_auth: web UI authentication.
--
-- Two tables, deliberately separate from the browser inbox model:
--
--   * settings       - key/value store. Holds the admin password hash under
--                      "admin_password". A table rather than a dedicated column
--                      so later settings (T04 API keys, UX preferences) need no
--                      new migration.
--   * auth_sessions  - server-side login sessions. A row exists only while the
--                      expiry is in the future; the cookie carries the id and
--                      nothing else, so a stolen cookie can be revoked server
--                      side by deleting the row.
--
-- The existing sessions table (per-browser inbox organisation) is untouched:
-- authentication is a gate in front of it, not a replacement.

CREATE TABLE IF NOT EXISTS settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS auth_sessions (
  id         TEXT PRIMARY KEY,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_auth_sessions_expires ON auth_sessions(expires_at);
