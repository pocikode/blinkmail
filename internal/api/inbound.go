package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"pocikode/blinkmail/internal/address"
	"pocikode/blinkmail/internal/mail"
	"pocikode/blinkmail/internal/store"
)

// Envelope metadata headers set by the Email Worker.
const (
	headerEnvelopeFrom = "X-Envelope-From"
	headerEnvelopeTo   = "X-Envelope-To"
)

// unknownSender is stored when neither the envelope nor the MIME From header
// yields a sender. The column is NOT NULL, so a placeholder is required.
const unknownSender = "unknown"

// inboundResponse acknowledges a stored delivery.
type inboundResponse struct {
	OK        bool   `json:"ok"`
	MessageID string `json:"messageId"`
	Duplicate bool   `json:"duplicate"`
}

// handleInbound receives raw MIME from the Email Worker.
//
// The caller has already been authenticated by the gate (an API key, not the
// login cookie). Envelope validation, the size limit and storage all complete
// before any success is acknowledged. Committing first is what makes the
// response truthful: the reference implementation logged failures and still let
// Cloudflare treat the message as delivered, silently losing mail.
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	envelopeFrom := strings.TrimSpace(r.Header.Get(headerEnvelopeFrom))
	envelopeTo := strings.TrimSpace(r.Header.Get(headerEnvelopeTo))

	// The envelope recipient drives routing, never the MIME To header: that one
	// is attacker-controlled and need not match the mailbox actually addressed.
	toLocal, toDomain, err := validateEnvelopeTo(envelopeTo, s.cfg.MailDomains)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	raw, oversized, err := readRaw(r.Body, s.cfg.MaxInboundBytes)
	if err != nil {
		s.log.Error("inbound read failed", "error", err)
		writeError(w, http.StatusBadRequest, "Could not read message body")
		return
	}
	if oversized {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"Message exceeds the %d byte limit", s.cfg.MaxInboundBytes))
		return
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "Message body is empty")
		return
	}

	parsed, err := mail.Parse(raw)
	if err != nil {
		// An unreadable message is a delivery failure, not a success. 422 lets
		// the Worker reject rather than acknowledge lost mail.
		s.log.Error("MIME parse failed", "error", err)
		writeError(w, http.StatusUnprocessableEntity, "Could not parse message")
		return
	}

	sender, err := resolveSender(envelopeFrom, parsed.From)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	inbox := address.Join(toLocal, toDomain)
	msg := store.NewMessage{
		ID:          newMessageID(),
		Inbox:       inbox,
		From:        sender,
		Subject:     parsed.Subject,
		Body:        parsed.Body,
		DeliveryKey: store.DeliveryHash(envelopeFrom, inbox, raw),
	}

	// DeliverMessage commits the inbox and the message in one transaction.
	// Success is acknowledged only after that commit.
	if err := s.store.DeliverMessage(ctx, msg); err != nil {
		if errors.Is(err, store.ErrDuplicateDelivery) {
			// An earlier attempt already committed this delivery, which counts
			// as success so the relay does not retry forever.
			s.log.Info("duplicate delivery acknowledged", "address", inbox)
			writeJSON(w, http.StatusOK, inboundResponse{OK: true, Duplicate: true})
			return
		}
		s.log.Error("inbound storage failed", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not store message")
		return
	}

	s.log.Info("message stored", "address", inbox)
	writeJSON(w, http.StatusOK, inboundResponse{OK: true, MessageID: msg.ID})
}

// validateEnvelopeTo checks the recipient and that its domain is served here.
//
// Cloudflare may route any address on the zone to the Worker, so accepting an
// unknown domain would create inboxes nobody can claim and mail that can never
// be read back.
func validateEnvelopeTo(to string, domains []string) (local, domain string, err error) {
	if strings.TrimSpace(to) == "" {
		return "", "", errors.New("Missing X-Envelope-To header")
	}

	local, domain, err = address.Split(to)
	if err != nil {
		return "", "", fmt.Errorf("Invalid envelope recipient: %s", err)
	}
	if err := address.ValidateLocalPart(local); err != nil {
		return "", "", fmt.Errorf("Invalid envelope recipient: %s", err)
	}

	for _, d := range domains {
		if d == domain {
			return local, domain, nil
		}
	}
	return "", "", fmt.Errorf("Unsupported recipient domain: %s", domain)
}

// resolveSender determines the stored display sender.
//
// The SMTP null reverse-path (RFC 5321 3.6.3) is legitimate for bounces and
// auto-replies, so it is accepted and the MIME From header is used instead.
func resolveSender(envelopeFrom, mimeFrom string) (string, error) {
	f := strings.ToLower(strings.TrimSpace(envelopeFrom))

	// A valid non-null envelope sender wins: it is what the receiving MTA saw.
	if f != "" && f != address.NullReversePath {
		if err := address.ValidateEnvelopeFrom(f); err != nil {
			return "", fmt.Errorf("Invalid envelope sender: %s", err)
		}
		return f, nil
	}

	if m := strings.ToLower(strings.TrimSpace(mimeFrom)); m != "" {
		return m, nil
	}
	return unknownSender, nil
}

// readRaw reads the body, reporting whether it exceeded the limit.
//
// Reading once into memory is deliberate: the same bytes feed the delivery hash
// and the MIME parser, so hashing cannot diverge from what was parsed.
func readRaw(body io.Reader, limit int64) (raw []byte, oversized bool, err error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, fmt.Errorf("read raw message: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

// newMessageID returns a unique message identifier.
//
// The reference used an epoch timestamp plus 8 hex characters of a UUID, which
// is guessable and can collide within the same millisecond under bursts. A full
// random value avoids both; the ID is exposed in API responses.
func newMessageID() string {
	return "msg_" + randomHex(16)
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure means no secure entropy source is available.
		// Falling back to a clock value would reintroduce collisions.
		panic(fmt.Sprintf("api: crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(b)
}
