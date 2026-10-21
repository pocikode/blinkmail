package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// APIKey is a managed API key.
//
// Hash is populated only by APIKeyByHash; every other query leaves it empty and
// it is never serialised, so the stored hash cannot leak through /api/keys.
type APIKey struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	Hash       string  `json:"-"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// CreateAPIKey stores a new key. The caller generates the id, the display
// prefix and the hash; the plaintext never reaches the database.
func (s *Store) CreateAPIKey(ctx context.Context, id, name, prefix, keyHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, prefix, key_hash, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, name, prefix, keyHash, formatTime(NowUTC()),
	)
	if err != nil {
		return fmt.Errorf("store: create API key: %w", err)
	}
	return nil
}

// APIKeys lists every key, newest first. The hash is never selected, so it
// cannot be returned to a caller by accident.
func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, prefix, created_at, last_used_at
		FROM api_keys
		ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: list API keys: %w", err)
	}
	defer rows.Close()

	out := []APIKey{}
	for rows.Next() {
		var (
			k  APIKey
			lu sql.NullString
		)
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &lu); err != nil {
			return nil, fmt.Errorf("store: scan API key: %w", err)
		}
		k.LastUsedAt = nullStringPtr(lu)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate API keys: %w", err)
	}
	return out, nil
}

// DeleteAPIKey revokes a key. ErrNotFound means there was nothing to revoke.
func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete API key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete API key: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: API key %s", ErrNotFound, id)
	}
	return nil
}

// APIKeyByHash returns the key whose hash matches. The hash is included so the
// caller can confirm the match in constant time before trusting the row.
func (s *Store) APIKeyByHash(ctx context.Context, keyHash string) (APIKey, error) {
	var (
		k  APIKey
		lu sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, prefix, key_hash, created_at, last_used_at
		FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&k.ID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt, &lu)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return APIKey{}, fmt.Errorf("%w: API key", ErrNotFound)
	case err != nil:
		return APIKey{}, fmt.Errorf("store: get API key by hash: %w", err)
	}
	k.LastUsedAt = nullStringPtr(lu)
	return k, nil
}

// TouchAPIKey records a successful use. Best effort: the caller logs and
// continues on error, because a stale timestamp must never fail a request.
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		formatTime(NowUTC()), id,
	); err != nil {
		return fmt.Errorf("store: touch API key: %w", err)
	}
	return nil
}

// nullStringPtr maps SQL NULL to a nil pointer, so a never-used key serialises
// as "last_used_at": null rather than an empty string.
func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}
