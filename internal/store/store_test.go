package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpen_RunsMigrationsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("expected at least one applied migration")
	}
}

func TestEPG_UpsertReplaces(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	t0 := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	events := []EPGEvent{
		{ServiceID: 23608, EventID: 100, Start: t0, Duration: 30 * time.Minute,
			Title: "Original", Synopsis: "v1"},
	}
	if err := s.UpsertEPGEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	// Same key, different title:
	events[0].Title = "Updated"
	events[0].Synopsis = "v2"
	if err := s.UpsertEPGEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	got, err := s.EPGBetween(ctx, 23608, t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Title != "Updated" {
		t.Fatalf("title not updated: %q", got[0].Title)
	}
}

func TestEPG_NowPlaying(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	t0 := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)
	events := []EPGEvent{
		{ServiceID: 1, EventID: 1, Start: t0.Add(-time.Hour), Duration: 30 * time.Minute, Title: "earlier"},
		{ServiceID: 1, EventID: 2, Start: t0, Duration: time.Hour, Title: "now"},
		{ServiceID: 1, EventID: 3, Start: t0.Add(2 * time.Hour), Duration: time.Hour, Title: "later"},
	}
	if err := s.UpsertEPGEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	e, err := s.NowPlaying(ctx, 1, t0.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e == nil || e.Title != "now" {
		t.Fatalf("got %v", e)
	}

	// Gap: between earlier (ends t0-30m) and now (starts t0).
	e, err = s.NowPlaying(ctx, 1, t0.Add(-15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Fatalf("expected nil during gap, got %v", e)
	}
}

func TestSchedules_RoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	start := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	id, err := s.CreateSchedule(ctx, Schedule{
		Channel:   "mx",
		ServiceID: 23608,
		Start:     start,
		End:       start.Add(time.Hour),
		Lead:      30 * time.Second,
		Trail:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.ListSchedules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("ListSchedules: %v", list)
	}
	if list[0].State != ScheduleStatePending {
		t.Fatalf("default state: %s", list[0].State)
	}

	// DueSchedules: nothing due yet (start is 10m in the future, lead 30s).
	due, err := s.DueSchedules(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("unexpectedly due: %v", due)
	}

	// Move time forward enough that lead window kicks in.
	due, err = s.DueSchedules(ctx, start.Add(-15*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due, got %d", len(due))
	}

	if err := s.UpdateScheduleState(ctx, id, ScheduleStateCanceled); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueSchedules(ctx, start.Add(-15*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatal("canceled schedule still due")
	}
}

func TestRecordings_RoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	start := time.Now().UTC().Truncate(time.Second)
	id, err := s.CreateRecording(ctx, Recording{
		Channel: "mx",
		Title:   "Test",
		Start:   start,
		Path:    "/tmp/rec.ts",
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.FinalizeRecording(ctx, id,
		start.Add(time.Hour), 12345, RecordingStateDone, ""); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListRecordings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRecordings: %v", list)
	}
	if list[0].State != RecordingStateDone {
		t.Fatalf("state: %s", list[0].State)
	}
	if !list[0].End.Valid || !list[0].SizeBytes.Valid {
		t.Fatal("End/SizeBytes not set after Finalize")
	}
}

func TestRecordings_GetAndDelete(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	start := time.Now().UTC().Truncate(time.Second)
	id, err := s.CreateRecording(ctx, Recording{
		Channel: "mx",
		Title:   "報道ステーション",
		Start:   start,
		Path:    "/var/recordings/2026-07-30/mx_2130.ts",
	})
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.GetRecording(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil {
		t.Fatal("GetRecording returned nil for a row that exists")
	}
	if rec.Channel != "mx" || rec.Title != "報道ステーション" ||
		rec.Path != "/var/recordings/2026-07-30/mx_2130.ts" ||
		rec.State != RecordingStateRecording || !rec.Start.Equal(start) {
		t.Fatalf("rec = %+v", rec)
	}
	if rec.End.Valid || rec.SizeBytes.Valid {
		t.Fatalf("unfinalized row has End/SizeBytes: %+v", rec)
	}

	// A missing row is an ordinary answer, not an error — callers 404 on it.
	missing, err := s.GetRecording(ctx, id+999)
	if err != nil {
		t.Fatalf("GetRecording(missing): %v", err)
	}
	if missing != nil {
		t.Fatalf("GetRecording(missing) = %+v, want nil", missing)
	}

	deleted, err := s.DeleteRecording(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("DeleteRecording reported nothing deleted")
	}
	// The second delete must report false so the API can answer 404
	// instead of pretending it removed something.
	deleted, err = s.DeleteRecording(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("DeleteRecording deleted the same row twice")
	}
	list, err := s.ListRecordings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rows after delete: %+v", list)
	}
}
