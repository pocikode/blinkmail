package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// EmbeddedMigrations returns the filesystem holding Blinkmail's startup
// migrations.
func EmbeddedMigrations() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		// Unreachable: the directory is embedded at compile time.
		panic(fmt.Sprintf("store: embedded migrations missing: %v", err))
	}
	return sub
}

// migrate applies every pending migration inside a single transaction.
//
// Running migrations at startup is idempotent: the schema_migrations table
// records which files have been applied, and the DDL itself uses IF NOT EXISTS.
// That makes repeated startup safe, including when several containers share a
// volume.
func (s *Store) migrate(migrations fs.FS) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := ensureMigrationsTable(ctx, s.db); err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, s.db)
	if err != nil {
		return err
	}

	files, err := migrationFiles(migrations)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("store: no migrations found")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	for _, name := range files {
		if applied[name] {
			continue
		}

		body, err := fs.ReadFile(migrations, name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}

		stmts := splitStatements(string(body))
		if len(stmts) == 0 {
			return fmt.Errorf("store: migration %s contains no statements", name)
		}
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("store: apply migration %s: %w", name, err)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			name, formatTime(NowUTC()),
		); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migrations: %w", err)
	}
	return nil
}

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`)
	if err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	return nil
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: list applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: scan migration name: %w", err)
		}
		applied[name] = true
	}
	return applied, rows.Err()
}

// migrationFiles returns .sql migration names in lexical order, so that
// numerically prefixed files (001_, 002_, ...) apply in sequence.
func migrationFiles(migrations fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(migrations, ".")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations directory: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// splitStatements breaks a migration file into individual statements.
//
// Blinkmail's migrations are plain DDL with no triggers, views, or BEGIN...END
// blocks, so splitting on semicolons outside of quoted strings and comments is
// sufficient and avoids shipping a full SQL parser.
func splitStatements(body string) []string {
	var (
		stmts   []string
		current strings.Builder
		inStr   bool // inside a single-quoted string literal
	)

	lines := strings.Split(body, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip whole-line SQL comments.
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		// A classic loop is required here: the inline-comment case advances i
		// past the remainder of the line.
		for i := 0; i < len(line); i++ {
			ch := line[i]
			switch {
			case inStr:
				current.WriteByte(ch)
				if ch == '\'' {
					inStr = false
				}
			case ch == '\'':
				inStr = true
				current.WriteByte(ch)
			case ch == '-' && i+1 < len(line) && line[i+1] == '-':
				// Inline comment: ignore the rest of the line.
				i = len(line)
			case ch == ';':
				if stmt := strings.TrimSpace(current.String()); stmt != "" {
					stmts = append(stmts, stmt)
				}
				current.Reset()
			default:
				current.WriteByte(ch)
			}
		}
		current.WriteByte('\n')
	}

	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		stmts = append(stmts, stmt)
	}
	return stmts
}
