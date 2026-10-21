package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"pocikode/blinkmail/internal/store"
)

// API key format: "bm_" plus 32 random bytes as hex (67 characters). A 256-bit
// random key needs no KDF — there is no low-entropy secret to stretch — so only
// a plain SHA-256 is stored, and only so the plaintext never sits in the
// database. prefix is the first 12 characters, kept for display.
const (
	apiKeyPrefix    = "bm_"
	apiKeyBytes     = 32
	apiKeyLen       = len(apiKeyPrefix) + apiKeyBytes*2 // 67
	apiKeyDisplayed = 12                                // "bm_" + 9 hex chars
	maxAPIKeyName   = 64
)

// apiKeyPattern matches only server-generated keys. Anything else is rejected
// before it reaches a database lookup.
var apiKeyPattern = regexp.MustCompile(`^bm_[0-9a-f]{64}$`)

// apiKeySessionPrefix namespaces synthetic inbox sessions so one can never
// collide with a browser session token (64 hex characters).
const apiKeySessionPrefix = "key_"

// ---- Key helpers ----

// validAPIKey reports whether s has the shape of a server-issued API key. The
// length check is redundant with the pattern but makes the expected size
// explicit and short-circuits the common case of a truncated paste.
func validAPIKey(s string) bool {
	return len(s) == apiKeyLen && apiKeyPattern.MatchString(s)
}

// newAPIKey returns a fresh plaintext key and its stored hash.
func newAPIKey() (plaintext, hash string, err error) {
	b := make([]byte, apiKeyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate API key: %w", err)
	}
	plaintext = apiKeyPrefix + hex.EncodeToString(b)
	return plaintext, hashAPIKey(plaintext), nil
}

// hashAPIKey returns the lowercase hex SHA-256 of a full plaintext key.
func hashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// apiKeyDisplayPrefix is the stored display prefix: the first 12 characters.
func apiKeyDisplayPrefix(plaintext string) string {
	if len(plaintext) < apiKeyDisplayed {
		return plaintext
	}
	return plaintext[:apiKeyDisplayed]
}

// apiKeySessionID is the stable synthetic inbox session for a key. Deriving it
// from the key id (rather than a random token) keeps a client's inboxes stable
// across restarts without the client storing anything.
func apiKeySessionID(keyID string) string { return apiKeySessionPrefix + keyID }

// newAPIKeyID returns the opaque identifier stored in api_keys.id. It is bare
// random hex because apiKeySessionID prefixes it with "key_".
func newAPIKeyID() string { return randomHex(16) }

// ---- Handlers ----

// apiKeyResponse is one entry of GET /api/keys. It deliberately has no key
// field: the plaintext is only ever returned by the create response.
type apiKeyResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// createAPIKeyRequest is the POST /api/keys body.
type createAPIKeyRequest struct {
	Name string `json:"name"`
}

// createAPIKeyResponse is the only response that ever carries the plaintext.
type createAPIKeyResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

// requireCookieOnly refuses key-authenticated callers on key management routes.
//
// Without this a leaked key could mint a fresh key (or delete one) and escalate
// from "can read mail" to "owns the account". The refusal is 403 rather than
// 401 so an API client can tell "wrong credential" from "right credential,
// wrong kind".
func requireCookieOnly(w http.ResponseWriter, r *http.Request) bool {
	p, ok := principalFrom(r)
	if !ok || !p.allowsManagement() {
		writeError(w, http.StatusForbidden, "API key management requires a browser login")
		return false
	}
	return true
}

// handleListAPIKeys returns every key's metadata, never its secret.
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !requireCookieOnly(w, r) {
		return
	}

	keys, err := s.store.APIKeys(r.Context())
	if err != nil {
		s.log.Error("API key listing failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not list API keys")
		return
	}

	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyResponse{
			ID:         k.ID,
			Name:       k.Name,
			Prefix:     k.Prefix,
			CreatedAt:  k.CreatedAt,
			LastUsedAt: k.LastUsedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleCreateAPIKey mints a key and returns the plaintext exactly once.
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireCookieOnly(w, r) {
		return
	}

	var req createAPIKeyRequest
	if err := decodeJSON(r, maxJSONBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	name := strings.TrimSpace(req.Name)
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	case utf8.RuneCountInString(name) > maxAPIKeyName:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Name must be at most %d characters", maxAPIKeyName))
		return
	}

	plaintext, hash, err := newAPIKey()
	if err != nil {
		s.log.Error("API key generation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not create API key")
		return
	}

	id := newAPIKeyID()
	if err := s.store.CreateAPIKey(r.Context(), id, name, apiKeyDisplayPrefix(plaintext), hash); err != nil {
		// Never log the plaintext or the hash, even on failure.
		s.log.Error("API key creation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not create API key")
		return
	}

	// Only the id is logged: never the plaintext, and never the hash.
	s.log.Info("API key created", "key_id", id)
	// The plaintext appears here and nowhere else, ever again.
	writeJSON(w, http.StatusCreated, createAPIKeyResponse{ID: id, Name: name, Key: plaintext})
}

// handleDeleteAPIKey revokes a key. The key's synthetic session is left alone:
// deleting the link rows would be unrecoverable if a delete were accidental,
// and a revoked key can no longer authenticate to use them.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if !requireCookieOnly(w, r) {
		return
	}

	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing API key id")
		return
	}

	err := s.store.DeleteAPIKey(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "API key not found")
		return
	case err != nil:
		s.log.Error("API key deletion failed", "error", err, "key_id", id)
		writeError(w, http.StatusInternalServerError, "Could not delete API key")
		return
	}

	s.log.Info("API key revoked", "key_id", id)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
