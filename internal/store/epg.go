package store

import (
	"context"
	"database/sql"
	"time"
)

// EPGEvent is one EIT event row, decoded into Go types.
type EPGEvent struct {
	ServiceID  uint16
	EventID    uint16
	Start      time.Time
	Duration   time.Duration
	Title      string
	Synopsis   string
	Raw        string
	IngestedAt time.Time
}

// UpsertEPGEvents writes events transactionally, replacing existing
// (service_id, event_id) rows. Use this from epg.Refresher.
func (s *Store) UpsertEPGEvents(ctx context.Context, events []EPGEvent) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `
        INSERT INTO epg_events (
            service_id, event_id, start_utc, duration_s,
            title, synopsis, raw, ingested_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
        ON CONFLICT(service_id, event_id) DO UPDATE SET
            start_utc   = excluded.start_utc,
            duration_s  = excluded.duration_s,
            title       = excluded.title,
            synopsis    = excluded.synopsis,
            raw         = excluded.raw,
            ingested_at = excluded.ingested_at
    `)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	for _, e := range events {
		ingested := e.IngestedAt.Unix()
		if ingested == 0 {
			ingested = now
		}
		_, err := stmt.ExecContext(ctx,
			e.ServiceID, e.EventID,
			e.Start.Unix(), int64(e.Duration.Seconds()),
			e.Title, e.Synopsis, e.Raw, ingested,
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EPGBetween returns events for serviceID that overlap [from, to].
// Pass serviceID == 0 to return events across all services.
func (s *Store) EPGBetween(ctx context.Context, serviceID uint16, from, to time.Time) ([]EPGEvent, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if serviceID == 0 {
		rows, err = s.db.QueryContext(ctx, `
            SELECT service_id, event_id, start_utc, duration_s,
                   title, COALESCE(synopsis,''), COALESCE(raw,''), ingested_at
            FROM epg_events
            WHERE start_utc + duration_s >= ? AND start_utc <= ?
            ORDER BY start_utc ASC
        `, from.Unix(), to.Unix())
	} else {
		rows, err = s.db.QueryContext(ctx, `
            SELECT service_id, event_id, start_utc, duration_s,
                   title, COALESCE(synopsis,''), COALESCE(raw,''), ingested_at
            FROM epg_events
            WHERE service_id = ?
              AND start_utc + duration_s >= ?
              AND start_utc <= ?
            ORDER BY start_utc ASC
        `, serviceID, from.Unix(), to.Unix())
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EPGEvent
	for rows.Next() {
		var (
			e        EPGEvent
			startU   int64
			durS     int64
			ingestU  int64
		)
		if err := rows.Scan(&e.ServiceID, &e.EventID, &startU, &durS,
			&e.Title, &e.Synopsis, &e.Raw, &ingestU); err != nil {
			return nil, err
		}
		e.Start = time.Unix(startU, 0).UTC()
		e.Duration = time.Duration(durS) * time.Second
		e.IngestedAt = time.Unix(ingestU, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// NowPlaying returns the event currently airing on serviceID, or nil
// if nothing in the table covers `at`.
func (s *Store) NowPlaying(ctx context.Context, serviceID uint16, at time.Time) (*EPGEvent, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT service_id, event_id, start_utc, duration_s,
               title, COALESCE(synopsis,''), COALESCE(raw,''), ingested_at
        FROM epg_events
        WHERE service_id = ?
          AND start_utc <= ?
          AND start_utc + duration_s > ?
        ORDER BY start_utc DESC
        LIMIT 1
    `, serviceID, at.Unix(), at.Unix())

	var (
		e       EPGEvent
		startU  int64
		durS    int64
		ingestU int64
	)
	err := row.Scan(&e.ServiceID, &e.EventID, &startU, &durS,
		&e.Title, &e.Synopsis, &e.Raw, &ingestU)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	e.Start = time.Unix(startU, 0).UTC()
	e.Duration = time.Duration(durS) * time.Second
	e.IngestedAt = time.Unix(ingestU, 0).UTC()
	return &e, nil
}
