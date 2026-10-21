// Package store owns all SQLite persistence for Blinkmail.
//
// It uses a four-table model (inboxes, messages, sessions, session_inboxes)
// with a unique delivery_hash column for deduplicating repeated Email Worker
// deliveries, plus the authentication tables (settings, auth_sessions) and
// api_keys for managed API credentials.
//
// Every query uses bound parameters. No user input is ever interpolated into
// SQL.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, allows CGO_ENABLED=0
)

// Sentinel errors returned by Store methods.
var (
	// ErrDuplicateDelivery reports that an identical message was already
	// stored. A duplicate counts as a successful delivery.
	ErrDuplicateDelivery = errors.New("duplicate delivery")

	// ErrAddressTaken reports that an address already exists. Callers that
	// generate random addresses retry; the database constraint is what makes
	// concurrent creation safe.
	ErrAddressTaken = errors.New("inbox address already exists")

	// ErrNotFound reports that no row matched.
	ErrNotFound = errors.New("not found")
)

// SQLite extended result codes for constraint violations.
//
// The distinction matters: every PRIMARY KEY in this schema (inboxes.address,
// sessions.id, session_inboxes composite, messages.id) is a TEXT key backed by
// a unique index and reports SQLITE_CONSTRAINT_PRIMARYKEY (1555). The only
// explicit UNIQUE constraint is messages.delivery_hash, which reports
// SQLITE_CONSTRAINT_UNIQUE (2067). Treating the two alike would let a messages.id
// clash be misread as a duplicate delivery and acknowledged, losing a distinct
// message; so each collision site matches exactly the code it expects.
const (
	codeConstraintUnique  = 2067 // SQLITE_CONSTRAINT_UNIQUE
	codeConstraintPrimary = 1555 // SQLITE_CONSTRAINT_PRIMARYKEY
)

// Inbox is a public disposable mailbox.
//
// Inboxes are reachable by anyone who knows the address; sessions only
// organise which addresses a particular browser sees.
type Inbox struct {
	Address   string `json:"address"`
	CreatedAt string `json:"created_at"`
}

// Message is one stored inbound email.
type Message struct {
	ID           string `json:"id"`
	InboxAddress string `json:"inbox_address"`
	FromAddress  string `json:"from_address"`
	Subject      string `json:"subject"`
	Body         string `json:"body"`
	ReceivedAt   string `json:"received_at"`
}

// NewMessage is a parsed inbound message awaiting storage.
type NewMessage struct {
	ID          string
	Inbox       string
	From        string
	Subject     string
	Body        string
	DeliveryKey string
}

// Store wraps the SQLite connection pool. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// migrations found in the given filesystem.
//
// The parent directory of path is created first, because Blinkmail runs in a
// container whose mounted volume may not exist yet.
func Open(path string, migrations fs.FS) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is empty")
	}
	if path != ":memory:" {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("store: create database directory %s: %w", dir, err)
			}
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}

	// SQLite allows only one writer at a time, so a large pool just adds lock
	// contention. Keep it small and let _txlock=immediate take the write lock
	// up front instead of upgrading mid-transaction.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	s := &Store{db: db}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}
	if err := s.migrate(migrations); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds the driver data source name with the pragmas Blinkmail requires.
//
// journal_mode and foreign_keys are applied per connection by the driver, so
// declaring them in the DSN covers every connection the pool opens.
func dsn(path string) string {
	return fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate",
		path,
	)
}

// DB exposes the underlying handle for health checks and tests.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Health verifies the database accepts connections and queries.
func (s *Store) Health(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return err
	}
	var n int
	return s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&n)
}

// Timestamps are stored as RFC 3339 UTC so browsers parse them as UTC. The
// reference implementation's "YYYY-MM-DD HH:MM:SS" format has no zone and is
// read as local time, shifting displayed times.
const timeLayout = time.RFC3339Nano

// NowUTC returns the current time in UTC, truncated to milliseconds to match
// the schema's default format.
func NowUTC() time.Time {
	return time.Now().UTC().Truncate(time.Millisecond)
}

// formatTime renders t in the stored timestamp format.
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// DeliveryHash returns the deduplication key for a delivery: a hash of the
// envelope sender, the envelope recipient and the raw MIME bytes. Identical
// redeliveries produce identical keys, so the UNIQUE constraint on
// messages.delivery_hash rejects the repeat instead of storing a duplicate.
func DeliveryHash(envelopeFrom, envelopeTo string, raw []byte) string {
	h := sha256.New()
	// Length-prefix each component so a crafted address cannot shift the
	// field boundary and collide with a different delivery.
	writeFramed(h, envelopeFrom)
	writeFramed(h, envelopeTo)
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// writeFramed writes a 4-byte big-endian length followed by s.
func writeFramed(h hash.Hash, s string) {
	l := uint32(len(s))
	h.Write([]byte{byte(l >> 24), byte(l >> 16), byte(l >> 8), byte(l)})
	h.Write([]byte(s))
}

// sqliteCode extracts the extended result code from a driver error, or -1.
func sqliteCode(err error) int {
	var coder interface{ Code() int }
	if errors.As(err, &coder) {
		return coder.Code()
	}
	return -1
}

// isDuplicateDelivery reports whether err is a UNIQUE violation on
// messages.delivery_hash (2067) — i.e. this exact delivery was already
// committed. A PRIMARY KEY clash on messages.id is deliberately excluded: that
// is a distinct message with a colliding ID and must surface as an error.
func isDuplicateDelivery(err error) bool {
	return sqliteCode(err) == codeConstraintUnique
}

// isAddressTaken reports whether err is a PRIMARY KEY violation on
// inboxes.address (1555), meaning the address already exists.
func isAddressTaken(err error) bool {
	return sqliteCode(err) == codeConstraintPrimary
}
