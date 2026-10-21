package api_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/store"
)

// TestInboundRequiresAuth verifies missing or wrong secrets are rejected.
func TestInboundRequiresAuth(t *testing.T) {
	e := newTestEnv(t)

	// No Authorization header.
	w := e.deliverRaw("Subject: No auth\n\nbody", "from@x.com", "to@mail.example.com", nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", w.Code)
	}

	// Wrong secret.
	w = e.deliverRaw("Subject: Wrong\n\nbody", "from@x.com", "to@mail.example.com", map[string]string{
		"Authorization": "Bearer wrong-secret-value-at-least-32-chars-long",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: status = %d, want 401", w.Code)
	}

	// Missing "Bearer " prefix.
	w = e.deliverRaw("Subject: No prefix\n\nbody", "from@x.com", "to@mail.example.com", map[string]string{
		"Authorization": e.secret,
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no prefix: status = %d, want 401", w.Code)
	}

	// Empty Bearer value.
	w = e.deliverRaw("Subject: Empty\n\nbody", "from@x.com", "to@mail.example.com", map[string]string{
		"Authorization": "Bearer ",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("empty bearer: status = %d, want 401", w.Code)
	}
}

// TestInboundValidatesEnvelopeTo verifies recipient validation and domain checks.
func TestInboundValidatesEnvelopeTo(t *testing.T) {
	e := newTestEnv(t)

	cases := []struct {
		name       string
		envelopeTo string
		wantStatus int
		wantBody   string // substring to look for in the error
	}{
		{"missing header", "", http.StatusBadRequest, "Missing X-Envelope-To"},
		{"unsupported domain", "user@evil.com", http.StatusBadRequest, "Unsupported recipient domain"},
		{"no at sign", "not-an-email", http.StatusBadRequest, "Invalid envelope recipient"},
		{"empty local", "@mail.example.com", http.StatusBadRequest, "Invalid envelope recipient"},
		{"empty domain", "user@", http.StatusBadRequest, "Invalid envelope recipient"},
		{"space in local", "bad name@mail.example.com", http.StatusBadRequest, "Invalid envelope recipient"},
		{"valid configured domain 1", "user@mail.example.com", http.StatusOK, ""},
		{"valid configured domain 2", "user@mail.example.org", http.StatusOK, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build headers fresh per subtest so no state leaks between cases.
			// deliverRaw sets X-Envelope-To from its parameter, so an empty
			// envelopeTo exercises the missing-header path.
			w := e.deliverRaw("Subject: Test\n\nbody", "from@x.com", tc.envelopeTo, map[string]string{
				"Authorization": "Bearer " + e.secret,
			})
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Errorf("body = %s, want %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestInboundEnvelopeFromNullReversePath verifies the SMTP null reverse-path
// is accepted (bounces) and falls back to the MIME From header for display.
func TestInboundEnvelopeFromNullReversePath(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	raw := "From: bounce@mailer.example.org\nSubject: Auto-reply\n\nbody"
	w := e.deliverRaw(raw, "<>", "user@mail.example.com", map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	// Claim the inbox and verify the message is stored with the MIME From
	// address, not the null reverse-path.
	e.createInbox(t, sid, "user", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode("user@mail.example.com")+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("messages: %d", w.Code)
	}
	var msgs []struct {
		FromAddress string `json:"from_address"`
		Subject     string `json:"subject"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if msgs[0].FromAddress != "bounce@mailer.example.org" {
		t.Errorf("from_address = %q, want the MIME From", msgs[0].FromAddress)
	}
}

// TestInboundEnvelopeFromFallbackToUnknown verifies that when both the envelope
// and MIME From are missing, "unknown" is stored rather than an empty string.
func TestInboundEnvelopeFromFallbackToUnknown(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	raw := "Subject: No sender\n\nbody"
	w := e.deliverRaw(raw, "<>", "unknown@mail.example.com", map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	e.createInbox(t, sid, "unknown", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode("unknown@mail.example.com")+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	var msgs []struct {
		FromAddress string `json:"from_address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d", len(msgs))
	}
	if msgs[0].FromAddress != "unknown" {
		t.Errorf("from_address = %q, want 'unknown'", msgs[0].FromAddress)
	}
}

// TestInboundEnvelopeFromInvalidRejected verifies a malformed envelope sender
// is rejected with 400.
func TestInboundEnvelopeFromInvalidRejected(t *testing.T) {
	e := newTestEnv(t)

	for _, bad := range []string{"not an email", "@nodomain.com", "user@", "user@nodot"} {
		w := e.deliverRaw("Subject: Bad from\n\nbody", bad, "user@mail.example.com", map[string]string{
			"Authorization": "Bearer " + e.secret,
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("envelope from %q: status = %d, want 400", bad, w.Code)
		}
		if !strings.Contains(w.Body.String(), "Invalid envelope sender") {
			t.Errorf("body = %s, want 'Invalid envelope sender'", w.Body.String())
		}
	}
}

// TestInboundEmptyBody verifies an empty raw body is rejected.
func TestInboundEmptyBody(t *testing.T) {
	e := newTestEnv(t)

	w := e.deliverRaw("", "from@x.com", "user@mail.example.com", map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "empty") {
		t.Errorf("body = %s, want mention of empty body", w.Body.String())
	}
}

// TestInboundSizeLimit verifies messages exceeding the configured limit are 413.
func TestInboundSizeLimit(t *testing.T) {
	e := newTestEnv(t)

	// The test env sets a 1 MiB limit. Exceed it.
	bigBody := strings.Repeat("A", (1<<20)+1000)
	w := e.deliverRaw("Subject: Big\n\n"+bigBody, "from@x.com", "user@mail.example.com", map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	if !strings.Contains(w.Body.String(), "exceeds") {
		t.Errorf("body = %s", w.Body.String())
	}

	// Exactly at the limit should succeed.
	atLimit := strings.Repeat("B", (1<<20)-100)
	w = e.deliverRaw("Subject: At limit\n\n"+atLimit, "from@x.com", "user2@mail.example.com", map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusOK {
		t.Errorf("at limit: status = %d, want 200", w.Code)
	}
}

// TestInboundDeduplication verifies identical deliveries (same envelope + raw)
// are acknowledged as duplicates rather than stored twice.
func TestInboundDeduplication(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	raw := "Subject: Dedupe test\n\nbody"
	addr := "dedupe@mail.example.com"
	headers := map[string]string{
		"Authorization":   "Bearer " + e.secret,
		"X-Envelope-From": "from@example.org",
		"X-Envelope-To":   addr,
	}

	// First delivery: should succeed and store.
	w := e.deliverRaw(raw, "from@example.org", addr, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("first: status = %d", w.Code)
	}
	var resp1 struct {
		OK        bool   `json:"ok"`
		Duplicate bool   `json:"duplicate"`
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp1.OK || resp1.Duplicate {
		t.Errorf("first response = %+v", resp1)
	}
	firstID := resp1.MessageID

	// Identical second delivery: should return duplicate:true.
	w = e.deliverRaw(raw, "from@example.org", addr, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("second: status = %d", w.Code)
	}
	var resp2 struct {
		OK        bool   `json:"ok"`
		Duplicate bool   `json:"duplicate"`
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp2.OK || !resp2.Duplicate {
		t.Errorf("second response = %+v, want duplicate:true", resp2)
	}
	if resp2.MessageID != "" {
		t.Errorf("duplicate has messageId = %q, want empty", resp2.MessageID)
	}

	// Verify only one message was stored.
	e.createInbox(t, sid, "dedupe", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	var msgs []struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("messages = %d, want 1 (deduplicated)", len(msgs))
	}
	if msgs[0].ID != firstID {
		t.Errorf("message ID = %q, want the first ID %q", msgs[0].ID, firstID)
	}
}

// TestInboundDifferentRawNotDeduped verifies that changing the raw bytes for
// the same envelope produces a distinct delivery.
func TestInboundDifferentRawNotDeduped(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr := "distinct@mail.example.com"
	headers := map[string]string{
		"Authorization":   "Bearer " + e.secret,
		"X-Envelope-From": "from@example.org",
		"X-Envelope-To":   addr,
	}

	raw1 := "Subject: First\n\nbody one"
	raw2 := "Subject: Second\n\nbody two"

	w := e.deliverRaw(raw1, "from@example.org", addr, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("first: %d", w.Code)
	}
	w = e.deliverRaw(raw2, "from@example.org", addr, headers)
	if w.Code != http.StatusOK {
		t.Fatalf("second: %d", w.Code)
	}
	var resp2 struct {
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp2.Duplicate {
		t.Error("different raw bytes were deduplicated")
	}

	e.createInbox(t, sid, "distinct", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	var msgs []struct{ Subject string }
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("messages = %d, want 2 (distinct deliveries)", len(msgs))
	}
}

// TestInboundCreatesMissingInbox verifies mail to an unclaimed address
// auto-creates the inbox so a later browser claim finds it.
func TestInboundCreatesMissingInbox(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr := "preexisting@mail.example.com"
	exists, err := e.store.InboxExists(t.Context(), addr)
	if err != nil {
		t.Fatalf("InboxExists: %v", err)
	}
	if exists {
		t.Fatal("inbox already exists before delivery")
	}

	w := e.deliverRaw("Subject: Early\n\nbody", "from@x.com", addr, map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	// The inbox must now exist.
	exists, err = e.store.InboxExists(t.Context(), addr)
	if err != nil {
		t.Fatalf("InboxExists: %v", err)
	}
	if !exists {
		t.Error("inbox was not auto-created by delivery")
	}

	// A browser claiming it must see the message.
	e.createInbox(t, sid, "preexisting", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("messages: %d", w.Code)
	}
	var msgs []struct{ Subject string }
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Early" {
		t.Errorf("messages = %+v, want the early message", msgs)
	}
}

// TestInboundDeliveryHashMatches verifies the deduplication key is computed
// correctly and matches the store layer's hash.
func TestInboundDeliveryHashMatches(t *testing.T) {
	e := newTestEnv(t)

	raw := "Subject: Hash test\n\nbody"
	envelopeFrom := "from@example.org"
	addr := "hash@mail.example.com"

	w := e.deliverRaw(raw, envelopeFrom, addr, map[string]string{
		"Authorization": "Bearer " + e.secret,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	// Compute the expected hash independently.
	expected := store.DeliveryHash(envelopeFrom, addr, []byte(raw))

	// Query the database and verify the stored delivery_hash matches.
	var got string
	err := e.store.DB().QueryRowContext(t.Context(),
		`SELECT delivery_hash FROM messages WHERE inbox_address = ?`, addr).Scan(&got)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got != expected {
		t.Errorf("delivery_hash = %q, want %q", got, expected)
	}

	// Verify it's the correct SHA-256 of envelope-from + envelope-to + raw.
	h := sha256.New()
	writeLenPrefixed(h, envelopeFrom)
	writeLenPrefixed(h, addr)
	h.Write([]byte(raw))
	wantHex := hex.EncodeToString(h.Sum(nil))
	if got != wantHex {
		t.Errorf("delivery_hash = %q, want SHA-256 %q", got, wantHex)
	}
}

func writeLenPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	l := uint32(len(s))
	h.Write([]byte{byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)})
	h.Write([]byte(s))
}

// TestInboundParsesMIMECorrectly verifies that MIME parsing (encoded subjects,
// multipart, charsets, transfer encodings) works end-to-end through the API.
func TestInboundParsesMIMECorrectly(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	cases := []struct {
		name        string
		raw         string
		wantSubject string
		wantBody    string
	}{
		{
			"encoded subject UTF-8 base64",
			"Subject: =?UTF-8?B?SGVsbG8gQmxpbmttYWls?=\nContent-Type: text/plain\n\nbody",
			"Hello Blinkmail",
			"body",
		},
		{
			"multipart alternative prefers plain",
			"Subject: Multi\nContent-Type: multipart/alternative; boundary=B\n\n--B\nContent-Type: text/plain\n\nplain\n--B\nContent-Type: text/html\n\n<p>html</p>\n--B--",
			"Multi",
			"plain",
		},
		{
			"iso-8859-1 charset",
			"Subject: Latin\nContent-Type: text/plain; charset=iso-8859-1\n\ncaf\xe9",
			"Latin",
			"café",
		},
		{
			"base64 transfer encoding",
			"Subject: B64\nContent-Type: text/plain\nContent-Transfer-Encoding: base64\n\naGVsbG8=",
			"B64",
			"hello",
		},
		{
			"HTML-only stored as text",
			"Subject: HTML\nContent-Type: text/html\n\n<script>alert(1)</script>",
			"HTML",
			"<script>alert(1)</script>",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr := "mime" + string(rune('0'+i)) + "@mail.example.com"
			w := e.deliverRaw(tc.raw, "from@x.com", addr, map[string]string{
				"Authorization": "Bearer " + e.secret,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}

			localPart := strings.TrimSuffix(addr, "@mail.example.com")
			e.createInbox(t, sid, localPart, "mail.example.com")
			w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
				"x-session-id": sid,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("messages: %d", w.Code)
			}
			var msgs []struct {
				Subject string `json:"subject"`
				Body    string `json:"body"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("messages = %d, want 1", len(msgs))
			}
			if msgs[0].Subject != tc.wantSubject {
				t.Errorf("subject = %q, want %q", msgs[0].Subject, tc.wantSubject)
			}
			if msgs[0].Body != tc.wantBody {
				t.Errorf("body = %q, want %q", msgs[0].Body, tc.wantBody)
			}
		})
	}
}
