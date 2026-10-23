package mail_test

import (
	"strings"
	"testing"

	"pocikode/blinkmail/internal/mail"
)

func TestParsePlainText(t *testing.T) {
	raw := []byte("From: sender@example.com\r\n" +
		"Subject: Plain hello\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Line one\r\nLine two\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Subject != "Plain hello" {
		t.Errorf("Subject = %q, want %q", got.Subject, "Plain hello")
	}
	if got.Body != "Line one\nLine two" {
		t.Errorf("Body = %q, want CRLF normalized and trimmed", got.Body)
	}
	if got.From != "sender@example.com" {
		t.Errorf("From = %q, want sender@example.com", got.From)
	}
	if got.HTMLOnly {
		t.Error("HTMLOnly = true for a plain text message")
	}
}

func TestParsePrefersPlainTextOverHTML(t *testing.T) {
	raw := []byte("Subject: Alternative\r\n" +
		"Content-Type: multipart/alternative; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"the plain version\r\n" +
		"--B\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>the html version</p>\r\n" +
		"--B--\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "the plain version" {
		t.Errorf("Body = %q, want the plain text part", got.Body)
	}
	if got.HTMLOnly {
		t.Error("HTMLOnly = true when a plain text part was available")
	}
	// The HTML part is kept separately so the web client can render it; Body
	// stays the plain text version for API consumers.
	if got.HTML != "<p>the html version</p>" {
		t.Errorf("HTML = %q, want the html part", got.HTML)
	}
}

func TestParsePlainTextHasNoHTMLPart(t *testing.T) {
	raw := []byte("Subject: No html\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"just text\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.HTML != "" {
		t.Errorf("HTML = %q, want empty for a text/plain message", got.HTML)
	}
}

func TestParseHTMLOnlyKeepsMarkupInBothFields(t *testing.T) {
	raw := []byte("Subject: HTML only\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>Hello <b>there</b></p>\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// Body keeps the historical behaviour: the HTML source, for API clients
	// that predate the separate field.
	if got.Body != "<p>Hello <b>there</b></p>" {
		t.Errorf("Body = %q, want the html source", got.Body)
	}
	if got.HTML != got.Body {
		t.Errorf("HTML = %q, want it to match Body for an HTML-only message", got.HTML)
	}
}

func TestParseHTMLOnlyIsStoredAsText(t *testing.T) {
	// A message with only HTML must still be stored — as literal text, which
	// the frontend renders inertly. The parser must NOT strip or execute it,
	// and must flag it so callers know the body was HTML.
	raw := []byte("Subject: HTML only\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><script>alert(1)</script><p>Hi <b>there</b></p></body></html>\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !got.HTMLOnly {
		t.Error("HTMLOnly = false for an HTML-only message")
	}
	// The raw markup is preserved verbatim; escaping is the frontend's job.
	if !strings.Contains(got.Body, "<script>alert(1)</script>") {
		t.Errorf("Body = %q, want the HTML preserved as text", got.Body)
	}
}

func TestParseEncodedSubject(t *testing.T) {
	// RFC 2047 encoded-word subject in UTF-8 (base64) — "Hello Blinkmail".
	raw := []byte("Subject: =?UTF-8?B?SGVsbG8gQmxpbmttYWls?=\r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Subject != "Hello Blinkmail" {
		t.Errorf("Subject = %q, want the decoded %q", got.Subject, "Hello Blinkmail")
	}
}

func TestParseEncodedSubjectQuotedPrintable(t *testing.T) {
	// "Café" in RFC 2047 Q encoding.
	raw := []byte("Subject: =?utf-8?Q?Caf=C3=A9?=\r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Subject != "Café" {
		t.Errorf("Subject = %q, want Café", got.Subject)
	}
}

func TestParseNonUTF8CharsetBody(t *testing.T) {
	// Latin-1 body: "caf\xe9" is "café" in ISO-8859-1. go-message's charset
	// package must decode it to UTF-8.
	raw := []byte("Subject: Latin\r\n" +
		"Content-Type: text/plain; charset=iso-8859-1\r\n" +
		"\r\ncaf\xe9 na\xefve\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "café naïve" {
		t.Errorf("Body = %q, want iso-8859-1 decoded to %q", got.Body, "café naïve")
	}
}

func TestParseBase64TransferEncoding(t *testing.T) {
	// "hello base64 world" encoded in base64.
	raw := []byte("Subject: B64\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\naGVsbG8gYmFzZTY0IHdvcmxk\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "hello base64 world" {
		t.Errorf("Body = %q, want decoded base64", got.Body)
	}
}

func TestParseQuotedPrintableTransferEncoding(t *testing.T) {
	// "100% = done" with a soft line break, quoted-printable encoded.
	raw := []byte("Subject: QP\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n100% =3D =\r\ndone\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "100% = done" {
		t.Errorf("Body = %q, want decoded quoted-printable", got.Body)
	}
}

func TestParseMissingSubjectUsesPlaceholder(t *testing.T) {
	raw := []byte("From: a@b.com\r\nContent-Type: text/plain\r\n\r\nbody only\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Subject != mail.NoSubject {
		t.Errorf("Subject = %q, want %q", got.Subject, mail.NoSubject)
	}
}

func TestParseNestedMultipart(t *testing.T) {
	// multipart/mixed containing a multipart/alternative plus an attachment.
	// The nested plain text must be found and the attachment ignored.
	raw := []byte("Subject: Nested\r\n" +
		"Content-Type: multipart/mixed; boundary=OUTER\r\n" +
		"\r\n" +
		"--OUTER\r\n" +
		"Content-Type: multipart/alternative; boundary=INNER\r\n" +
		"\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"nested plain\r\n" +
		"--INNER\r\n" +
		"Content-Type: text/html\r\n" +
		"\r\n" +
		"<p>nested html</p>\r\n" +
		"--INNER--\r\n" +
		"--OUTER\r\n" +
		"Content-Type: application/pdf; name=\"doc.pdf\"\r\n" +
		"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"JVBERi0xLjQK\r\n" +
		"--OUTER--\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "nested plain" {
		t.Errorf("Body = %q, want the nested plain text", got.Body)
	}
	if strings.Contains(got.Body, "PDF") || strings.Contains(got.Body, "JVBERi") {
		t.Error("attachment content leaked into the body")
	}
}

func TestParseIgnoresAttachments(t *testing.T) {
	// A message whose ONLY substantive content is an attachment plus a short
	// text part. The attachment body must never appear.
	raw := []byte("Subject: Attach\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"see attached\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain; name=\"secret.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"secret.txt\"\r\n" +
		"\r\n" +
		"ATTACHMENT_SECRET_PAYLOAD\r\n" +
		"--B--\r\n")

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "see attached" {
		t.Errorf("Body = %q, want only the inline text", got.Body)
	}
	if strings.Contains(got.Body, "ATTACHMENT_SECRET_PAYLOAD") {
		t.Error("attachment body was stored; attachments must be ignored")
	}
}

// TestParseSalvagesPartiallyBrokenMessages verifies that odd-but-readable mail
// is parsed leniently: an unknown charset, a broken transfer encoding, or a
// malformed encoded-word subject must not discard the whole message.
func TestParseSalvagesPartiallyBrokenMessages(t *testing.T) {
	cases := map[string]struct {
		raw         string
		wantSubject string
		wantBody    string
	}{
		"unknown charset keeps ascii body": {
			raw:         "Subject: ?\r\nContent-Type: text/plain; charset=x-unknown-charset\r\n\r\nbody\r\n",
			wantSubject: "?",
			wantBody:    "body",
		},
		"broken base64 yields empty body not a crash": {
			raw:         "Subject: B\r\nContent-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!!not base64!!!!\r\n",
			wantSubject: "B",
			wantBody:    "",
		},
		"malformed encoded-word subject kept raw": {
			raw:         "Subject: =?UTF-8?B?====\r\nContent-Type: text/plain\r\n\r\nbody\r\n",
			wantSubject: "=?UTF-8?B?====",
			wantBody:    "body",
		},
		"truncated multipart salvages the open part": {
			raw:         "Content-Type: multipart/mixed; boundary=X\r\n\r\n--X\r\nContent-Type: text/plain\r\n\r\nbody that never closes\r\n",
			wantSubject: mail.NoSubject,
			wantBody:    "body that never closes",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := mail.Parse([]byte(tc.raw))
			if err != nil {
				t.Fatalf("Parse returned an error for salvageable input: %v", err)
			}
			if got.Subject != tc.wantSubject {
				t.Errorf("Subject = %q, want %q", got.Subject, tc.wantSubject)
			}
			if got.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tc.wantBody)
			}
		})
	}
}

// TestParseRejectsUnreadableHeaders verifies that input with no parsable header
// section at all surfaces ErrUnparsable, so the caller can reject the delivery
// rather than storing an empty message and acknowledging it.
func TestParseRejectsUnreadableHeaders(t *testing.T) {
	for name, raw := range map[string]string{
		"no header/body separator": "this is not email at all\r\n",
		"binary garbage":           "\x00\x01\x02\x03binary nonsense with no CRLF",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := mail.Parse([]byte(raw)); err == nil {
				t.Fatalf("Parse succeeded on unreadable input %q, want ErrUnparsable", raw)
			}
		})
	}
}

func TestParseTruncatesOversizedBody(t *testing.T) {
	// A body far larger than MaxBodyBytes must be truncated, not stored whole.
	big := strings.Repeat("A", mail.MaxBodyBytes+10000)
	raw := []byte("Subject: Big\r\nContent-Type: text/plain\r\n\r\n" + big)

	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got.Body) > mail.MaxBodyBytes+64 {
		t.Errorf("Body length = %d, want at most ~%d after truncation", len(got.Body), mail.MaxBodyBytes)
	}
	if !strings.Contains(got.Body, "truncated") {
		t.Error("truncated body should carry a truncation marker")
	}
}

func TestParseEmptyBody(t *testing.T) {
	raw := []byte("Subject: Empty\r\nContent-Type: text/plain\r\n\r\n")
	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Body != "" {
		t.Errorf("Body = %q, want empty", got.Body)
	}
	if got.Subject != "Empty" {
		t.Errorf("Subject = %q, want Empty", got.Subject)
	}
}

func TestParseFromWithDisplayName(t *testing.T) {
	raw := []byte("From: \"Alice Example\" <alice@example.org>\r\n" +
		"Subject: Display\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// From should be the bare address, not the display name.
	if got.From != "alice@example.org" {
		t.Errorf("From = %q, want the bare address", got.From)
	}
}

func TestParseMultipleFromUsesFirst(t *testing.T) {
	raw := []byte("From: a@one.com, b@two.com\r\n" +
		"Subject: Multi\r\nContent-Type: text/plain\r\n\r\nbody\r\n")
	got, err := mail.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.From != "a@one.com" {
		t.Errorf("From = %q, want the first address", got.From)
	}
}
