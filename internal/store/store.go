// Package store wraps the sqlite database used for EPG events,
// recordings, and schedules.
//
// The driver is modernc.org/sqlite (pure Go, no cgo), so cross-
// compile to arm64 / Raspberry Pi stays a `GOOS=linux GOARCH=arm64
// go build` one-liner.
//
// WAL is enabled so periodic EPG batch writes don't block UI reads.
// No write-concurrency tuning beyond that — see CLAUDE.md for why.
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Store is the daemon's database handle. All callers share one Store.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the sqlite database at path and applies any
// pending migrations from internal/store/migrations/.
//
// Pass ":memory:" for an in-process database (used by tests).
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=journal_mode(wal)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// Reasonable defaults; sqlite handles a single writer fine.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB for callers that need raw queries.
// Avoid in new code; prefer typed methods on Store.
func (s *Store) DB() *sql.DB { return s.db }

// migrate applies all embedded SQL files in lexical order, recording
// each in schema_migrations so reruns are no-ops.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`
        CREATE TABLE IF NOT EXISTS schema_migrations (
            name       TEXT PRIMARY KEY,
            applied_at INTEGER NOT NULL
        )
    `); err != nil {
		return err
	}

	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, name,
		).Scan(&applied)
		if err != nil {
			return err
		}
		if applied > 0 {
			continue
		}

		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations(name, applied_at) VALUES (?, ?)`,
			name, time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
