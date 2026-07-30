package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func (r Recording) MarshalJSON() ([]byte, error) {
	type out struct {
		ID         int64          `json:"id"`
		ScheduleID *int64         `json:"schedule_id"`
		Channel    string         `json:"channel"`
		Title      string         `json:"title,omitempty"`
		Start      time.Time      `json:"start"`
		End        *time.Time     `json:"end"`
		Path       string         `json:"path"`
		SizeBytes  *int64         `json:"size_bytes"`
		State      RecordingState `json:"state"`
		Error      string         `json:"error,omitempty"`
	}
	o := out{
		ID:      r.ID,
		Channel: r.Channel,
		Title:   r.Title,
		Start:   r.Start,
		Path:    r.Path,
		State:   r.State,
		Error:   r.Error,
	}
	if r.ScheduleID.Valid {
		v := r.ScheduleID.Int64
		o.ScheduleID = &v
	}
	if r.End.Valid {
		v := r.End.Time
		o.End = &v
	}
	if r.SizeBytes.Valid {
		v := r.SizeBytes.Int64
		o.SizeBytes = &v
	}
	return json.Marshal(o)
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

// recordingColumns is shared by every read so the scan order below stays
// valid for all of them.
const recordingColumns = `id, schedule_id, channel, COALESCE(title,''),
               start_utc, end_utc, path, size_bytes, state, COALESCE(error,'')`

func (s *Store) ListRecordings(ctx context.Context) ([]Recording, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT `+recordingColumns+`
        FROM recordings
        ORDER BY start_utc DESC
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Recording
	for rows.Next() {
		r, err := scanRecording(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRecording returns one recording, or (nil, nil) when no such row
// exists — "not found" is an ordinary answer here, not a failure.
func (s *Store) GetRecording(ctx context.Context, id int64) (*Recording, error) {
	row := s.db.QueryRowContext(ctx, `
        SELECT `+recordingColumns+`
        FROM recordings WHERE id = ?
    `, id)
	r, err := scanRecording(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteRecording removes the row and reports whether one was actually
// there. It does not touch the file on disk — the caller owns that,
// because only it knows which paths it is willing to unlink.
func (s *Store) DeleteRecording(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface{ Scan(dest ...any) error }

func scanRecording(sc rowScanner) (Recording, error) {
	var (
		r      Recording
		startU int64
		endU   sql.NullInt64
		state  string
	)
	if err := sc.Scan(&r.ID, &r.ScheduleID, &r.Channel, &r.Title,
		&startU, &endU, &r.Path, &r.SizeBytes, &state, &r.Error); err != nil {
		return Recording{}, err
	}
	r.Start = time.Unix(startU, 0).UTC()
	if endU.Valid {
		r.End = sql.NullTime{Time: time.Unix(endU.Int64, 0).UTC(), Valid: true}
	}
	r.State = RecordingState(state)
	return r, nil
}
