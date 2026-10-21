-- 001_init: Blinkmail base schema.
--
-- Preserves the four-table model of the Tempik reference implementation
-- (MIT License, Copyright (c) 2026 masantoid) and adds only what Blinkmail
-- needs for delivery deduplication and safe operation:
--
--   * messages.delivery_hash  UNIQUE  - dedupes repeated Email Worker deliveries
--   * messages.subject/body   NOT NULL - prevents a parser writing NULL past the default
--   * timestamps in RFC 3339 UTC ("...Z") so browsers parse them as UTC rather
--     than as local time.

CREATE TABLE IF NOT EXISTS inboxes (
  address    TEXT PRIMARY KEY,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS messages (
  id            TEXT PRIMARY KEY,
  inbox_address TEXT NOT NULL REFERENCES inboxes(address),
  from_address  TEXT NOT NULL,
  subject       TEXT NOT NULL DEFAULT '(no subject)',
  body          TEXT NOT NULL DEFAULT '',
  received_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  -- SHA-256 of envelope-from + envelope-to + raw MIME for this delivery.
  delivery_hash TEXT UNIQUE
);

CREATE INDEX IF NOT EXISTS idx_messages_inbox    ON messages(inbox_address);
CREATE INDEX IF NOT EXISTS idx_messages_received ON messages(inbox_address, received_at DESC);

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS session_inboxes (
  session_id    TEXT NOT NULL REFERENCES sessions(id),
  inbox_address TEXT NOT NULL REFERENCES inboxes(address),
  created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
  PRIMARY KEY (session_id, inbox_address)
);

CREATE INDEX IF NOT EXISTS idx_session_inboxes_session ON session_inboxes(session_id);
