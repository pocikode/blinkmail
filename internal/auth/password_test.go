package auth_test

import (
	"errors"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/auth"
)

const password = "correct-horse-battery-staple"

// TestHashVerifyRoundTrip is the core property: a hash verifies the password it
// was derived from and rejects everything else.
func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	ok, err := auth.Verify(hash, password)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Error("Verify rejected the correct password")
	}

	for _, wrong := range []string{"", "correct-horse-battery-stapl", password + "x", strings.ToUpper(password)} {
		ok, err := auth.Verify(hash, wrong)
		if err != nil {
			t.Fatalf("Verify(%q): %v", wrong, err)
		}
		if ok {
			t.Errorf("Verify accepted the wrong password %q", wrong)
		}
	}
}

// TestHashEncoding pins the stored format: the scheme, iteration count, salt
// and key are all readable, and nothing about the password is.
func TestHashEncoding(t *testing.T) {
	hash, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}

	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		t.Fatalf("hash %q has %d fields, want 4", hash, len(parts))
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Errorf("scheme = %q, want pbkdf2-sha256", parts[0])
	}
	if parts[1] != "600000" {
		t.Errorf("iterations = %q, want 600000", parts[1])
	}
	// 32-byte salt and key are 64 hex characters each.
	if len(parts[2]) != 64 {
		t.Errorf("salt length = %d hex chars, want 64", len(parts[2]))
	}
	if len(parts[3]) != 64 {
		t.Errorf("key length = %d hex chars, want 64", len(parts[3]))
	}
	if strings.Contains(hash, password) {
		t.Error("encoded hash contains the plaintext password")
	}
}

// TestHashUsesFreshSalt guards against a fixed salt: identical passwords must
// produce different stored values, or one cracked hash would reveal every
// account that shares the password.
func TestHashUsesFreshSalt(t *testing.T) {
	a, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := auth.Hash(password)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
	// Both must still verify.
	for _, h := range []string{a, b} {
		ok, err := auth.Verify(h, password)
		if err != nil || !ok {
			t.Errorf("Verify(%q) = %v, %v", h, ok, err)
		}
	}
}

// TestVerifyRejectsMalformedHash ensures a corrupted stored value is reported as
// an error rather than as a wrong password, so the two cases can be handled
// differently (500 vs 401).
func TestVerifyRejectsMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"not-a-hash",
		"pbkdf2-sha256$600000$onlythreefields",
		"pbkdf2-sha256$abc$aa$bb",
		"pbkdf2-sha256$600000$zz$bb",
		"pbkdf2-sha256$600000$aa$zz",
		"pbkdf2-sha256$600000$$bb",
		"bcrypt$600000$aa$bb",
	}
	for _, encoded := range cases {
		if _, err := auth.Verify(encoded, password); !errors.Is(err, auth.ErrMalformedHash) {
			t.Errorf("Verify(%q) error = %v, want ErrMalformedHash", encoded, err)
		}
	}
}

// TestValidatePassword pins the shared policy: eight characters, and nothing
// else — long passphrases and symbols are welcome.
func TestValidatePassword(t *testing.T) {
	for _, ok := range []string{"12345678", "a longer passphrase", "p@ss w0rd!"} {
		if err := auth.ValidatePassword(ok); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", auth.MinPasswordLength-1)} {
		if err := auth.ValidatePassword(bad); !errors.Is(err, auth.ErrPasswordTooShort) {
			t.Errorf("ValidatePassword(%q) = %v, want ErrPasswordTooShort", bad, err)
		}
	}
}
