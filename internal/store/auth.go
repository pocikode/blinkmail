package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// settingAdminPassword is the settings key holding the admin password hash.
//
// The hash is stored PBKDF2-encoded (see internal/auth), never in plain text,
// so a database leak does not hand over the password itself.
const settingAdminPassword = "admin_password"

// SetPasswordHash stores the admin password hash, replacing any previous value.
//
// Upsert rather than insert: changing the password must work on an existing
// row, and the first-start seed must not have to know whether it is first.
func (s *Store) SetPasswordHash(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingAdminPassword, hash, formatTime(NowUTC()),
	)
	if err != nil {
		return fmt.Errorf("store: set password hash: %w", err)
	}
	return nil
}

// PasswordHash returns the stored admin password hash.
//
// ErrNotFound means no password has been set yet, which is what tells startup
// that this is a first run and INITIAL_PASSWORD must seed one.
func (s *Store) PasswordHash(ctx context.Context) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, settingAdminPassword,
	).Scan(&hash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: admin password is not set", ErrNotFound)
	case err != nil:
		return "", fmt.Errorf("store: read password hash: %w", err)
	}
	return hash, nil
}

// CreateAuthSession registers a login session that expires at expiresAt.
func (s *Store) CreateAuthSession(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO auth_sessions (id, created_at, expires_at) VALUES (?, ?, ?)`,
		id, formatTime(NowUTC()), formatTime(expiresAt),
	)
	if err != nil {
		return fmt.Errorf("store: create auth session: %w", err)
	}
	return nil
}

// AuthSessionValid reports whether a login session exists and has not expired.
//
// The expiry check happens in SQL against the stored timestamp, so a session
// cannot outlive its row merely because the process's clock moved; an expired
// row is reported as invalid and left for DeleteExpiredAuthSessions to remove.
func (s *Store) AuthSessionValid(ctx context.Context, id string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM auth_sessions WHERE id = ? AND expires_at > ?`,
		id, formatTime(NowUTC()),
	).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: check auth session: %w", err)
	}
	return true, nil
}

// DeleteAuthSession revokes one login session. Deleting an unknown id is a
// no-op so logout stays idempotent.
func (s *Store) DeleteAuthSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE id = ?`, id,
	); err != nil {
		return fmt.Errorf("store: delete auth session: %w", err)
	}
	return nil
}

// DeleteOtherAuthSessions revokes every login session except keepID.
//
// A password change must log out every other browser: otherwise a session
// created with the old password would survive the rotation, defeating the point
// of changing it.
func (s *Store) DeleteOtherAuthSessions(ctx context.Context, keepID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE id <> ?`, keepID,
	); err != nil {
		return fmt.Errorf("store: delete other auth sessions: %w", err)
	}
	return nil
}

// DeleteExpiredAuthSessions prunes login sessions whose expiry has passed.
//
// Called once at startup: nothing else needs the rows, and the index on
// expires_at makes the sweep cheap.
func (s *Store) DeleteExpiredAuthSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM auth_sessions WHERE expires_at <= ?`, formatTime(NowUTC()),
	); err != nil {
		return fmt.Errorf("store: delete expired auth sessions: %w", err)
	}
	return nil
}
