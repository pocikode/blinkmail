package api_test

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authStatus fetches /api/auth/status carrying the environment's login cookie,
// which is what the frontend's boot check does.
func (e testEnv) authStatus(t *testing.T) bool {
	t.Helper()
	w := e.doRequestRaw(http.MethodGet, "/api/auth/status", nil, nil, e.cookie)
	if w.Code != http.StatusOK {
		t.Fatalf("auth/status: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("auth/status unmarshal: %v", err)
	}
	return resp.Authenticated
}

// findCookie returns the named cookie from a response, or nil.
func findCookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestGateRejectsUnauthenticatedAPI verifies every protected route answers 401
// with the documented message when no cookie is sent.
func TestGateRejectsUnauthenticatedAPI(t *testing.T) {
	e := newTestEnv(t)

	protected := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/session"},
		{http.MethodGet, "/api/inboxes"},
		{http.MethodPost, "/api/inboxes"},
		{http.MethodGet, "/api/inboxes/a%40mail.example.com/messages"},
		{http.MethodDelete, "/api/inboxes/a%40mail.example.com"},
		{http.MethodPost, "/api/logout"},
		{http.MethodPost, "/api/password"},
		{http.MethodGet, "/api/keys"},
		{http.MethodPost, "/api/keys"},
		{http.MethodDelete, "/api/keys/someid"},
		{http.MethodPost, "/api/inbound"},
	}
	for _, route := range protected {
		w := e.doRequestRaw(route.method, route.path, nil,
			map[string]string{"Content-Type": "application/json"})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", route.method, route.path, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), "Authentication required") {
			t.Errorf("%s %s: body = %s", route.method, route.path, w.Body.String())
		}
	}
}

// TestPublicRoutesNeedNoAuth verifies the routes the login screen depends on
// stay reachable without a cookie.
func TestPublicRoutesNeedNoAuth(t *testing.T) {
	e := newTestEnv(t)

	for _, path := range []string{"/api/auth/status", "/api/config", "/healthz"} {
		w := e.doRequestRaw(http.MethodGet, path, nil, nil)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", path, w.Code)
		}
	}
}

// TestAuthStatusReflectsCookie verifies the status endpoint reports the real
// gate decision, which is what the frontend boots on.
func TestAuthStatusReflectsCookie(t *testing.T) {
	e := newTestEnv(t)

	// No cookie at all.
	w := e.doRequestRaw(http.MethodGet, "/api/auth/status", nil, nil)
	var anon struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &anon); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if anon.Authenticated {
		t.Error("auth/status reported authenticated without a cookie")
	}

	if !e.authStatus(t) {
		t.Error("auth/status reported unauthenticated with a valid cookie")
	}

	// A cookie with the right shape but no session row is not a credential.
	bogus := &http.Cookie{Name: "blinkmail_auth", Value: strings.Repeat("f", 64)}
	w = e.doRequestRaw(http.MethodGet, "/api/auth/status", nil, nil, bogus)
	var resp struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Authenticated {
		t.Error("auth/status accepted an unknown session token")
	}
}

// TestLoginWrongPassword verifies the failure response is a 401 with the
// documented message and no cookie.
func TestLoginWrongPassword(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodPost, "/api/login",
		map[string]string{"password": "not-the-password"},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid password") {
		t.Errorf("body = %s, want Invalid password", w.Body.String())
	}
	if c := findCookie(w, "blinkmail_auth"); c != nil {
		t.Errorf("failed login set a cookie: %+v", c)
	}
}

// TestLoginCorrectPassword verifies the success response and the cookie's
// attributes: the security properties live in the attributes, not the value.
func TestLoginCorrectPassword(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequestRaw(http.MethodPost, "/api/login",
		map[string]string{"password": e.password},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Errorf("body = %s, want ok:true", w.Body.String())
	}

	c := findCookie(w, "blinkmail_auth")
	if c == nil {
		t.Fatal("login set no blinkmail_auth cookie")
	}
	if len(c.Value) != 64 {
		t.Errorf("cookie value length = %d, want 64 hex chars", len(c.Value))
	}
	if c.Path != "/" {
		t.Errorf("cookie Path = %q, want /", c.Path)
	}
	if !c.HttpOnly {
		t.Error("cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}
	if c.MaxAge != 2592000 {
		t.Errorf("cookie MaxAge = %d, want 2592000", c.MaxAge)
	}
	// httptest requests are plain HTTP, so Secure must be off; the TLS case is
	// covered by TestLoginCookieIsSecureOverTLS.
	if c.Secure {
		t.Error("cookie is Secure on a non-TLS request")
	}

	// The cookie must actually work.
	sid := e.getSession(t)
	w = e.doRequest(http.MethodGet, "/api/inboxes", nil, map[string]string{"x-session-id": sid})
	if w.Code != http.StatusOK {
		t.Errorf("authenticated request: status = %d, want 200", w.Code)
	}
}

// TestLoginCookieIsSecureOverTLS verifies Secure follows the request scheme.
func TestLoginCookieIsSecureOverTLS(t *testing.T) {
	e := newTestEnv(t)

	req := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"`+e.password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{} // any non-nil state means the request is TLS
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	c := findCookie(w, "blinkmail_auth")
	if c == nil {
		t.Fatal("login set no cookie")
	}
	if !c.Secure {
		t.Error("cookie is not Secure over TLS")
	}
}

// TestLoginRateLimit verifies that after five failures the sixth attempt is
// rejected with 429 — even when it carries the right password, so the limit
// cannot be probed around.
func TestLoginRateLimit(t *testing.T) {
	e := newTestEnv(t)

	for i := range 5 {
		w := e.doRequestRaw(http.MethodPost, "/api/login",
			map[string]string{"password": "wrong-password"},
			map[string]string{"Content-Type": "application/json"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, w.Code)
		}
	}

	w := e.doRequestRaw(http.MethodPost, "/api/login",
		map[string]string{"password": e.password},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("after the limit: status = %d, want 429 (%s)", w.Code, w.Body.String())
	}
	if c := findCookie(w, "blinkmail_auth"); c != nil {
		t.Error("rate-limited login handed out a cookie")
	}
}

// TestPostRequiresJSONContentType verifies the CSRF defence: a state-changing
// request without an application/json content type is refused with 415.
func TestPostRequiresJSONContentType(t *testing.T) {
	e := newTestEnv(t)
	sid := e.getSession(t)

	cases := []struct {
		name        string
		contentType string
	}{
		{"form", "application/x-www-form-urlencoded"},
		{"text", "text/plain"},
		{"missing", ""},
		{"multipart", "multipart/form-data; boundary=x"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/inboxes", strings.NewReader("localPart=x"))
		req.AddCookie(e.cookie)
		req.Header.Set("x-session-id", sid)
		if tc.contentType != "" {
			req.Header.Set("Content-Type", tc.contentType)
		}
		w := httptest.NewRecorder()
		e.srv.ServeHTTP(w, req)

		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status = %d, want 415", tc.name, w.Code)
		}
	}

	// The JSON content type, with parameters, is accepted.
	w := e.doRequest(http.MethodPost, "/api/inboxes", nil, map[string]string{
		"x-session-id": sid,
		"Content-Type": "application/json; charset=utf-8",
	})
	if w.Code != http.StatusCreated {
		t.Errorf("json content type: status = %d, want 201", w.Code)
	}

	// Login enforces the same rule.
	w = e.doRequestRaw(http.MethodPost, "/api/login",
		map[string]string{"password": e.password},
		map[string]string{"Content-Type": "text/plain"})
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("login without JSON content type: status = %d, want 415", w.Code)
	}
}

// TestLogoutInvalidatesSession verifies logout deletes the row, clears the
// cookie, and that a second logout without a session is still a 401 rather than
// a crash.
func TestLogoutInvalidatesSession(t *testing.T) {
	e := newTestEnv(t)

	w := e.doRequest(http.MethodPost, "/api/logout", nil, map[string]string{
		"Content-Type": "application/json",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	c := findCookie(w, "blinkmail_auth")
	if c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("logout did not clear the cookie: %+v", c)
	}

	// The old cookie no longer authenticates.
	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil, nil, e.cookie)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("request after logout: status = %d, want 401", w.Code)
	}
	if e.authStatus(t) {
		t.Error("auth/status still reports authenticated after logout")
	}

	// Logging out again with the now-invalid cookie is a 401, not a 500.
	w = e.doRequestRaw(http.MethodPost, "/api/logout", nil,
		map[string]string{"Content-Type": "application/json"}, e.cookie)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("second logout: status = %d, want 401", w.Code)
	}
}

// TestPasswordChange verifies the change is applied, the current password is
// required, the new minimum is enforced, and other sessions are revoked while
// the caller's survives.
func TestPasswordChange(t *testing.T) {
	e := newTestEnv(t)

	// A second browser logged in with the same password.
	other := e.login(t, e.password)

	// Wrong current password is rejected and changes nothing.
	w := e.doRequest(http.MethodPost, "/api/password", map[string]string{
		"currentPassword": "wrong-password",
		"newPassword":     "brand-new-password",
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password: status = %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid current password") {
		t.Errorf("body = %s", w.Body.String())
	}

	// Too-short new password is a 400.
	w = e.doRequest(http.MethodPost, "/api/password", map[string]string{
		"currentPassword": e.password,
		"newPassword":     "short",
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("short new password: status = %d, want 400", w.Code)
	}

	// The real change.
	const newPassword = "brand-new-password"
	w = e.doRequest(http.MethodPost, "/api/password", map[string]string{
		"currentPassword": e.password,
		"newPassword":     newPassword,
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("password change: status = %d, want 200 (%s)", w.Code, w.Body.String())
	}

	// The caller's own session still works...
	sid := e.getSession(t)
	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil,
		map[string]string{"x-session-id": sid}, e.cookie)
	if w.Code != http.StatusOK {
		t.Errorf("caller session after change: status = %d, want 200", w.Code)
	}
	// ...but every other session was revoked.
	w = e.doRequestRaw(http.MethodGet, "/api/inboxes", nil, nil, other)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("other session after change: status = %d, want 401", w.Code)
	}

	// The old password no longer logs in; the new one does.
	w = e.doRequestRaw(http.MethodPost, "/api/login",
		map[string]string{"password": e.password},
		map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("login with old password: status = %d, want 401", w.Code)
	}
	fresh := e.login(t, newPassword)
	if fresh == nil {
		t.Error("could not log in with the new password")
	}
}

// TestPasswordChangeStoresAHashedValue verifies the plaintext never reaches the
// database.
func TestPasswordChangeStoresAHashedValue(t *testing.T) {
	e := newTestEnv(t)
	const newPassword = "another-long-password"

	w := e.doRequest(http.MethodPost, "/api/password", map[string]string{
		"currentPassword": e.password,
		"newPassword":     newPassword,
	}, map[string]string{"Content-Type": "application/json"})
	if w.Code != http.StatusOK {
		t.Fatalf("password change: %d %s", w.Code, w.Body.String())
	}

	hash, err := e.store.PasswordHash(t.Context())
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if strings.Contains(hash, newPassword) {
		t.Error("stored hash contains the plaintext password")
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$600000$") {
		t.Errorf("stored hash = %q, want the pbkdf2-sha256 encoding", hash)
	}
}

// TestLoginSuccessResetsRateLimit verifies a correct password clears the
// failure count, so a user who mistyped a few times is not one attempt away
// from being locked out for the rest of the window.
func TestLoginSuccessResetsRateLimit(t *testing.T) {
	e := newTestEnv(t)

	for range 4 {
		w := e.doRequestRaw(http.MethodPost, "/api/login",
			map[string]string{"password": "wrong-password"},
			map[string]string{"Content-Type": "application/json"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", w.Code)
		}
	}

	if c := e.login(t, e.password); c == nil {
		t.Fatal("correct login after failures was refused")
	}

	// The counter is back to zero: five more failures are needed to hit the
	// limit, so the first four must still be plain 401s.
	for i := range 4 {
		w := e.doRequestRaw(http.MethodPost, "/api/login",
			map[string]string{"password": "wrong-password"},
			map[string]string{"Content-Type": "application/json"})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d after reset: status = %d, want 401", i+1, w.Code)
		}
	}
}
