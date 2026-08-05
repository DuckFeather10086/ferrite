package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AudioOffset is a cached A/V skew measurement for one channel.
type AudioOffset struct {
	Channel  string
	OffsetS  float64   // raw measurement: video_pts - audio_pts, seconds
	Measured time.Time // UTC
}

// Age reports how long ago the measurement was taken.
func (a AudioOffset) Age() time.Duration { return time.Since(a.Measured) }

// AudioOffsetFor returns the cached skew for channel. ok is false when
// nothing has been measured yet.
func (s *Store) AudioOffsetFor(ctx context.Context, channel string) (AudioOffset, bool, error) {
	var (
		out       = AudioOffset{Channel: channel}
		measuredU int64
	)
	err := s.db.QueryRowContext(ctx, `
        SELECT offset_s, measured_utc FROM av_offsets WHERE channel = ?
    `, channel).Scan(&out.OffsetS, &measuredU)
	if errors.Is(err, sql.ErrNoRows) {
		return AudioOffset{}, false, nil
	}
	if err != nil {
		return AudioOffset{}, false, err
	}
	out.Measured = time.Unix(measuredU, 0).UTC()
	return out, true, nil
}

// PutAudioOffset records (or replaces) the measured skew for channel.
func (s *Store) PutAudioOffset(ctx context.Context, channel string, offsetS float64) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO av_offsets (channel, offset_s, measured_utc)
        VALUES (?, ?, ?)
        ON CONFLICT(channel) DO UPDATE SET
            offset_s     = excluded.offset_s,
            measured_utc = excluded.measured_utc
    `, channel, offsetS, time.Now().UTC().Unix())
	return err
}

// ForgetAudioOffset drops the cached measurement for channel, forcing a
// re-probe on the next tune. Removing a row that isn't there is not an
// error.
func (s *Store) ForgetAudioOffset(ctx context.Context, channel string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM av_offsets WHERE channel = ?`, channel)
	return err
}

// ListAudioOffsets returns every cached measurement, channel-ascending.
func (s *Store) ListAudioOffsets(ctx context.Context) ([]AudioOffset, error) {
	rows, err := s.db.QueryContext(ctx, `
        SELECT channel, offset_s, measured_utc FROM av_offsets ORDER BY channel
    `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AudioOffset
	for rows.Next() {
		var (
			a         AudioOffset
			measuredU int64
		)
		if err := rows.Scan(&a.Channel, &a.OffsetS, &measuredU); err != nil {
			return nil, err
		}
		a.Measured = time.Unix(measuredU, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}
