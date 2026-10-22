-- 004_api_key_scopes: per-key permissions.
--
-- A key is a capability, not an identity: the relay only needs to deliver mail,
-- an integration only needs to read it. Keeping the set on the key means one
-- leaked read-only key cannot also inject mail.
--
-- Comma-separated rather than a join table: the set is two fixed values, and a
-- second table would add a migration, a query and a transaction for nothing.
-- Rows that predate this column were used both ways, so they keep both.

ALTER TABLE api_keys ADD COLUMN permissions TEXT NOT NULL DEFAULT 'inbound,read';
