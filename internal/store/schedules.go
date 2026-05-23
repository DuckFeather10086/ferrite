package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type ScheduleState string

const (
	ScheduleStatePending  ScheduleState = "pending"
	ScheduleStateRunning  ScheduleState = "running"
	ScheduleStateDone     ScheduleState = "done"
	ScheduleStateFailed   ScheduleState = "failed"
	ScheduleStateCanceled ScheduleState = "canceled"
)

type Schedule struct {
	ID        int64
	Channel   string
	ServiceID uint16
	EventID   sql.NullInt64
	Start     time.Time
	End       time.Time
	Lead      time.Duration
	Trail     time.Duration
	State     ScheduleState
	CreatedAt time.Time
}

// MarshalJSON renders durations as integer seconds and nullable
// EventID as either int64 or JSON null. Keeps the API surface
// language-neutral (Lead's underlying time.Duration would otherwise
// marshal to nanoseconds, which is awkward for callers).
func (s Schedule) MarshalJSON() ([]byte, error) {
	type out struct {
		ID        int64         `json:"id"`
		Channel   string        `json:"channel"`
		ServiceID uint16        `json:"service_id"`
		EventID   *int64        `json:"event_id"`
		Start     time.Time     `json:"start"`
		End       time.Time     `json:"end"`
		LeadS     int64         `json:"lead_s"`
		TrailS    int64         `json:"trail_s"`
		State     ScheduleState `json:"state"`
		CreatedAt time.Time     `json:"created_at"`
	}
	o := out{
		ID:        s.ID,
		Channel:   s.Channel,
		ServiceID: s.ServiceID,
		Start:     s.Start,
		End:       s.End,
		LeadS:     int64(s.Lead.Seconds()),
		TrailS:    int64(s.Trail.Seconds()),
		State:     s.State,
		CreatedAt: s.CreatedAt,
	}
	if s.EventID.Valid {
		v := s.EventID.Int64
		o.EventID = &v
	}
	return json.Marshal(o)
}

func (s *Store) CreateSchedule(ctx context.Context, sch Schedule) (int64, error) {
	if sch.State == "" {
		sch.State = ScheduleStatePending
	}
	if sch.CreatedAt.IsZero() {
		sch.CreatedAt = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `
        INSERT INTO schedules (
            channel, service_id, event_id,
            start_utc, end_utc, lead_s, trail_s,
            state, created_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
    `,
		sch.Channel, sch.ServiceID, sch.EventID,
		sch.Start.Unix(), sch.End.Unix(),
		int64(sch.Lead.Seconds()), int64(sch.Trail.Seconds()),
		string(sch.State), sch.CreatedAt.Unix(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateScheduleState(ctx context.Context, id int64, state ScheduleState) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE schedules SET state = ? WHERE id = ?`, string(state), id)
	return err
}

// DueSchedules returns pending schedules whose (start - lead) <= now.
func (s *Store) DueSchedules(ctx context.Context, now time.Time) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, channel, COALESCE(service_id,0), event_id,
               start_utc, end_utc, lead_s, trail_s,
               state, created_at
        FROM schedules
        WHERE state = 'pending'
          AND (start_utc - lead_s) <= ?
        ORDER BY start_utc ASC
    `, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT id, channel, COALESCE(service_id,0), event_id,
               start_utc, end_utc, lead_s, trail_s,
               state, created_at
        FROM schedules
        ORDER BY start_utc ASC
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSchedules(rows)
}

func scanSchedules(rows *sql.Rows) ([]Schedule, error) {
	var out []Schedule
	for rows.Next() {
		var (
			sch                                   Schedule
			startU, endU, leadS, trailS, createdU int64
			state                                 string
		)
		if err := rows.Scan(&sch.ID, &sch.Channel, &sch.ServiceID, &sch.EventID,
			&startU, &endU, &leadS, &trailS, &state, &createdU); err != nil {
			return nil, err
		}
		sch.Start = time.Unix(startU, 0).UTC()
		sch.End = time.Unix(endU, 0).UTC()
		sch.Lead = time.Duration(leadS) * time.Second
		sch.Trail = time.Duration(trailS) * time.Second
		sch.State = ScheduleState(state)
		sch.CreatedAt = time.Unix(createdU, 0).UTC()
		out = append(out, sch)
	}
	return out, rows.Err()
}
