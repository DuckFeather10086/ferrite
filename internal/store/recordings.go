package store

import (
	"context"
	"database/sql"
	"time"
)

type RecordingState string

const (
	RecordingStateRecording RecordingState = "recording"
	RecordingStateDone      RecordingState = "done"
	RecordingStateFailed    RecordingState = "failed"
)

type Recording struct {
	ID         int64
	ScheduleID sql.NullInt64
	Channel    string
	Title      string
	Start      time.Time
	End        sql.NullTime
	Path       string
	SizeBytes  sql.NullInt64
	State      RecordingState
	Error      string
}

func (s *Store) CreateRecording(ctx context.Context, r Recording) (int64, error) {
	if r.State == "" {
		r.State = RecordingStateRecording
	}
	if r.Start.IsZero() {
		r.Start = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO recordings (schedule_id, channel, title, start_utc, path, state, error)
        VALUES (?, ?, ?, ?, ?, ?, ?)
    `, r.ScheduleID, r.Channel, r.Title, r.Start.Unix(), r.Path, string(r.State), r.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinalizeRecording(ctx context.Context, id int64, end time.Time, sizeBytes int64, state RecordingState, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
        UPDATE recordings SET
            end_utc    = ?,
            size_bytes = ?,
            state      = ?,
            error      = ?
        WHERE id = ?
    `, end.Unix(), sizeBytes, string(state), errMsg, id)
	return err
}

func (s *Store) ListRecordings(ctx context.Context) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, schedule_id, channel, COALESCE(title,''),
               start_utc, end_utc, path, size_bytes, state, COALESCE(error,'')
        FROM recordings
        ORDER BY start_utc DESC
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		var (
			r       Recording
			startU  int64
			endU    sql.NullInt64
			state   string
		)
		if err := rows.Scan(&r.ID, &r.ScheduleID, &r.Channel, &r.Title,
			&startU, &endU, &r.Path, &r.SizeBytes, &state, &r.Error); err != nil {
			return nil, err
		}
		r.Start = time.Unix(startU, 0).UTC()
		if endU.Valid {
			r.End = sql.NullTime{Time: time.Unix(endU.Int64, 0).UTC(), Valid: true}
		}
		r.State = RecordingState(state)
		out = append(out, r)
	}
	return out, rows.Err()
}
