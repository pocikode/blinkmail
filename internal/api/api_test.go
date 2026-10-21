package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/api"
	"pocikode/blinkmail/internal/config"
	"pocikode/blinkmail/internal/store"
)

// testEnv bundles a Server, its store, and the configured secret for use in a
// single test.
type testEnv struct {
	srv    *api.Server
	store  *store.Store
	secret string
}

// newTestEnv builds a server backed by a temporary SQLite file.
func newTestEnv(t *testing.T) testEnv {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path, store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		ListenAddr:      ":8080",
		DBPath:          path,
		AppName:         "Blinkmail",
		MailDomains:     []string{"mail.example.com", "mail.example.org"},
		WebHost:         "mail.example.com",
		InboundSecret:   "test-secret-value-at-least-32-chars-long",
		MaxInboundBytes: 1 << 20, // 1 MiB for tests
	}

	// A discard logger keeps test output readable; the handlers log every
	// request by default.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := api.New(api.Deps{Config: cfg, Store: st, Logger: logger})
	return testEnv{srv: srv, store: st, secret: cfg.InboundSecret}
}

// doRequest is a convenience wrapper around http.NewRequest + ServeHTTP.
func (e testEnv) doRequest(method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			panic(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)
	return w
}

// getSession creates a session and returns its ID.
func (e testEnv) getSession(t *testing.T) string {
	t.Helper()
	w := e.doRequest(http.MethodGet, "/api/session", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("getSession: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("getSession unmarshal: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatal("getSession: empty sessionId")
	}
	return resp.SessionID
}

// createInbox is a helper for POST /api/inboxes.
func (e testEnv) createInbox(t *testing.T, sid, localPart, domain string) (string, int) {
	t.Helper()
	body := map[string]interface{}{}
	if localPart != "" {
		body["localPart"] = localPart
	}
	if domain != "" {
		body["domain"] = domain
	}
	w := e.doRequest(http.MethodPost, "/api/inboxes", body, map[string]string{
		"x-session-id": sid,
		"Content-Type": "application/json",
	})
	if w.Code >= http.StatusBadRequest {
		return "", w.Code
	}
	var inbox struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("createInbox unmarshal: %v", err)
	}
	return inbox.Address, w.Code
}

// TestGetConfig verifies the public settings endpoint and that the inbound
// secret is never leaked.
func TestGetConfig(t *testing.T) {
	e := newTestEnv(t)
	w := e.doRequest(http.MethodGet, "/api/config", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var cfg struct {
		AppName     string   `json:"appName"`
		MailDomain  string   `json:"mailDomain"`
		MailDomains []string `json:"mailDomains"`
		WebHost     string   `json:"webHost"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.AppName != "Blinkmail" {
		t.Errorf("appName = %q", cfg.AppName)
	}
	if cfg.MailDomain != "mail.example.com" {
		t.Errorf("mailDomain = %q", cfg.MailDomain)
	}
	if len(cfg.MailDomains) != 2 {
		t.Errorf("mailDomains = %v", cfg.MailDomains)
	}
	if cfg.WebHost != "mail.example.com" {
		t.Errorf("webHost = %q", cfg.WebHost)
	}

	// The secret must NOT appear anywhere in the response body or headers.
	if strings.Contains(w.Body.String(), e.secret) {
		t.Error("inbound secret leaked in config response")
	}
	for k, vals := range w.Header() {
		for _, v := range vals {
			if strings.Contains(v, e.secret) {
				t.Errorf("inbound secret leaked in header %s: %s", k, v)
			}
		}
	}
}

// TestSessionCreation verifies a new session is issued when no header is sent.
func TestSessionCreation(t *testing.T) {
	e := newTestEnv(t)

	sid1 := e.getSession(t)
	if len(sid1) != 64 {
		t.Errorf("sessionId length = %d, want 64 hex chars", len(sid1))
	}
	// Each call without a header issues a fresh token.
	sid2 := e.getSession(t)
	if sid1 == sid2 {
		t.Error("two new sessions returned the same token")
	}
}

// TestSessionResumption verifies a valid token is resumed rather than replaced.
func TestSessionResumption(t *testing.T) {
	e := newTestEnv(t)

	sid := e.getSession(t)
	w := e.doRequest(http.MethodGet, "/api/session", nil, map[string]string{
		"x-session-id": sid,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("resume: status = %d", w.Code)
	}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SessionID != sid {
		t.Errorf("resumed session = %q, want %q", resp.SessionID, sid)
	}
}

// TestUnknownSessionIsReplaced verifies a client-supplied string that is not in
// the database gets a fresh token instead of being registered.
func TestUnknownSessionIsReplaced(t *testing.T) {
	e := newTestEnv(t)

	unknown := strings.Repeat("f", 64)
	w := e.doRequest(http.MethodGet, "/api/session", nil, map[string]string{
		"x-session-id": unknown,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (replace unknown)", w.Code)
	}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SessionID == unknown {
		t.Error("unknown token was echoed back instead of being replaced")
	}
	if len(resp.SessionID) != 64 {
		t.Errorf("replacement length = %d", len(resp.SessionID))
	}
}

// TestMalformedSessionOnSessionRouteIsReplaced verifies that GET /api/session
// treats a token with the wrong shape the same as no token: it issues a fresh
// one rather than rejecting. This keeps the frontend's startup self-healing
// simple — a corrupted localStorage token is replaced on page load.
//
// The strict rejection boundary lives on the protected routes instead, where a
// malformed or unknown token must be refused (see TestProtectedRoutesEnforceSession).
func TestMalformedSessionOnSessionRouteIsReplaced(t *testing.T) {
	e := newTestEnv(t)

	for _, bad := range []string{
		"short",
		strings.Repeat("g", 64), // not hex
		strings.Repeat("F", 64), // uppercase
		strings.Repeat("0", 63), // too short
		strings.Repeat("0", 65), // too long
	} {
		w := e.doRequest(http.MethodGet, "/api/session", nil, map[string]string{
			"x-session-id": bad,
		})
		if w.Code != http.StatusOK {
			t.Errorf("token %q: status = %d, want 200 (issue fresh)", bad, w.Code)
			continue
		}
		var resp struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// The malformed token must not be echoed back or registered.
		if resp.SessionID == bad {
			t.Errorf("token %q was echoed back instead of replaced", bad)
		}
		if len(resp.SessionID) != 64 {
			t.Errorf("replacement length = %d, want 64", len(resp.SessionID))
		}
		exists, err := e.store.SessionExists(t.Context(), bad)
		if err != nil {
			t.Fatalf("SessionExists: %v", err)
		}
		if exists {
			t.Errorf("malformed token %q was registered as a session", bad)
		}
	}
}

// TestProtectedRoutesEnforceSession verifies the 400-vs-401 boundary on routes
// that require a session: a missing header is 400, while a well-formed but
// unknown token is 401 (which the frontend uses to trigger re-authentication).
func TestProtectedRoutesEnforceSession(t *testing.T) {
	e := newTestEnv(t)

	// Missing header -> 400.
	w := e.doRequest(http.MethodGet, "/api/inboxes", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing header: status = %d, want 400", w.Code)
	}

	// Well-formed but unknown token -> 401.
	unknown := strings.Repeat("f", 64)
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{
		"x-session-id": unknown,
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unknown token: status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Unknown session") {
		t.Errorf("body = %s, want an unknown-session message", w.Body.String())
	}

	// Malformed token on a protected route -> 401 (invalid token), not 400.
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{
		"x-session-id": "not-a-valid-token",
	})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("malformed token: status = %d, want 401", w.Code)
	}
}

// TestListInboxesRequiresSession verifies 400 without a header.
func TestListInboxesRequiresSession(t *testing.T) {
	e := newTestEnv(t)
	w := e.doRequest(http.MethodGet, "/api/inboxes", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Missing x-session-id") {
		t.Errorf("body = %s, want mention of missing header", w.Body.String())
	}
}

// TestCreateCustomInbox verifies a requested local part is created.
func TestCreateCustomInbox(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr, code := e.createInbox(t, sid, "testuser", "mail.example.com")
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if addr != "testuser@mail.example.com" {
		t.Errorf("address = %q", addr)
	}

	// The inbox must appear in the session's list.
	w := e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{"x-session-id": sid})
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d", w.Code)
	}
	var inboxes []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inboxes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(inboxes) != 1 || inboxes[0].Address != addr {
		t.Errorf("inboxes = %+v, want [%s]", inboxes, addr)
	}
}

// TestCreateRandomInbox verifies an empty local part triggers generation.
func TestCreateRandomInbox(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr1, code := e.createInbox(t, sid, "", "mail.example.com")
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	if !strings.HasSuffix(addr1, "@mail.example.com") {
		t.Errorf("address = %q, want @mail.example.com", addr1)
	}
	local1 := strings.TrimSuffix(addr1, "@mail.example.com")
	if local1 == "" || len(local1) > 64 {
		t.Errorf("generated local part = %q", local1)
	}

	// A second random inbox must differ (collision probability is negligible).
	addr2, _ := e.createInbox(t, sid, "", "mail.example.com")
	if addr2 == addr1 {
		t.Error("two random inboxes returned the same address")
	}
}

// TestCreateInboxDefaultDomain verifies omitting domain uses the first configured.
func TestCreateInboxDefaultDomain(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr, _ := e.createInbox(t, sid, "user", "")
	if !strings.HasSuffix(addr, "@mail.example.com") {
		t.Errorf("address = %q, want the default domain", addr)
	}
}

// TestCreateInboxRejectsUnknownDomain verifies a non-configured domain is 400.
func TestCreateInboxRejectsUnknownDomain(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	_, code := e.createInbox(t, sid, "user", "evil.com")
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}

// TestCreateInboxValidatesLocalPart verifies bad characters are rejected.
func TestCreateInboxValidatesLocalPart(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	bad := []string{
		"bad name",
		"user@domain.com", // contains @
		".leading",
		"trailing.",
		"a..b",
		"user!",
		"üser",
		strings.Repeat("a", 65),
	}
	for _, lp := range bad {
		_, code := e.createInbox(t, sid, lp, "mail.example.com")
		if code != http.StatusBadRequest {
			t.Errorf("localPart %q: status = %d, want 400", lp, code)
		}
	}
}

// TestCreateInboxEmptyBody verifies an empty POST body generates a random inbox.
func TestCreateInboxEmptyBody(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	w := e.doRequest(http.MethodPost, "/api/inboxes", nil, map[string]string{
		"x-session-id": sid,
		"Content-Type": "application/json",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", w.Code)
	}
	var inbox struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if inbox.Address == "" {
		t.Error("empty address")
	}
}

// TestCreateInboxMalformedJSON verifies bad JSON is rejected with a clear error.
func TestCreateInboxMalformedJSON(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	req := httptest.NewRequest(http.MethodPost, "/api/inboxes", strings.NewReader("{not json"))
	req.Header.Set("x-session-id", sid)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// TestClaimExistingInbox verifies an address that already exists can be claimed
// by a second session and its history is visible.
func TestClaimExistingInbox(t *testing.T) {
	e := newTestEnv(t)
	sid1 := e.getSession(t)
	sid2 := e.getSession(t)

	addr, _ := e.createInbox(t, sid1, "shared", "mail.example.com")

	// Deliver a message to that address.
	raw := "Subject: Shared\n\nbody"
	w := e.deliverRaw(raw, "sender@example.com", addr, map[string]string{
		"Authorization": "Bearer " + e.secret,
		"Content-Type":  "message/rfc822",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("deliver: %d", w.Code)
	}

	// Session 2 claims the same address.
	addr2, code := e.createInbox(t, sid2, "shared", "mail.example.com")
	if code != http.StatusCreated {
		t.Fatalf("claim: %d", code)
	}
	if addr2 != addr {
		t.Errorf("claimed address = %q, want %q", addr2, addr)
	}

	// Session 2 can read the history.
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr2)+"/messages", nil, map[string]string{
		"x-session-id": sid2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("messages: %d", w.Code)
	}
	var msgs []struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Shared" {
		t.Errorf("messages = %+v, want the shared message", msgs)
	}
}

// TestUnlinkPreservesMessages verifies DELETE only removes the session link.
func TestUnlinkPreservesMessages(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr, _ := e.createInbox(t, sid, "keep", "mail.example.com")
	e.deliverRaw("Subject: Keep\n\nbody", "x@y.com", addr, map[string]string{
		"Authorization": "Bearer " + e.secret,
	})

	// Unlink it.
	w := e.doRequest(http.MethodDelete, "/api/inboxes/"+urlEncode(addr), nil, map[string]string{
		"x-session-id": sid,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("unlink: %d", w.Code)
	}
	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK {
		t.Error("ok = false")
	}

	// The session no longer lists it.
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{"x-session-id": sid})
	var inboxes []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inboxes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(inboxes) != 0 {
		t.Errorf("inboxes = %+v, want empty after unlink", inboxes)
	}

	// But the inbox and message still exist in the store.
	exists, err := e.store.InboxExists(t.Context(), addr)
	if err != nil {
		t.Fatalf("InboxExists: %v", err)
	}
	if !exists {
		t.Error("inbox was deleted by unlink")
	}
	msgs, err := e.store.Messages(t.Context(), addr)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Errorf("messages = %d, want 1 (preserved)", len(msgs))
	}

	// A second session can reclaim and read the history.
	sid2 := e.getSession(t)
	e.createInbox(t, sid2, "keep", "mail.example.com")
	w = e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
		"x-session-id": sid2,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("messages: %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Subject != "Keep" {
		t.Errorf("messages = %+v", msgs)
	}
}

// TestMessagesRequireSessionLink verifies 403 for an unlinked address.
func TestMessagesRequireSessionLink(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	addr := "notlinked@mail.example.com"
	w := e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(addr)+"/messages", nil, map[string]string{
		"x-session-id": sid,
	})
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Inbox not in this session") {
		t.Errorf("body = %s", w.Body.String())
	}
}

// TestMessagesInvalidAddress verifies a malformed address is rejected with 400
// by the handler's validation.
func TestMessagesInvalidAddress(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	// Note: a raw space cannot be tested through a URL path (httptest rejects
	// it as an invalid URL before routing); whitespace in a local part is
	// covered by address.ValidateLocalPart unit tests and by POST body
	// validation in TestCreateInboxValidatesLocalPart.
	for _, bad := range []string{"not-an-email", "user@", "@domain.com", "user@nodot", "bad!name@x.com"} {
		w := e.doRequest(http.MethodGet, "/api/inboxes/"+urlEncode(bad)+"/messages", nil, map[string]string{
			"x-session-id": sid,
		})
		if w.Code != http.StatusBadRequest {
			t.Errorf("address %q: status = %d, want 400", bad, w.Code)
		}
	}
}

// TestMessagesEmptyAddressPathIsRedirected documents that an empty address
// segment produces /api/inboxes//messages, which net/http's ServeMux
// path-cleans to /api/inboxes/messages and answers with a 307 redirect. The
// request never reaches the handler, so this is routing behaviour rather than
// validation; the handler's own empty-address guard is covered by unit tests
// that call decodeAddressPath directly.
func TestMessagesEmptyAddressPathIsRedirected(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	req := httptest.NewRequest(http.MethodGet, "/api/inboxes//messages", nil)
	req.Header.Set("x-session-id", sid)
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)

	if w.Code != http.StatusMovedPermanently && w.Code != http.StatusTemporaryRedirect {
		t.Errorf("empty address segment: status = %d, want a 3xx clean-path redirect", w.Code)
	}
}

// TestHealthEndpoint verifies /healthz returns JSON and the database status.
func TestHealthEndpoint(t *testing.T) {
	e := newTestEnv(t)
	w := e.doRequest(http.MethodGet, "/healthz", nil, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Status   string `json:"status"`
		Database string `json:"database"`
		Messages int    `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != "ok" || resp.Database != "ok" {
		t.Errorf("health = %+v", resp)
	}
}

// helpers for inbound delivery -------------------------------------------------

// deliverRaw POSTs raw MIME to /api/inbound with the given headers.
func (e testEnv) deliverRaw(raw, envelopeFrom, envelopeTo string, extraHeaders map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/inbound", strings.NewReader(raw))
	req.Header.Set("X-Envelope-From", envelopeFrom)
	req.Header.Set("X-Envelope-To", envelopeTo)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)
	return w
}

// urlEncode is a minimal percent-encoder for address path segments.
func urlEncode(s string) string {
	// net/http's PathValue already decodes, so we must encode '@' to %40.
	return strings.ReplaceAll(s, "@", "%40")
}

// testConfig returns the same configuration newTestEnv uses, so a server can be
// rebuilt around an injected (failing) store without changing behaviour. The
// ListenAddr and DBPath are irrelevant here because the store is supplied
// directly rather than opened from the path.
func testConfig(e testEnv) config.Config {
	return config.Config{
		ListenAddr:      ":8080",
		DBPath:          "/unused/in/tests",
		AppName:         "Blinkmail",
		MailDomains:     []string{"mail.example.com", "mail.example.org"},
		WebHost:         "mail.example.com",
		InboundSecret:   e.secret,
		MaxInboundBytes: 1 << 20,
	}
}

// discardLogger returns a logger that drops all output, keeping test logs clean.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
