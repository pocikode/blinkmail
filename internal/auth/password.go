// Package auth implements password hashing for Blinkmail's single admin
// account.
//
// Only the standard library is used: crypto/pbkdf2 (Go 1.24+) with HMAC-SHA256.
// The encoding is self-describing so the iteration count can be raised later
// without invalidating stored hashes:
//
//	pbkdf2-sha256$<iterations>$<salt-hex>$<key-hex>
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Password policy and hashing parameters.
const (
	// MinPasswordLength is the shortest password accepted from the UI or from
	// INITIAL_PASSWORD. Short enough to be usable, long enough to matter.
	MinPasswordLength = 8

	// Iterations is the PBKDF2 work factor. 600k HMAC-SHA256 iterations is the
	// current OWASP recommendation and costs a few hundred milliseconds, which
	// is unnoticeable for a login and expensive for an offline attacker.
	Iterations = 600_000

	// saltBytes and keyBytes: a 32-byte salt removes any practical reuse across
	// deployments, and a 32-byte key matches the hash output size.
	saltBytes = 32
	keyBytes  = 32

	// scheme names the algorithm in the encoded form.
	scheme = "pbkdf2-sha256"
)

// ErrMalformedHash reports an encoded hash that cannot be parsed. It is a
// storage-corruption signal, not a wrong password.
var ErrMalformedHash = errors.New("auth: malformed password hash")

// ErrPasswordTooShort reports a password below MinPasswordLength. Built with
// the constant so the message can never drift from the check.
var ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLength)

// ValidatePassword enforces the password policy shared by the login seed and
// the change-password endpoint, so both reject the same inputs.
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	return nil
}

// Hash derives an encoded PBKDF2 hash for password with a fresh random salt.
//
// A new salt per call means two users (or two changes by the same user) never
// share a derived key, so the stored value cannot be compared across rows.
func Hash(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, Iterations, keyBytes)
	if err != nil {
		return "", fmt.Errorf("auth: derive key: %w", err)
	}

	return fmt.Sprintf("%s$%d$%s$%s",
		scheme, Iterations, hex.EncodeToString(salt), hex.EncodeToString(key),
	), nil
}

// Verify reports whether password matches the encoded hash.
//
// The comparison is constant time so a timing side channel cannot reveal how
// many leading bytes of a candidate derivation were correct.
func Verify(encoded, password string) (bool, error) {
	parsedScheme, iter, salt, want, err := decode(encoded)
	if err != nil {
		return false, err
	}
	if parsedScheme != scheme {
		return false, fmt.Errorf("%w: unsupported scheme %q", ErrMalformedHash, parsedScheme)
	}

	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, fmt.Errorf("auth: derive key: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// decode splits an encoded hash into its parts.
//
// The iteration count and key length are read back from the stored value rather
// than assumed, so a hash written by an older or newer build still verifies.
func decode(encoded string) (schemeName string, iter int, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return "", 0, nil, nil, fmt.Errorf("%w: expected 4 fields, got %d", ErrMalformedHash, len(parts))
	}

	iter, err = strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return "", 0, nil, nil, fmt.Errorf("%w: invalid iteration count %q", ErrMalformedHash, parts[1])
	}

	salt, err = hex.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return "", 0, nil, nil, fmt.Errorf("%w: invalid salt", ErrMalformedHash)
	}

	key, err = hex.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return "", 0, nil, nil, fmt.Errorf("%w: invalid key", ErrMalformedHash)
	}

	return parts[0], iter, salt, key, nil
}
