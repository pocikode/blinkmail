// Package mail parses inbound MIME messages into the fields Blinkmail stores.
//
// Parsing uses github.com/emersion/go-message together with its charset
// package, which covers RFC 2047 encoded subjects, transfer encodings
// (base64, quoted-printable), multipart nesting, and non-UTF-8 character sets.
//
// Attachments are deliberately ignored: Blinkmail stores only a subject and a
// text body. When a message carries no text/plain part, the text/html part is
// stored verbatim. The frontend renders every body through text nodes, so HTML
// arrives as escaped text rather than being executed.
package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset" // registers CharsetReader for non-UTF-8 bodies
	"github.com/emersion/go-message/mail"
)

// Parsing limits.
const (
	// MaxBodyBytes caps the stored body. A 25 MiB raw message could otherwise
	// put megabytes of text into one SQLite row.
	MaxBodyBytes = 1 << 20 // 1 MiB
	// MaxParts caps how many MIME parts are examined, so a deeply nested or
	// part-bomb message cannot exhaust CPU.
	MaxParts = 256
)

// NoSubject is stored when a message has no usable Subject header. It matches
// the reference implementation's placeholder.
const NoSubject = "(no subject)"

// truncationMarker is appended when a body exceeds MaxBodyBytes.
const truncationMarker = "\n\n[message truncated]"

// Parsed is the extracted, displayable content of one inbound message.
type Parsed struct {
	// Subject is the decoded Subject header, or NoSubject when absent.
	Subject string
	// Body is the preferred text body. Plain text wins over HTML.
	Body string
	// From is the first address in the MIME From header, used only as a
	// fallback when the SMTP envelope sender is the null reverse-path.
	From string
	// HTMLOnly reports that Body came from a text/html part. The frontend
	// still renders it as escaped text.
	HTMLOnly bool
}

// ErrUnparsable reports a message whose MIME headers could not be read at all.
// Unlike a merely odd message, this cannot be stored meaningfully.
var ErrUnparsable = errors.New("message is not parsable MIME")

// Parse reads raw MIME bytes and extracts the subject and body.
//
// It is deliberately lenient: real-world mail routinely has malformed parts,
// unknown charsets, or broken transfer encodings. Whatever can be salvaged is
// returned. Only a message with no readable headers yields ErrUnparsable.
func Parse(raw []byte) (Parsed, error) {
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		// CreateReader returns a usable reader for unknown charsets; anything
		// else means the headers themselves are unreadable.
		if !message.IsUnknownCharset(err) {
			return Parsed{}, fmt.Errorf("%w: %v", ErrUnparsable, err)
		}
	}
	if reader == nil {
		return Parsed{}, ErrUnparsable
	}
	defer reader.Close()

	parsed := Parsed{
		Subject: subjectOf(reader.Header),
		From:    fromOf(reader.Header),
	}

	var plain, html string
	for range MaxParts {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !message.IsUnknownCharset(err) {
			// Skip the unreadable part but keep any content already gathered.
			continue
		}
		if part == nil {
			continue
		}

		// Attachments (including text/* parts marked as attachments) are not
		// stored, so their bodies are never read.
		inline, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		mediaType, _, _ := inline.ContentType()
		body, readErr := readPart(part.Body)
		if readErr != nil && body == "" {
			continue
		}
		if strings.TrimSpace(body) == "" {
			continue
		}

		switch strings.ToLower(mediaType) {
		case "text/plain":
			if plain == "" {
				plain = body
			}
		case "text/html":
			if html == "" {
				html = body
			}
		}
	}

	switch {
	case plain != "":
		parsed.Body = normalize(plain)
	case html != "":
		parsed.Body = normalize(html)
		parsed.HTMLOnly = true
	}

	return parsed, nil
}

// subjectOf decodes the Subject header, falling back to NoSubject.
func subjectOf(h mail.Header) string {
	subject, err := h.Subject()
	if err != nil {
		// An undecodable subject is still worth showing raw rather than losing.
		if raw := strings.TrimSpace(h.Get("Subject")); raw != "" {
			return raw
		}
		return NoSubject
	}
	if s := strings.TrimSpace(subject); s != "" {
		return s
	}
	return NoSubject
}

// fromOf returns the first From address, or "" when absent or unparseable.
func fromOf(h mail.Header) string {
	addrs, err := h.AddressList("From")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	if a := addrs[0]; a != nil {
		return strings.TrimSpace(a.Address)
	}
	return ""
}

// readPart reads a part body up to MaxBodyBytes.
func readPart(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	limited := io.LimitReader(r, MaxBodyBytes+int64(len(truncationMarker)))
	b, err := io.ReadAll(limited)
	if err != nil && len(b) == 0 {
		return "", err
	}
	return string(b), err
}

// normalize trims surrounding whitespace and enforces the stored body cap.
func normalize(body string) string {
	body = strings.TrimSpace(body)
	// Normalize CRLF and lone CR so the frontend's pre-wrap rendering is
	// consistent regardless of the sender's line endings.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")

	if len(body) > MaxBodyBytes {
		body = body[:MaxBodyBytes]
		// Avoid splitting a UTF-8 sequence mid-rune when truncating.
		for len(body) > 0 && !isValidUTF8Tail(body) {
			body = body[:len(body)-1]
		}
		body += truncationMarker
	}
	return body
}

// isValidUTF8Tail reports whether s ends on a rune boundary.
func isValidUTF8Tail(s string) bool {
	if s == "" {
		return true
	}
	// Walk back over continuation bytes (10xxxxxx); a valid boundary means the
	// byte we land on starts a rune that fits in the remaining bytes.
	i := len(s) - 1
	for i >= 0 && i > len(s)-4 && s[i]&0xC0 == 0x80 {
		i--
	}
	if i < 0 {
		return false
	}
	lead := s[i]
	var size int
	switch {
	case lead&0x80 == 0x00:
		size = 1
	case lead&0xE0 == 0xC0:
		size = 2
	case lead&0xF0 == 0xE0:
		size = 3
	case lead&0xF8 == 0xF0:
		size = 4
	default:
		return false
	}
	return i+size == len(s)
}
