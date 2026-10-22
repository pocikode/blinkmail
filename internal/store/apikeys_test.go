package store_test

import (
	"errors"
	"testing"

	"pocikode/blinkmail/internal/store"
)

// TestAPIKeyLifecycle covers create, list, lookup by hash, touch and delete.
func TestAPIKeyLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	keys, err := s.APIKeys(ctx)
	if err != nil {
		t.Fatalf("APIKeys on empty store: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("APIKeys = %+v, want empty", keys)
	}

	const (
		id   = "key-id-1"
		name = "relay"
		pfx  = "bm_012345678"
		hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	if err := s.CreateAPIKey(ctx, id, name, pfx, hash, "inbound,read"); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	keys, err = s.APIKeys(ctx)
	if err != nil {
		t.Fatalf("APIKeys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("APIKeys = %d rows, want 1", len(keys))
	}
	k := keys[0]
	if k.ID != id || k.Name != name || k.Prefix != pfx {
		t.Errorf("APIKeys[0] = %+v", k)
	}
	if k.Permissions != "inbound,read" {
		t.Errorf("Permissions = %q, want inbound,read", k.Permissions)
	}
	if k.CreatedAt == "" {
		t.Error("CreatedAt is empty")
	}
	if k.LastUsedAt != nil {
		t.Errorf("LastUsedAt = %v, want nil before first use", *k.LastUsedAt)
	}
	// The list must never carry the hash.
	if k.Hash != "" {
		t.Errorf("APIKeys exposed the hash %q", k.Hash)
	}

	byHash, err := s.APIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("APIKeyByHash: %v", err)
	}
	if byHash.ID != id {
		t.Errorf("APIKeyByHash id = %q, want %q", byHash.ID, id)
	}
	if byHash.Hash != hash {
		t.Errorf("APIKeyByHash hash = %q, want %q", byHash.Hash, hash)
	}
	if byHash.Permissions != "inbound,read" {
		t.Errorf("APIKeyByHash permissions = %q, want inbound,read", byHash.Permissions)
	}

	if _, err := s.APIKeyByHash(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("APIKeyByHash(unknown) = %v, want ErrNotFound", err)
	}

	if err := s.TouchAPIKey(ctx, id); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}
	touched, err := s.APIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("APIKeyByHash after touch: %v", err)
	}
	if touched.LastUsedAt == nil || *touched.LastUsedAt == "" {
		t.Error("LastUsedAt was not set by TouchAPIKey")
	}

	if err := s.DeleteAPIKey(ctx, id); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	if _, err := s.APIKeyByHash(ctx, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("APIKeyByHash after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteAPIKey(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteAPIKey(unknown) = %v, want ErrNotFound", err)
	}
}

// TestAPIKeyPermissionsDefaultForExistingRows verifies the migration default:
// a row inserted without permissions (as every pre-scoping row was) reads back
// as inbound,read rather than an empty capability set.
func TestAPIKeyPermissionsDefaultForExistingRows(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	const (
		id   = "legacy"
		hash = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)
	if _, err := s.DB().ExecContext(ctx, `
		INSERT INTO api_keys (id, name, prefix, key_hash, created_at)
		VALUES (?, 'legacy', 'bm_ccccccccc', ?, '2026-01-01T00:00:00Z')`,
		id, hash,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	k, err := s.APIKeyByHash(ctx, hash)
	if err != nil {
		t.Fatalf("APIKeyByHash: %v", err)
	}
	if k.Permissions != "inbound,read" {
		t.Errorf("permissions = %q, want the migration default inbound,read", k.Permissions)
	}
}

// TestAPIKeyHashIsUnique verifies the UNIQUE constraint the lookup relies on.
func TestAPIKeyHashIsUnique(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.CreateAPIKey(ctx, "one", "first", "bm_aaaaaaaaa", hash, "inbound,read"); err != nil {
		t.Fatalf("first CreateAPIKey: %v", err)
	}
	if err := s.CreateAPIKey(ctx, "two", "second", "bm_bbbbbbbbb", hash, "inbound,read"); err == nil {
		t.Error("a duplicate key_hash was accepted")
	}
}
