package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"pocikode/blinkmail/internal/auth"
	"pocikode/blinkmail/internal/config"
	"pocikode/blinkmail/internal/store"
)

// openTestStore opens a throwaway database with the real migrations applied.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sql"), store.EmbeddedMigrations())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// captureLogger returns a logger writing to a buffer the test can inspect.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// TestBootstrapRequiresInitialPasswordOnFirstStart verifies the process refuses
// to come up with no password at all: an unprotected UI would be open to anyone
// who can reach the port.
func TestBootstrapRequiresInitialPasswordOnFirstStart(t *testing.T) {
	db := openTestStore(t)
	logger, _ := captureLogger()

	err := bootstrapPassword(context.Background(), db, config.Config{}, logger)
	if err == nil {
		t.Fatal("bootstrapPassword succeeded without INITIAL_PASSWORD")
	}
	if !strings.Contains(err.Error(), "INITIAL_PASSWORD must be set on first start") {
		t.Errorf("error = %q, want the first-start message", err)
	}

	// Nothing may have been stored.
	if _, err := db.PasswordHash(context.Background()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PasswordHash = %v, want ErrNotFound", err)
	}
}

// TestBootstrapSeedsPasswordOnce verifies the seed is stored hashed and that a
// later start ignores the environment rather than resetting the password.
func TestBootstrapSeedsPasswordOnce(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	logger, logs := captureLogger()

	const seeded = "seeded-password"
	if err := bootstrapPassword(ctx, db, config.Config{InitialPassword: seeded}, logger); err != nil {
		t.Fatalf("bootstrapPassword: %v", err)
	}
	if !strings.Contains(logs.String(), "admin password initialised from INITIAL_PASSWORD") {
		t.Errorf("logs = %q, want the initialisation message", logs.String())
	}
	if strings.Contains(logs.String(), seeded) {
		t.Error("the password appears in the logs")
	}

	hash, err := db.PasswordHash(ctx)
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Errorf("stored hash = %q, want the pbkdf2 encoding", hash)
	}
	if strings.Contains(hash, seeded) {
		t.Error("stored hash contains the plaintext password")
	}
	ok, err := auth.Verify(hash, seeded)
	if err != nil || !ok {
		t.Fatalf("stored hash does not verify the seeded password: %v, %v", ok, err)
	}

	// Second start with a different INITIAL_PASSWORD must not change anything.
	logger2, logs2 := captureLogger()
	if err := bootstrapPassword(ctx, db, config.Config{InitialPassword: "a-different-password"}, logger2); err != nil {
		t.Fatalf("bootstrapPassword (second start): %v", err)
	}
	after, err := db.PasswordHash(ctx)
	if err != nil {
		t.Fatalf("PasswordHash: %v", err)
	}
	if after != hash {
		t.Error("INITIAL_PASSWORD overwrote an existing password")
	}
	if strings.Contains(logs2.String(), "initialised") {
		t.Error("second start logged an initialisation")
	}
}
