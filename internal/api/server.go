// Package api implements Blinkmail's HTTP surface: the public JSON API, the
// authenticated inbound mail endpoint, and the embedded web UI.
//
// It is built on net/http alone. Go 1.22+ method-and-wildcard routing patterns
// replace the reference implementation's Hono dependency.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"pocikode/blinkmail/internal/address"
	"pocikode/blinkmail/internal/config"
	"pocikode/blinkmail/internal/store"
)

// Session token size: 32 bytes rendered as 64 hex characters.
const (
	sessionTokenBytes = 32
	sessionTokenLen   = sessionTokenBytes * 2
)

// sessionTokenPattern matches only server-generated tokens. Client-supplied
// strings in any other shape are never registered as sessions.
var sessionTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Handler limits and timeouts.
const (
	// maxJSONBody caps request bodies on the JSON API routes.
	maxJSONBody = 8 << 10 // 8 KiB

	// apiTimeout bounds normal API work.
	apiTimeout = 30 * time.Second

	// inboundTimeout bounds MIME parsing plus the storage transaction.
	inboundTimeout = 120 * time.Second
)

// Store is the persistence surface the API depends on. Declaring it as an
// interface here (rather than importing the concrete *store.Store) lets tests
// inject failures — for example proving that a storage error on the inbound
// path surfaces as a 5xx and never as a successful acknowledgment.
//
// *store.Store satisfies this interface.
type Store interface {
	SessionExists(ctx context.Context, sessionID string) (bool, error)
	CreateSession(ctx context.Context, sessionID string) error
	CreateInbox(ctx context.Context, address string) error
	ClaimInbox(ctx context.Context, address string) error
	GetInbox(ctx context.Context, address string) (store.Inbox, error)
	SessionInboxes(ctx context.Context, sessionID string) ([]store.Inbox, error)
	LinkInboxToSession(ctx context.Context, sessionID, address string) error
	UnlinkInboxFromSession(ctx context.Context, sessionID, address string) error
	IsInboxInSession(ctx context.Context, sessionID, address string) (bool, error)
	Messages(ctx context.Context, address string) ([]store.Message, error)
	DeliverMessage(ctx context.Context, m store.NewMessage) error
	CountMessages(ctx context.Context) (int, error)

	// Authentication.
	SetPasswordHash(ctx context.Context, hash string) error
	PasswordHash(ctx context.Context) (string, error)
	CreateAuthSession(ctx context.Context, id string, expiresAt time.Time) error
	AuthSessionValid(ctx context.Context, id string) (bool, error)
	DeleteAuthSession(ctx context.Context, id string) error
	DeleteOtherAuthSessions(ctx context.Context, keepID string) error
}

// Server wires configuration, storage and routing together.
type Server struct {
	cfg          config.Config
	store        Store
	log          *slog.Logger
	gen          address.Generator
	mux          *http.ServeMux
	loginLimiter *loginLimiter
}

// Deps holds the collaborators a Server needs.
type Deps struct {
	Config config.Config
	Store  Store
	Logger *slog.Logger
	// Assets serves the embedded frontend. May be nil in tests.
	Assets http.Handler
}

// New builds a Server with all routes registered.
func New(d Deps) *Server {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}

	s := &Server{
		cfg:          d.Config,
		store:        d.Store,
		log:          log,
		gen:          address.NewGenerator(),
		mux:          http.NewServeMux(),
		loginLimiter: newLoginLimiter(),
	}
	s.routes(d.Assets)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// routes registers every endpoint.
//
// Every /api/ route sits behind the auth gate except the three that must work
// before a browser has logged in (/api/auth/status, /api/login, /api/config) and
// /api/inbound, which carries its own bearer secret from the Email Worker and
// is not a browser credential at all. /healthz stays public so deployment
// probes do not need credentials.
func (s *Server) routes(assets http.Handler) {
	mux := s.mux

	// Authentication. These decide whether the app is shown, so they cannot
	// themselves require a session.
	mux.Handle("GET /api/auth/status", s.withTimeout(apiTimeout, s.handleAuthStatus))
	mux.Handle("POST /api/login", s.withTimeout(apiTimeout, s.handleLogin))

	// Public settings.
	mux.Handle("GET /api/config", s.withTimeout(apiTimeout, s.handleConfig))

	// Authenticated: a valid login cookie is required for everything below.
	mux.Handle("POST /api/logout", s.protected(apiTimeout, s.handleLogout))
	mux.Handle("POST /api/password", s.protected(apiTimeout, s.handlePassword))

	// JSON API.
	mux.Handle("GET /api/session", s.protected(apiTimeout, s.handleSession))
	mux.Handle("GET /api/inboxes", s.protected(apiTimeout, s.handleListInboxes))
	mux.Handle("POST /api/inboxes", s.protected(apiTimeout, s.handleCreateInbox))
	mux.Handle("DELETE /api/inboxes/{address}", s.protected(apiTimeout, s.handleDeleteInbox))
	mux.Handle("GET /api/inboxes/{address}/messages", s.protected(apiTimeout, s.handleMessages))

	// Authenticated inbound mail from the Email Worker. It has its own bearer
	// secret and is deliberately outside the cookie gate: the relay is not a
	// browser and never holds a login session.
	mux.Handle("POST /api/inbound", s.withTimeout(inboundTimeout, s.handleInbound))

	// Deployment health check.
	mux.Handle("GET /healthz", s.withTimeout(10*time.Second, s.handleHealth))

	// Embedded frontend.
	if assets != nil {
		mux.Handle("/", assets)
	} else {
		mux.Handle("/", http.NotFoundHandler())
	}
}

// protected wraps a handler with the authentication gate and the per-request
// deadline. Order matters: the timeout wrapper is outside, so a panic inside
// the gate is recovered and logged like any other handler failure.
func (s *Server) protected(d time.Duration, next http.HandlerFunc) http.Handler {
	return s.withTimeout(d, s.requireAuth(next))
}

// withTimeout binds a per-request deadline and recovers from panics so one bad
// request cannot take the process down.
func (s *Server) withTimeout(d time.Duration, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if p := recover(); p != nil {
				s.log.Error("panic in request handler",
					"panic", p,
					"method", r.Method,
					"path", r.URL.Path,
				)
				if !rec.wroteHeader {
					writeError(rec, http.StatusInternalServerError, "Internal server error")
				}
			}
			s.log.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}()

		next(rec, r.WithContext(ctx))
	})
}

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// ---- Response helpers ----

// apiError is the reference-compatible error envelope.
type apiError struct {
	Error string `json:"error"`
}

// writeJSON encodes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Marshalling these small structs cannot realistically fail; if it did,
		// there is no meaningful response left to send.
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError sends the standard error envelope.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

// ---- Sessions ----

// newSessionToken returns a cryptographically secure session token.
func newSessionToken() (string, error) {
	b := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validSessionToken reports whether s has the shape of a server-issued token.
func validSessionToken(s string) bool {
	return sessionTokenPattern.MatchString(s)
}

// sessionFromRequest extracts and validates the x-session-id header. It returns
// an error only for a syntactically impossible token; unknown-but-well-formed
// tokens are the caller's decision.
func sessionFromRequest(r *http.Request) (string, error) {
	raw := strings.TrimSpace(r.Header.Get("x-session-id"))
	if raw == "" {
		return "", nil
	}
	if len(raw) != sessionTokenLen || !validSessionToken(raw) {
		return "", fmt.Errorf("session token is malformed")
	}
	return raw, nil
}

// requireSession resolves the caller's session. It writes the error response
// and returns false when the session is missing, malformed or unknown.
func (s *Server) requireSession(w http.ResponseWriter, r *http.Request) (string, bool) {
	token, err := sessionFromRequest(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Invalid session token")
		return "", false
	}
	if token == "" {
		writeError(w, http.StatusBadRequest, "Missing x-session-id header")
		return "", false
	}

	ctx := r.Context()
	ok, err := s.store.SessionExists(ctx, token)
	if err != nil {
		s.log.Error("session lookup failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not validate session")
		return "", false
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "Unknown session, request a new one from /api/session")
		return "", false
	}
	return token, true
}

// ---- Inbound authentication ----

// authorized reports whether the request carries the configured bearer secret.
// Comparison is constant-time to avoid leaking the secret byte by byte.
func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.InboundSecret)) == 1
}

// ---- Shared helpers ----

// decodeJSON reads a small JSON object body. An empty body decodes to an empty
// object so callers may omit optional fields entirely; malformed JSON is
// rejected rather than silently ignored.
func decodeJSON(r *http.Request, limit int64, out any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}
	if int64(len(body)) > limit {
		return errors.New("request body too large")
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}

	// Unknown keys are tolerated so future clients can send extra fields;
	// malformed JSON is rejected instead of being silently ignored.
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	// Reject trailing content after the first JSON value.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body: unexpected trailing data")
	}
	return nil
}
