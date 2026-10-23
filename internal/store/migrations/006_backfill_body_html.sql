-- 006_backfill_body_html: give pre-005 messages their HTML part back.
--
-- Rows in this state exist because the parser that predates 005_message_html had
-- only one body column: when a message had no text/plain part it stored the
-- text/html source in `body`. 005 added `body_html` with a default of '', so
-- every one of those rows kept `body_html = ''` and the reader fell back to
-- plain text, showing raw `<html>...` markup in the inbox. For messages that had
-- both parts nothing can be recovered (the HTML was never stored); only the
-- "no plain part" rows whose `body` *is* HTML source can be repaired, which is
-- what this statement does.
--
-- The predicate is deliberately conservative. An HTML-looking first token alone
-- is not enough: a plain-text mail can legitimately start with `<p>` and never
-- close it, and silently rendering that as HTML would corrupt it. So a body must
-- also contain a closing tag (`</`) before it is treated as markup. Bodies whose
-- `body_html` is already set are excluded, which makes the statement a no-op on
-- already-migrated and newly parsed rows and therefore safe to re-run.

UPDATE messages
SET body_html = body
WHERE body_html = ''
  AND length(body) > 0
  AND body LIKE '%</%'
  AND (
    lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<html%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<!doctype%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<body%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<div%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<table%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<center%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<span%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<p>%'
    OR lower(ltrim(body, char(9) || char(10) || char(13) || ' ')) LIKE '<p %'
  );
