package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"sync"
	"time"

	"pocikode/blinkmail/internal/auth"
	"pocikode/blinkmail/internal/store"
)

// Authentication cookie and session lifetime.
const (
	// authCookieName is deliberately specific to Blinkmail so it cannot clash
	// with another application on the same origin.
	authCookieName = "blinkmail_auth"

	// authSessionTTL is both the cookie's Max-Age and the auth_sessions row
	// expiry, so the browser and the server forget a session at the same time.
	authSessionTTL = 30 * 24 * time.Hour
	authCookieAge  = int(authSessionTTL / time.Second) // 2592000
)

// Login rate limiting. This is per-process and resets on restart: it exists to
// blunt online guessing against a single password, not as a durable audit
// trail. A restart (or a second replica) starts with a clean slate, which is an
// accepted trade for keeping the state in memory and dependency-free.
const (
	loginMaxFailures = 5
	loginWindow      = 15 * time.Minute
	// loginMaxEntries bounds memory: one small struct per attacking IP. Beyond
	// this, expired entries are pruned and then the map is trimmed back, so a
	// flood of source addresses cannot grow it without limit.
	loginMaxEntries = 10_000
)

// principal is the authenticated caller. Today the only credential is the
// session cookie, so its ID is all there is; T04 extends this with API-key
// callers, which is why the gate hands handlers a principal rather than a bare
// bool.
type principal struct {
	sessionID string
}

type principalKey struct{}

// withPrincipal stores p on the request context for the handler.
func withPrincipal(ctx context.Context, p principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalFrom returns the principal the gate attached to r.
func principalFrom(r *http.Request) (principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(principal)
	return p, ok
}

// ---- Gate ----

// authenticate resolves the caller's credential into a principal.
//
// This is the single place the gate consults credentials. T04 slots the API-key
// check in here: try the Authorization header, return a principal for a valid
// key, and only then fall back to the cookie. Handlers never need to know how
// the caller authenticated.
func (s *Server) authenticate(r *http.Request) (principal, bool, error) {
	cookie, err := r.Cookie(authCookieName)
	if err != nil || cookie.Value == "" {
		return principal{}, false, nil
	}
	// Shape-check before touching the database: the cookie is attacker-supplied,
	// and only server-issued tokens are worth a query.
	if !validSessionToken(cookie.Value) {
		return principal{}, false, nil
	}

	valid, err := s.store.AuthSessionValid(r.Context(), cookie.Value)
	if err != nil {
		return principal{}, false, fmt.Errorf("validate auth session: %w", err)
	}
	if !valid {
		return principal{}, false, nil
	}
	return principal{sessionID: cookie.Value}, true, nil
}

// requireAuth is the gate in front of every non-public /api/ route.
//
// It also enforces the CSRF defence for state-changing methods: a request whose
// Content-Type is not application/json is rejected with 415. A browser lets a
// cross-site form or <img> send a POST without a CORS preflight, but it cannot
// set Content-Type to application/json without one — so requiring that header
// blocks forged state changes. SameSite=Lax on the cookie is the second half:
// it keeps the cookie off cross-site requests in the first place. Neither
// measure alone covers every browser, and together they do.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireJSONContentType(w, r) {
			return
		}

		p, ok, err := s.authenticate(r)
		if err != nil {
			s.log.Error("auth session check failed", "error", err)
			writeError(w, http.StatusInternalServerError, "Could not validate authentication")
			return
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return
		}

		next(w, r.WithContext(withPrincipal(r.Context(), p)))
	}
}

// requireJSONContentType enforces the CSRF rule for methods that change state.
// Safe methods carry no state change, so they need no header.
func requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	// ParseMediaType ignores parameters such as charset and tolerates casing,
	// so "Application/JSON; charset=utf-8" is accepted while a form or text
	// body is not.
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	return true
}

// ---- Cookies ----

// setAuthCookie issues the session cookie for token.
//
// Secure is set only when the request itself is TLS. A reverse proxy that
// terminates TLS and forwards plain HTTP will therefore produce a non-Secure
// cookie; the README tells operators to either forward the original scheme or
// accept that the cookie travels over the loopback hop.
func setAuthCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true, // unreadable from JavaScript, so XSS cannot exfiltrate it
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   authCookieAge,
	})
}

// clearAuthCookie expires the cookie in the browser. The attributes must match
// the ones it was set with, or the browser keeps the original cookie.
func clearAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// ---- Rate limiter ----

// loginLimiter tracks failed logins per client IP in a fixed window.
//
// In-memory and mutex-protected: the deployment is a single process, and a
// database table would add write load for state that is allowed to be forgotten
// on restart.
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*loginAttempt
	now      func() time.Time // injectable for tests
}

// loginAttempt is one IP's failure count and the end of its window.
type loginAttempt struct {
	failures int
	until    time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]*loginAttempt), now: time.Now}
}

// blocked reports whether ip has exhausted its attempts in the current window.
func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	a, ok := l.attempts[ip]
	if !ok {
		return false
	}
	if !l.now().Before(a.until) {
		// Window over: the entry carries no information any more.
		delete(l.attempts, ip)
		return false
	}
	return a.failures >= loginMaxFailures
}

// fail records one failed attempt, starting a window if there is none.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if a, ok := l.attempts[ip]; ok && now.Before(a.until) {
		a.failures++
		return
	}

	l.makeRoomLocked(now)
	l.attempts[ip] = &loginAttempt{failures: 1, until: now.Add(loginWindow)}
}

// reset forgets an IP's failures after a successful login, so a legitimate user
// who mistyped a few times is not left half-way to the limit.
func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, ip)
}

// makeRoomLocked keeps the map bounded: it drops expired entries, and if the
// map is still full evicts an arbitrary one. Eviction is deliberately blunt;
// an attacker holding 10k source addresses has already defeated per-IP
// limiting, so spending more effort here would buy nothing.
func (l *loginLimiter) makeRoomLocked(now time.Time) {
	if len(l.attempts) < loginMaxEntries {
		return
	}
	for ip, a := range l.attempts {
		if !now.Before(a.until) {
			delete(l.attempts, ip)
		}
	}
	for ip := range l.attempts {
		if len(l.attempts) < loginMaxEntries {
			break
		}
		delete(l.attempts, ip)
	}
}

// clientIP identifies the caller for rate limiting.
//
// Only the transport peer address is trusted. X-Forwarded-For is deliberately
// ignored: without a configured trusted proxy any client could set it and
// bypass the limit entirely. Behind a reverse proxy every login therefore
// shares the proxy's address, which throttles all clients together — safe, if
// blunt.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- Handlers ----

// authStatusResponse is the GET /api/auth/status payload.
type authStatusResponse struct {
	Authenticated bool `json:"authenticated"`
}

// handleAuthStatus tells the frontend whether to show the login view or the
// app. Public: it reveals only whether this browser holds a valid cookie.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	_, ok, err := s.authenticate(r)
	if err != nil {
		s.log.Error("auth status check failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not validate authentication")
		return
	}
	writeJSON(w, http.StatusOK, authStatusResponse{Authenticated: ok})
}

// loginRequest is the POST /api/login body. Password only: there is one admin
// account and no username to guess.
type loginRequest struct {
	Password string `json:"password"`
}

// handleLogin verifies the admin password and starts a login session.
//
// Only failures are counted, so a successful login is never blocked by earlier
// attempts from the same address.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if !requireJSONContentType(w, r) {
		return
	}

	ip := clientIP(r)
	if s.loginLimiter.blocked(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many failed login attempts, try again later")
		return
	}

	var req loginRequest
	if err := decodeJSON(r, maxJSONBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	hash, err := s.store.PasswordHash(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Bootstrap seeds a password before serving, so this means the row was
		// removed underneath a running server. Fail loudly rather than accept
		// any password.
		s.log.Error("admin password is not set")
		writeError(w, http.StatusInternalServerError, "Admin password is not configured")
		return
	case err != nil:
		s.log.Error("password hash read failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not verify password")
		return
	}

	match, err := auth.Verify(hash, req.Password)
	if err != nil {
		// A malformed stored hash is a server problem, not a wrong password.
		s.log.Error("stored password hash is invalid", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not verify password")
		return
	}
	if !match {
		s.loginLimiter.fail(ip)
		writeError(w, http.StatusUnauthorized, "Invalid password")
		return
	}

	token, err := newSessionToken()
	if err != nil {
		s.log.Error("auth session token generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}
	if err := s.store.CreateAuthSession(ctx, token, store.NowUTC().Add(authSessionTTL)); err != nil {
		s.log.Error("auth session creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not create session")
		return
	}

	s.loginLimiter.reset(ip)
	setAuthCookie(w, r, token)
	s.log.Info("login succeeded", "ip", ip)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout revokes the caller's login session and clears the cookie.
//
// Idempotent: deleting an already-gone row is a no-op, so a repeated logout
// still returns 200.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r)
	if !ok {
		// Unreachable behind the gate; fail closed rather than silently.
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	if err := s.store.DeleteAuthSession(r.Context(), p.sessionID); err != nil {
		s.log.Error("auth session deletion failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not log out")
		return
	}

	clearAuthCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// changePasswordRequest is the POST /api/password body.
type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handlePassword changes the admin password and revokes every other session.
//
// The current password is required even though the caller is already
// authenticated: that is the difference between a stolen cookie being able to
// read mail and being able to take the account over. Other sessions are dropped
// so a password change actually locks out whoever knew the old one; the
// caller's own session survives so the UI does not have to log in again.
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	p, ok := principalFrom(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return
	}

	var req changePasswordRequest
	if err := decodeJSON(r, maxJSONBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, "New password must be at least 8 characters")
		return
	}

	hash, err := s.store.PasswordHash(ctx)
	if err != nil {
		s.log.Error("password hash read failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not verify password")
		return
	}

	match, err := auth.Verify(hash, req.CurrentPassword)
	if err != nil {
		s.log.Error("stored password hash is invalid", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not verify password")
		return
	}
	if !match {
		writeError(w, http.StatusUnauthorized, "Invalid current password")
		return
	}

	newHash, err := auth.Hash(req.NewPassword)
	if err != nil {
		s.log.Error("password hashing failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not change password")
		return
	}
	if err := s.store.SetPasswordHash(ctx, newHash); err != nil {
		s.log.Error("password hash update failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not change password")
		return
	}
	if err := s.store.DeleteOtherAuthSessions(ctx, p.sessionID); err != nil {
		// The password is already changed; report the failure so the operator
		// knows old sessions may still be alive.
		s.log.Error("could not revoke other auth sessions", "error", err)
		writeError(w, http.StatusInternalServerError, "Password changed but other sessions could not be revoked")
		return
	}

	s.log.Info("admin password changed, other sessions revoked")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
