// Package store wraps the sqlite database used for EPG events,
// recordings, and schedules. Open with WAL so the EPG ingest doesn't
// stall UI reads — see CLAUDE.md.
package store

import "database/sql"

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the sqlite database at path and runs any
// pending migrations from internal/store/migrations/.
func Open(path string) (*Store, error) {
	panic("not implemented")
}

func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
