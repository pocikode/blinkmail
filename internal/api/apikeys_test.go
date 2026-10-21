package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/api"
	"pocikode/blinkmail/internal/store"
)

// listKeys fetches GET /api/keys as the logged-in browser. The store type is
// reused because its JSON tags are exactly the documented response shape; its
// Hash field is tagged json:"-" so it can never be decoded from the response.
func (e testEnv) listKeys(t *testing.T) []store.APIKey {
	t.Helper()
	w := e.doRequest(http.MethodGet, "/api/keys", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list keys: %d %s", w.Code, w.Body.String())
	}
	var out []store.APIKey
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("list keys unmarshal: %v", err)
	}
	return out
}

// TestCreateAPIKeyReturnsPlaintextOnce verifies the create response shape and
// that the plaintext never appears in the list afterwards.
func TestCreateAPIKeyReturnsPlaintextOnce(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": "  Laptop  "},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Key  string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.Name != "Laptop" {
		t.Errorf("name = %q, want the trimmed value", created.Name)
	}
	if len(created.Key) != 67 || !strings.HasPrefix(created.Key, "bm_") {
		t.Errorf("key = %q, want bm_ plus 64 hex chars", created.Key)
	}

	// The plaintext and the hash appear in this response only.
	if strings.Contains(w.Body.String(), "key_hash") {
		t.Error("create response leaked the stored hash field")
	}

	keys := e.listKeys(t)
	if len(keys) != 2 { // the seeded environment key plus this one
		t.Fatalf("keys = %d, want 2", len(keys))
	}
	for _, k := range keys {
		if k.Hash != "" {
			t.Errorf("list entry carries the key hash: %+v", k)
		}
		if k.ID != created.ID {
			continue
		}
		if k.Name != "Laptop" {
			t.Errorf("listed name = %q", k.Name)
		}
		if k.Prefix != created.Key[:12] {
			t.Errorf("prefix = %q, want %q", k.Prefix, created.Key[:12])
		}
		if k.LastUsedAt != nil {
			t.Errorf("last_used_at = %v, want null before first use", *k.LastUsedAt)
		}
	}

	// Nothing in the list body may contain the plaintext or its hash.
	body := e.doRequest(http.MethodGet, "/api/keys", nil, nil).Body.String()
	if strings.Contains(body, created.Key) {
		t.Error("list response contains the plaintext key")
	}
	if strings.Contains(body, hashForTest(created.Key)) {
		t.Error("list response contains the key hash")
	}
}

// TestCreateAPIKeyValidatesName covers the trimmed/required/max-64 rule.
func TestCreateAPIKeyValidatesName(t *testing.T) {
	e := newTestEnv(t)

	cases := map[string]string{
		"missing":  "",
		"blank":    "   ",
		"too long": strings.Repeat("a", 65),
	}
	for name, value := range cases {
		w := e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": value},
			map[string]string{"Content-Type": "application/json"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, w.Code)
		}
	}

	// Exactly 64 characters is allowed.
	w := e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": strings.Repeat("a", 64)},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusCreated {
		t.Errorf("64-char name: status = %d, want 201", w.Code)
	}
}

// TestAPIKeyAuthenticatesInboxRoutes verifies a key works without x-session-id
// and that its inboxes are scoped to the key's own synthetic session.
func TestAPIKeyAuthenticatesInboxRoutes(t *testing.T) {
	e := newTestEnv(t)

	// Create an inbox with no session header at all.
	w := e.doRequestRaw(http.MethodPost, "/api/inboxes",
		map[string]string{"localPart": "apikey", "domain": "mail.example.com"},
		map[string]string{
			"Authorization": "Bearer " + e.apiKey,
			"Content-Type":  "application/json",
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("create inbox with key: %d %s", w.Code, w.Body.String())
	}

	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Fatalf("list inboxes with key: %d %s", w.Code, w.Body.String())
	}
	var inboxes []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inboxes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(inboxes) != 1 || inboxes[0].Address != "apikey@mail.example.com" {
		t.Errorf("inboxes = %+v, want the key's inbox", inboxes)
	}

	// The browser's own session must not see it: ownership is per key.
	sid := e.getSession(t)
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{"x-session-id": sid})
	if err := json.Unmarshal(w.Body.Bytes(), &inboxes); err != nil {
		t.Fatalf("unmarshal browser inboxes: %v", err)
	}
	if len(inboxes) != 0 {
		t.Errorf("browser session sees API-key inboxes: %+v", inboxes)
	}

	// The synthetic session is stable: a second key call resumes the same inboxes
	// and the sessions row is created lazily exactly once.
	keySession := "key_" + e.apiKeyID
	exists, err := e.store.SessionExists(t.Context(), keySession)
	if err != nil {
		t.Fatalf("SessionExists: %v", err)
	}
	if !exists {
		t.Errorf("synthetic session %q was not created lazily", keySession)
	}
}

// TestAPIKeyWorksOnInbound covers the relay path: a key authenticates
// POST /api/inbound even without a login cookie.
func TestAPIKeyWorksOnInbound(t *testing.T) {
	e := newTestEnv(t)

	w := e.deliverRaw("Subject: Via key\n\nbody", "from@x.com", "relay@mail.example.com",
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Fatalf("inbound with key: %d %s", w.Code, w.Body.String())
	}

	var resp struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK {
		t.Errorf("response = %+v", resp)
	}
}

// TestAPIKeyTouchUpdatesLastUsedAt verifies a successful key auth records use.
func TestAPIKeyTouchUpdatesLastUsedAt(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	stored, err := e.store.APIKeyByHash(t.Context(), hashForTest(e.apiKey))
	if err != nil {
		t.Fatalf("APIKeyByHash: %v", err)
	}
	if stored.LastUsedAt == nil || *stored.LastUsedAt == "" {
		t.Error("last_used_at was not updated after a successful key request")
	}
}

// TestUnknownAPIKeyIsRejected verifies unknown and malformed keys are a generic
// 401 that says nothing about which part failed.
func TestUnknownAPIKeyIsRejected(t *testing.T) {
	e := newTestEnv(t)

	cases := map[string]string{
		"unknown key":   "Bearer bm_" + strings.Repeat("a", 64),
		"malformed key": "Bearer not-a-key",
		"wrong scheme":  "Basic " + e.apiKey,
		"bare key":      e.apiKey,
		"empty bearer":  "Bearer ",
		"truncated key": "Bearer bm_" + strings.Repeat("a", 63),
		"uppercase hex": "Bearer bm_" + strings.Repeat("A", 64),
	}
	for name, header := range cases {
		w := e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
			map[string]string{"Authorization": header})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "Authentication required") {
			t.Errorf("%s: body = %s", name, w.Body.String())
		}
	}
}

// TestDeletedAPIKeyStopsWorking verifies revocation takes effect immediately.
func TestDeletedAPIKeyStopsWorking(t *testing.T) {
	e := newTestEnv(t)
	key, id := e.createAPIKey(t, "temporary")

	w := e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + key})
	if w.Code != http.StatusOK {
		t.Fatalf("before delete: status = %d", w.Code)
	}

	w = e.doRequest(http.MethodDelete, "/api/keys/"+id, nil,
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("delete body = %s", w.Body.String())
	}

	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + key})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("after delete: status = %d, want 401", w.Code)
	}

	// Deleting it again is a 404, not a silent success.
	w = e.doRequest(http.MethodDelete, "/api/keys/"+id, nil,
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", w.Code)
	}
}

// TestAPIKeyCannotManageKeys is the escalation guard: a leaked key must not be
// able to mint or revoke keys.
func TestAPIKeyCannotManageKeys(t *testing.T) {
	e := newTestEnv(t)
	key, id := e.createAPIKey(t, "read-only")

	auth := map[string]string{
		"Authorization": "Bearer " + key,
		"Content-Type":  "application/json",
	}

	w := e.doRequestRaw(http.MethodGet, "/api/keys", nil, auth)
	if w.Code != http.StatusForbidden {
		t.Errorf("list with key: status = %d, want 403", w.Code)
	}

	w = e.doRequestRaw(http.MethodPost, "/api/keys", map[string]string{"name": "escalated"}, auth)
	if w.Code != http.StatusForbidden {
		t.Errorf("create with key: status = %d, want 403", w.Code)
	}
	if len(e.listKeys(t)) != 2 {
		t.Error("a key-authenticated caller created a key")
	}

	w = e.doRequestRaw(http.MethodDelete, "/api/keys/"+id, nil, auth)
	if w.Code != http.StatusForbidden {
		t.Errorf("delete with key: status = %d, want 403", w.Code)
	}
	// The key must still work after the refused delete.
	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil, auth)
	if w.Code != http.StatusOK {
		t.Errorf("key stopped working after a refused delete: %d", w.Code)
	}
}

// TestCookieStillAuthenticates verifies the browser path is unchanged, including
// that cookie callers keep the x-session-id behaviour.
func TestCookieStillAuthenticates(t *testing.T) {
	e := newTestEnv(t)

	// Cookie auth works on /api/inboxes, and still needs x-session-id: there is no
	// synthetic session for a browser.
	w := e.doRequest(http.MethodGet, "/api/inboxes", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("cookie without session header: status = %d, want 400", w.Code)
	}

	sid := e.getSession(t)
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{"x-session-id": sid})
	if w.Code != http.StatusOK {
		t.Errorf("cookie with session header: status = %d, want 200", w.Code)
	}

	// And the cookie can manage keys.
	if w := e.doRequest(http.MethodGet, "/api/keys", nil, nil); w.Code != http.StatusOK {
		t.Errorf("key list with cookie: status = %d", w.Code)
	}
}

// TestAPIKeySessionIsPerKey verifies two keys do not share inbox ownership.
func TestAPIKeySessionIsPerKey(t *testing.T) {
	e := newTestEnv(t)
	keyA, idA := e.createAPIKey(t, "a")
	keyB, idB := e.createAPIKey(t, "b")

	w := e.doRequestRaw(http.MethodPost, "/api/inboxes",
		map[string]string{"localPart": "onlya", "domain": "mail.example.com"},
		map[string]string{
			"Authorization": "Bearer " + keyA,
			"Content-Type":  "application/json",
		})
	if w.Code != http.StatusCreated {
		t.Fatalf("create with key A: %d", w.Code)
	}

	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + keyB})
	var inboxes []struct {
		Address string `json:"address"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &inboxes); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(inboxes) != 0 {
		t.Errorf("key B sees key A's inboxes: %+v", inboxes)
	}

	// Each key has its own synthetic session.
	for _, id := range []string{idA, idB} {
		exists, err := e.store.SessionExists(t.Context(), "key_"+id)
		if err != nil {
			t.Fatalf("SessionExists: %v", err)
		}
		if !exists {
			t.Errorf("session for key %s was not created", id)
		}
	}
}

// TestAPIKeyTouchFailureIsNotFatal verifies a failing last_used_at update never
// fails the request.
func TestAPIKeyTouchFailureIsNotFatal(t *testing.T) {
	e := newTestEnv(t)
	fs := &touchFailingStore{Store: e.store}
	srv := newServerWithStore(t, e, fs)

	req := newAPIKeyRequest(http.MethodGet, "/api/inboxes", e.apiKey)
	w := serve(srv, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 despite a failed touch (%s)", w.Code, w.Body.String())
	}
}

// TestGateStorageFailureOnAPIKeyLookup verifies the gate fails closed when the
// key lookup itself errors.
func TestGateStorageFailureOnAPIKeyLookup(t *testing.T) {
	env := newFailingEnv(t, func(f *failingStore) {
		f.failAPIKey = true
	})

	req := newAPIKeyRequest(http.MethodGet, "/api/inboxes", env.apiKey)
	req.AddCookie(env.cookie)
	w := serve(env.srv, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "inbox") {
		t.Errorf("body = %s, must not leak app data", w.Body.String())
	}
}

// TestAPIKeyManagementStorageFailures verifies the management routes surface
// storage errors as 500 rather than a misleading success.
func TestAPIKeyManagementStorageFailures(t *testing.T) {
	env := newFailingEnv(t, func(f *failingStore) {
		f.failAPIKey = true
	})

	w := doAuthRequest(env.srv, env.cookie, http.MethodGet, "/api/keys", "", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("list: status = %d, want 500", w.Code)
	}

	w = doAuthRequest(env.srv, env.cookie, http.MethodPost, "/api/keys",
		`{"name":"x"}`, nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("create: status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "bm_") {
		t.Errorf("create failure leaked key material: %s", w.Body.String())
	}

	w = doAuthRequest(env.srv, env.cookie, http.MethodDelete, "/api/keys/anything", "", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("delete: status = %d, want 500", w.Code)
	}
}

// hashForTest mirrors the server's storage hash so tests can look rows up
// without exposing the hashing helper across packages.
func hashForTest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// TestAPIKeyHashIsStoredNotPlaintext verifies the database holds a hash and a
// prefix, never the plaintext.
func TestAPIKeyHashIsStoredNotPlaintext(t *testing.T) {
	e := newTestEnv(t)
	key, id := e.createAPIKey(t, "hashcheck")

	var stored, prefix string
	err := e.store.DB().QueryRowContext(t.Context(),
		`SELECT key_hash, prefix FROM api_keys WHERE id = ?`, id).Scan(&stored, &prefix)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if stored == key {
		t.Error("the plaintext key was stored in key_hash")
	}
	if stored != hashForTest(key) {
		t.Errorf("key_hash = %q, want the SHA-256 of the plaintext", stored)
	}
	if len(stored) != 64 {
		t.Errorf("key_hash length = %d, want 64 hex chars", len(stored))
	}
	if prefix != key[:12] {
		t.Errorf("prefix = %q, want %q", prefix, key[:12])
	}
}

// touchFailingStore wraps the real store and always fails TouchAPIKey, proving
// a stale last_used_at never fails a request.
type touchFailingStore struct{ *store.Store }

func (touchFailingStore) TouchAPIKey(context.Context, string) error {
	return errStorage
}

// newServerWithStore builds a server around an arbitrary store implementation.
func newServerWithStore(t *testing.T, e testEnv, st api.Store) *api.Server {
	t.Helper()
	return api.New(api.Deps{Config: testConfig(e), Store: st, Logger: discardLogger()})
}

// newAPIKeyRequest builds a request authenticated by a managed API key.
func newAPIKeyRequest(method, path, key string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// serve runs a request through a server.
func serve(srv *api.Server, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

// TestAPIKeyCannotChangePassword verifies the account-takeover path is closed:
// a key that knows the current password still cannot rotate it.
func TestAPIKeyCannotChangePassword(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodPost, "/api/password",
		map[string]string{
			"currentPassword": e.password,
			"newPassword":     "an-attacker-chosen-password",
		},
		map[string]string{
			"Authorization": "Bearer " + e.apiKey,
			"Content-Type":  "application/json",
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	// The password must be unchanged: logging in with the original still works.
	e.login(t, e.password)
}

// TestAPIKeySessionRouteReturnsKeySession verifies GET /api/session is coherent
// for a key caller, so a client that does call it is not handed a token that
// would isolate it from its own inboxes.
func TestAPIKeySessionRouteReturnsKeySession(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodGet, "/api/session", nil,
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.SessionID != "key_"+e.apiKeyID {
		t.Errorf("sessionId = %q, want key_%s", resp.SessionID, e.apiKeyID)
	}
}

// TestAPIKeyLogoutDoesNotRevokeKey verifies logout is a no-op for a key caller
// rather than deleting its synthetic inbox session.
func TestAPIKeyLogoutDoesNotRevokeKey(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodPost, "/api/logout", nil,
		map[string]string{
			"Authorization": "Bearer " + e.apiKey,
			"Content-Type":  "application/json",
		})
	if w.Code != http.StatusOK {
		t.Fatalf("logout: status = %d (%s)", w.Code, w.Body.String())
	}

	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"Authorization": "Bearer " + e.apiKey})
	if w.Code != http.StatusOK {
		t.Errorf("key stopped working after logout: %d", w.Code)
	}
}

// TestCreateAPIKeyNameLengthIsRunes verifies the 64-character cap counts
// characters, not bytes, so a non-ASCII name is not rejected early.
func TestCreateAPIKeyNameLengthIsRunes(t *testing.T) {
	e := newTestEnv(t)

	// 64 multi-byte characters: 128 bytes, but 64 characters.
	name := strings.Repeat("é", 64)
	w := e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": name},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusCreated {
		t.Errorf("64-rune name: status = %d, want 201 (%s)", w.Code, w.Body.String())
	}

	w = e.doRequest(http.MethodPost, "/api/keys", map[string]string{"name": name + "é"},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusBadRequest {
		t.Errorf("65-rune name: status = %d, want 400", w.Code)
	}
}
