package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SessionExists reports whether a session token is registered.
func (s *Store) SessionExists(ctx context.Context, sessionID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, sessionID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: session exists: %w", err)
	}
	return true, nil
}

// CreateSession registers a server-generated session token.
func (s *Store) CreateSession(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		sessionID, formatTime(NowUTC()),
	)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// CreateInbox registers a public inbox. An existing address is left untouched,
// so claiming an address that already has mail preserves its history.
func (s *Store) CreateInbox(ctx context.Context, address string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO inboxes (address, created_at) VALUES (?, ?) ON CONFLICT(address) DO NOTHING`,
		address, formatTime(NowUTC()),
	)
	if err != nil {
		return fmt.Errorf("store: create inbox %q: %w", address, err)
	}
	return nil
}

// ClaimInbox registers a new address and fails if it already exists.
//
// Random address generation depends on this: two concurrent requests cannot
// both succeed for the same generated address, so a loser retries instead of
// silently sharing a mailbox it did not create.
func (s *Store) ClaimInbox(ctx context.Context, address string) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inboxes (address, created_at) VALUES (?, ?)`,
		address, formatTime(NowUTC()),
	)
	if err != nil {
		if isAddressTaken(err) {
			return fmt.Errorf("%w: %s", ErrAddressTaken, address)
		}
		return fmt.Errorf("store: claim inbox %q: %w", address, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrAddressTaken, address)
	}
	return nil
}

// GetInbox returns a single inbox.
func (s *Store) GetInbox(ctx context.Context, address string) (Inbox, error) {
	var in Inbox
	err := s.db.QueryRowContext(ctx,
		`SELECT address, created_at FROM inboxes WHERE address = ?`, address,
	).Scan(&in.Address, &in.CreatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Inbox{}, fmt.Errorf("%w: inbox %s", ErrNotFound, address)
	case err != nil:
		return Inbox{}, fmt.Errorf("store: get inbox %q: %w", address, err)
	}
	return in, nil
}

// InboxExists reports whether an address is registered.
func (s *Store) InboxExists(ctx context.Context, address string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM inboxes WHERE address = ?`, address).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: inbox exists: %w", err)
	}
	return true, nil
}

// SessionInboxes lists the inboxes linked to a session, newest first.
func (s *Store) SessionInboxes(ctx context.Context, sessionID string) ([]Inbox, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.address, i.created_at
		FROM inboxes i
		INNER JOIN session_inboxes si ON si.inbox_address = i.address
		WHERE si.session_id = ?
		ORDER BY i.created_at DESC, i.address ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: list session inboxes: %w", err)
	}
	defer rows.Close()

	out := []Inbox{}
	for rows.Next() {
		var in Inbox
		if err := rows.Scan(&in.Address, &in.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan inbox: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate session inboxes: %w", err)
	}
	return out, nil
}

// LinkInboxToSession attaches an existing inbox to a session. Relinking is a
// no-op, which is how any browser can re-claim a known public address.
func (s *Store) LinkInboxToSession(ctx context.Context, sessionID, address string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session_inboxes (session_id, inbox_address, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id, inbox_address) DO NOTHING`,
		sessionID, address, formatTime(NowUTC()),
	)
	if err != nil {
		return fmt.Errorf("store: link inbox %q to session: %w", address, err)
	}
	return nil
}

// UnlinkInboxFromSession removes the session link only. The inbox and its
// messages are preserved, so removing an address hides it from this browser
// without deleting any mail.
func (s *Store) UnlinkInboxFromSession(ctx context.Context, sessionID, address string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session_inboxes WHERE session_id = ? AND inbox_address = ?`,
		sessionID, address,
	)
	if err != nil {
		return fmt.Errorf("store: unlink inbox %q: %w", address, err)
	}
	return nil
}

// IsInboxInSession reports whether a session is linked to an address.
func (s *Store) IsInboxInSession(ctx context.Context, sessionID, address string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM session_inboxes WHERE session_id = ? AND inbox_address = ?`,
		sessionID, address,
	).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check session inbox link: %w", err)
	}
	return true, nil
}

// Messages returns an inbox's messages, newest first.
func (s *Store) Messages(ctx context.Context, address string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, inbox_address, from_address, subject, body, received_at
		FROM messages
		WHERE inbox_address = ?
		ORDER BY received_at DESC, id DESC`, address)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.InboxAddress, &m.FromAddress, &m.Subject, &m.Body, &m.ReceivedAt); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate messages: %w", err)
	}
	return out, nil
}

// GetMessage returns a single message by ID.
func (s *Store) GetMessage(ctx context.Context, id string) (Message, error) {
	var m Message
	err := s.db.QueryRowContext(ctx, `
		SELECT id, inbox_address, from_address, subject, body, received_at
		FROM messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.InboxAddress, &m.FromAddress, &m.Subject, &m.Body, &m.ReceivedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Message{}, fmt.Errorf("%w: message %s", ErrNotFound, id)
	case err != nil:
		return Message{}, fmt.Errorf("store: get message: %w", err)
	}
	return m, nil
}

// DeliverMessage stores an inbound message, creating its inbox if needed, in a
// single transaction.
//
// Both writes commit before the caller acknowledges the delivery, so a success
// response always means the message is durable. A repeated delivery is
// rejected by the UNIQUE constraint on delivery_hash and reported as
// ErrDuplicateDelivery — also a success from the relay's point of view.
func (s *Store) DeliverMessage(ctx context.Context, m NewMessage) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delivery transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	now := formatTime(NowUTC())

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO inboxes (address, created_at) VALUES (?, ?) ON CONFLICT(address) DO NOTHING`,
		m.Inbox, now,
	); err != nil {
		return fmt.Errorf("store: ensure inbox %q: %w", m.Inbox, err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (id, inbox_address, from_address, subject, body, received_at, delivery_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Inbox, m.From, m.Subject, m.Body, now, nullIfEmpty(m.DeliveryKey),
	); err != nil {
		if isDuplicateDelivery(err) {
			return ErrDuplicateDelivery
		}
		return fmt.Errorf("store: insert message: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit delivery: %w", err)
	}
	return nil
}

// nullIfEmpty maps "" to SQL NULL so rows without a delivery key do not
// collide with each other under the UNIQUE constraint. SQLite treats NULLs as
// distinct, which is exactly the behaviour wanted here.
func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// CountMessages returns the stored message total, used by /healthz.
func (s *Store) CountMessages(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count messages: %w", err)
	}
	return n, nil
}
