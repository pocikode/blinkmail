package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Limits for the external read surface.
const (
	// defaultV1Limit is what a client gets without asking: enough for a normal
	// poll burst, small enough that a response stays cheap.
	defaultV1Limit = 100

	// maxV1Limit caps one page. Without it a single request could pull a whole
	// mailbox into memory, which is a denial-of-service lever on a public API.
	maxV1Limit = 500
)

// ---- /api/v1 handlers ----

// handleV1ListInboxes lists every inbox that has mail, newest activity first.
//
// Global rather than session-scoped: this is a mail feed for an integration, and
// inboxes are public by address. Inboxes with no messages are excluded by the
// store because an empty address tells a reader nothing.
func (s *Server) handleV1ListInboxes(w http.ResponseWriter, r *http.Request) {
	inboxes, err := s.store.InboxSummaries(r.Context())
	if err != nil {
		s.log.Error("inbox summary listing failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not list inboxes")
		return
	}
	writeJSON(w, http.StatusOK, inboxes)
}

// handleV1Messages lists an inbox's messages oldest first, optionally only
// those newer than ?since=.
//
// An address that never received mail is an empty array, not a 404: inboxes are
// created lazily on first delivery, so "not found" would be the normal state for
// a fresh address and every poll would have to special-case it.
func (s *Server) handleV1Messages(w http.ResponseWriter, r *http.Request) {
	addr, status, err := decodeAddressPath(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}

	query := r.URL.Query()
	since := strings.TrimSpace(query.Get("since"))
	if since != "" {
		if _, err := time.Parse(time.RFC3339, since); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid since timestamp, want RFC 3339")
			return
		}
	}

	limit := defaultV1Limit
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "Invalid limit, want a positive integer")
			return
		}
		limit = min(n, maxV1Limit)
	}

	messages, err := s.store.MessagesSince(r.Context(), addr, since, limit)
	if err != nil {
		s.log.Error("message listing failed", "error", err, "address", addr)
		writeError(w, http.StatusInternalServerError, errReadMessages.Error())
		return
	}
	writeJSON(w, http.StatusOK, messages)
}
