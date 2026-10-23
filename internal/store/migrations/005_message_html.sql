-- 005_message_html: keep the text/html part alongside the text body.
--
-- Before this column the parser had to choose: it stored text/plain when a
-- message had one and otherwise the text/html source, so the reader could only
-- ever show markup as literal text. Storing the HTML part separately lets the
-- web client render real HTML mail while `body` keeps its old meaning
-- (text/plain when present, otherwise the HTML source), so API consumers and
-- existing rows are unaffected.
--
-- Existing rows get the default '' and keep rendering as plain text; there is
-- no way to recover an HTML part that was never stored.

ALTER TABLE messages ADD COLUMN body_html TEXT NOT NULL DEFAULT '';
