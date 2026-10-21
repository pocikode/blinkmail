-- 003_api_keys: managed API keys.
--
-- Keys are created and revoked from the web UI, so rotating one no longer
-- means restarting the app or sharing a static secret through the environment.
--
-- Only a SHA-256 of the plaintext is stored. A 256-bit random key has no
-- guessable structure to slow down, so a KDF would add cost without adding
-- security; the hash exists to keep the plaintext out of the database.
--
-- prefix is the first 12 characters ("bm_" + 9 hex) so the UI can tell keys
-- apart without ever holding the secret again.

CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  last_used_at TEXT
);
