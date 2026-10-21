package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"pocikode/blinkmail/internal/address"
	"pocikode/blinkmail/internal/store"
)

// randomAddressAttempts bounds how many times generation retries after the
// database reports a collision. The address space (~800k combinations) makes
// exhaustion practically impossible.
const randomAddressAttempts = 50

// Errors whose message is returned verbatim to the client in the standard
// error envelope. Internal causes are logged separately, never leaked here.
var (
	errCreateInbox  = errors.New("Could not create inbox")
	errUniqueAlloc  = errors.New("Could not allocate a unique address, please retry")
	errMissingAddr  = errors.New("Missing address")
	errReadMessages = errors.New("Could not read messages")
)

// configResponse preserves the reference JSON shape for GET /api/config.
type configResponse struct {
	AppName     string   `json:"appName"`
	MailDomain  string   `json:"mailDomain"`
	MailDomains []string `json:"mailDomains"`
}

// sessionResponse preserves the reference JSON shape for GET /api/session.
type sessionResponse struct {
	SessionID string `json:"sessionId"`
}

// createInboxRequest is the POST /api/inboxes body. Both fields are optional.
type createInboxRequest struct {
	LocalPart *string `json:"localPart"`
	Domain    *string `json:"domain"`
}

// handleConfig serves public application settings.
//
// The inbound secret is never included: this route is unauthenticated and
// public.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	domains := s.cfg.MailDomains
	// Copy so a caller cannot mutate the configured slice through the response.
	out := make([]string, len(domains))
	copy(out, domains)

	writeJSON(w, http.StatusOK, configResponse{
		AppName:     s.cfg.AppName,
		MailDomain:  s.cfg.DefaultDomain(),
		MailDomains: out,
	})
}

// handleSession creates a new anonymous session or resumes a valid existing
// one.
//
// Unlike the reference implementation, a client-supplied token is only resumed
// when it already exists in the database. Arbitrary strings are never
// registered as sessions; that would let anyone mint a token, or squat one
// another browser later receives.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if supplied, err := sessionFromRequest(r); err == nil && supplied != "" {
		exists, err := s.store.SessionExists(ctx, supplied)
		if err != nil {
			s.log.Error("session lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, "Could not validate session")
			return
		}
		if exists {
			writeJSON(w, http.StatusOK, sessionResponse{SessionID: supplied})
			return
		}
		// Fall through: an unknown token gets a fresh one.
	}

	for range 5 {
		token, err := newSessionToken()
		if err != nil {
			s.log.Error("session token generation failed", "error", err)
			writeError(w, http.StatusInternalServerError, "Could not create session")
			return
		}
		if err := s.store.CreateSession(ctx, token); err != nil {
			// Only a primary-key collision can fail here; retry with new entropy.
			continue
		}
		writeJSON(w, http.StatusOK, sessionResponse{SessionID: token})
		return
	}

	s.log.Error("could not allocate a unique session token after retries")
	writeError(w, http.StatusInternalServerError, "Could not create session")
}

// handleListInboxes returns the inboxes linked to the caller's session.
func (s *Server) handleListInboxes(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	inboxes, err := s.store.SessionInboxes(r.Context(), sessionID)
	if err != nil {
		s.log.Error("inbox listing failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not list inboxes")
		return
	}
	writeJSON(w, http.StatusOK, inboxes)
}

// handleCreateInbox creates or claims an address and links it to the session.
//
// Inboxes are public by address: claiming an address that already exists links
// the caller to the same mailbox and exposes its history. That is the intended
// behaviour of a public disposable-mail service, and is stated in the UI and
// docs rather than hidden.
func (s *Server) handleCreateInbox(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	var req createInboxRequest
	if err := decodeJSON(r, maxJSONBody, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	domain, err := s.resolveDomain(req.Domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	localPart := derefTrimLower(req.LocalPart)
	addr, status, err := s.createOrClaimAddress(r, domain, localPart)
	if err != nil {
		if status >= http.StatusInternalServerError {
			s.log.Error("inbox creation failed", "error", err, "domain", domain)
		}
		writeError(w, status, err.Error())
		return
	}

	if err := s.store.LinkInboxToSession(r.Context(), sessionID, addr); err != nil {
		s.log.Error("inbox linking failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, "Could not link inbox to session")
		return
	}

	inbox, err := s.store.GetInbox(r.Context(), addr)
	if err != nil {
		s.log.Error("inbox read-back failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, "Could not read created inbox")
		return
	}
	writeJSON(w, http.StatusCreated, inbox)
}

// createOrClaimAddress returns the address to link for a create request.
//
// A requested local part is validated and claimed idempotently (so re-claiming
// an existing public address works). An empty local part triggers random
// generation, where collisions must not be shared: the database PRIMARY KEY is
// the arbiter, so concurrent generators cannot land on the same new address.
func (s *Server) createOrClaimAddress(r *http.Request, domain, localPart string) (string, int, error) {
	ctx := r.Context()

	if localPart == "" {
		return s.createRandomAddress(ctx, domain)
	}

	if err := address.ValidateLocalPart(localPart); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("Invalid local part: %s", err)
	}
	addr := address.Join(localPart, domain)
	if err := s.store.CreateInbox(ctx, addr); err != nil {
		return "", http.StatusInternalServerError, errCreateInbox
	}
	return addr, http.StatusCreated, nil
}

// createRandomAddress generates an unused address, relying on the unique
// PRIMARY KEY to reject races. A loser retries rather than sharing a mailbox.
func (s *Server) createRandomAddress(ctx context.Context, domain string) (string, int, error) {
	for range randomAddressAttempts {
		candidate := address.Join(s.gen.RandomLocalPart(), domain)
		err := s.store.ClaimInbox(ctx, candidate)
		switch {
		case err == nil:
			return candidate, http.StatusCreated, nil
		case errors.Is(err, store.ErrAddressTaken):
			continue // lost the race or the name is taken; try again
		default:
			return "", http.StatusInternalServerError, errCreateInbox
		}
	}

	// Last resort: append an unguessable random suffix. Using the clock here
	// (as the reference does) would leak timing and could still collide.
	candidate := address.Join(s.gen.RandomLocalPart()+s.gen.RandomSuffix(8), domain)
	if err := s.store.ClaimInbox(ctx, candidate); err != nil {
		if errors.Is(err, store.ErrAddressTaken) {
			return "", http.StatusConflict, errUniqueAlloc
		}
		return "", http.StatusInternalServerError, errCreateInbox
	}
	return candidate, http.StatusCreated, nil
}

// resolveDomain picks and validates the target domain.
func (s *Server) resolveDomain(requested *string) (string, error) {
	d := derefTrimLower(requested)
	if d == "" {
		return s.cfg.DefaultDomain(), nil
	}
	if err := address.ValidateDomain(d); err != nil {
		return "", fmt.Errorf("Invalid domain: %s", err)
	}
	if !s.cfg.HasDomain(d) {
		return "", fmt.Errorf("Invalid domain: %s. Allowed: %s", d, strings.Join(s.cfg.MailDomains, ", "))
	}
	return d, nil
}

// handleDeleteInbox unlinks an address from this session only. The inbox and
// all of its messages are preserved.
func (s *Server) handleDeleteInbox(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	addr, status, err := decodeAddressPath(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}

	if err := s.store.UnlinkInboxFromSession(r.Context(), sessionID, addr); err != nil {
		s.log.Error("inbox unlink failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, "Could not remove inbox from session")
		return
	}

	// Unlinking an address this session never held is a no-op, matching the
	// reference behaviour: removal is idempotent.
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleMessages lists an inbox's messages after checking the session link.
//
// The link check is an organisation rule, not a confidentiality boundary:
// inboxes are public by address, so anyone can link an address and read it.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	sessionID, ok := s.requireSession(w, r)
	if !ok {
		return
	}

	addr, status, err := decodeAddressPath(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}

	linked, err := s.store.IsInboxInSession(r.Context(), sessionID, addr)
	if err != nil {
		s.log.Error("session link check failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, errReadMessages.Error())
		return
	}
	if !linked {
		writeError(w, http.StatusForbidden, "Inbox not in this session")
		return
	}

	messages, err := s.store.Messages(r.Context(), addr)
	if err != nil {
		s.log.Error("message listing failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, errReadMessages.Error())
		return
	}
	writeJSON(w, http.StatusOK, messages)
}

// decodeAddressPath extracts and validates the {address} path value.
func decodeAddressPath(r *http.Request) (string, int, error) {
	raw := r.PathValue("address")
	// net/http already percent-decodes path values, so no manual decoding is
	// applied. The reference decoded twice, which let a literal '%' produce a
	// different address than the one that was linked.
	addr := strings.ToLower(strings.TrimSpace(raw))
	if addr == "" {
		return "", http.StatusBadRequest, errMissingAddr
	}
	if err := address.Validate(addr); err != nil {
		return "", http.StatusBadRequest, fmt.Errorf("Invalid address: %w", err)
	}
	return addr, http.StatusOK, nil
}

// derefTrimLower normalises an optional JSON string field.
func derefTrimLower(s *string) string {
	if s == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*s))
}

// healthResponse is the /healthz payload.
type healthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
	Messages int    `json:"messages"`
}

// handleHealth reports application and database health for deployment checks.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	count, err := s.store.CountMessages(ctx)
	if err != nil {
		s.log.Error("health check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{
			Status:   "unhealthy",
			Database: "unavailable",
		})
		return
	}

	writeJSON(w, http.StatusOK, healthResponse{
		Status:   "ok",
		Database: "ok",
		Messages: count,
	})
}
