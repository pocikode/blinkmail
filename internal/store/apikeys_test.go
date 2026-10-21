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
	if err := s.CreateAPIKey(ctx, id, name, pfx, hash); err != nil {
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

// TestAPIKeyHashIsUnique verifies the UNIQUE constraint the lookup relies on.
func TestAPIKeyHashIsUnique(t *testing.T) {
	s, _ := openTemp(t)
	ctx := t.Context()

	const hash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := s.CreateAPIKey(ctx, "one", "first", "bm_aaaaaaaaa", hash); err != nil {
		t.Fatalf("first CreateAPIKey: %v", err)
	}
	if err := s.CreateAPIKey(ctx, "two", "second", "bm_bbbbbbbbb", hash); err == nil {
		t.Error("a duplicate key_hash was accepted")
	}
}
