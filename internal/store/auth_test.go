package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"pocikode/blinkmail/internal/store"
)

// TestPasswordHashRoundTrip verifies the hash is stored and read back unchanged,
// and that an upsert replaces it rather than creating a second row.
func TestPasswordHashRoundTrip(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	if _, err := s.PasswordHash(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PasswordHash on empty store = %v, want ErrNotFound", err)
	}

	const first = "pbkdf2-sha256$600000$aa$bb"
	if err := s.SetPasswordHash(ctx, first); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	got, err := s.PasswordHash(ctx)
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if got != first {
		t.Errorf("PasswordHash = %q, want %q", got, first)
	}

	// A change replaces the value; the settings table must not accumulate rows.
	const second = "pbkdf2-sha256$600000$cc$dd"
	if err := s.SetPasswordHash(ctx, second); err != nil {
		t.Fatalf("SetPasswordHash (replace): %v", err)
	}
	got, err = s.PasswordHash(ctx)
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if got != second {
		t.Errorf("PasswordHash = %q, want the replaced value %q", got, second)
	}

	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&n); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if n != 1 {
		t.Errorf("settings rows = %d, want 1", n)
	}
}

// TestAuthSessionLifecycle covers create, validate, delete, and the fact that a
// session disappears from the valid set once its expiry has passed.
func TestAuthSessionLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	id := strings.Repeat("a", 64)
	if err := s.CreateAuthSession(ctx, id, store.NowUTC().Add(time.Hour)); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}

	valid, err := s.AuthSessionValid(ctx, id)
	if err != nil {
		t.Fatalf("AuthSessionValid: %v", err)
	}
	if !valid {
		t.Error("fresh session is not valid")
	}

	// An unknown id is simply invalid, not an error.
	valid, err = s.AuthSessionValid(ctx, strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("AuthSessionValid (unknown): %v", err)
	}
	if valid {
		t.Error("unknown session reported valid")
	}

	if err := s.DeleteAuthSession(ctx, id); err != nil {
		t.Fatalf("DeleteAuthSession: %v", err)
	}
	valid, err = s.AuthSessionValid(ctx, id)
	if err != nil {
		t.Fatalf("AuthSessionValid after delete: %v", err)
	}
	if valid {
		t.Error("deleted session reported valid")
	}
	// Deleting again is a no-op so logout stays idempotent.
	if err := s.DeleteAuthSession(ctx, id); err != nil {
		t.Fatalf("DeleteAuthSession (repeat): %v", err)
	}
}

// TestAuthSessionExpiry verifies an expired row is not valid, even before the
// pruning sweep has run.
func TestAuthSessionExpiry(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	expired := strings.Repeat("c", 64)
	if err := s.CreateAuthSession(ctx, expired, store.NowUTC().Add(-time.Minute)); err != nil {
		t.Fatalf("CreateAuthSession: %v", err)
	}

	valid, err := s.AuthSessionValid(ctx, expired)
	if err != nil {
		t.Fatalf("AuthSessionValid: %v", err)
	}
	if valid {
		t.Error("expired session reported valid")
	}

	if err := s.DeleteExpiredAuthSessions(ctx); err != nil {
		t.Fatalf("DeleteExpiredAuthSessions: %v", err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_sessions`).Scan(&n); err != nil {
		t.Fatalf("count auth sessions: %v", err)
	}
	if n != 0 {
		t.Errorf("auth_sessions rows = %d after pruning, want 0", n)
	}
}

// TestDeleteOtherAuthSessions verifies a password change can keep the caller's
// session while revoking every other one.
func TestDeleteOtherAuthSessions(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	keep := strings.Repeat("d", 64)
	other := strings.Repeat("e", 64)
	expires := store.NowUTC().Add(time.Hour)
	for _, id := range []string{keep, other} {
		if err := s.CreateAuthSession(ctx, id, expires); err != nil {
			t.Fatalf("CreateAuthSession(%s): %v", id, err)
		}
	}

	if err := s.DeleteOtherAuthSessions(ctx, keep); err != nil {
		t.Fatalf("DeleteOtherAuthSessions: %v", err)
	}

	for id, want := range map[string]bool{keep: true, other: false} {
		valid, err := s.AuthSessionValid(ctx, id)
		if err != nil {
			t.Fatalf("AuthSessionValid(%s): %v", id, err)
		}
		if valid != want {
			t.Errorf("session %s valid = %v, want %v", id, valid, want)
		}
	}
}
