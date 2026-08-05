package store

import (
	"context"
	"testing"
	"time"
)

func recordingRow(t *testing.T, s *Store, channel string) int64 {
	t.Helper()
	id, err := s.CreateRecording(context.Background(), Recording{
		Channel: channel,
		Path:    "var/recordings/" + channel + ".ts",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// Finishing a recording is what queues it. Doing that in the same statement
// is the point: an enqueue that lived anywhere else could be lost by a daemon
// that died between writing the row and asking for the work.
func TestFinalizeQueuesADoneRecording(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	done := recordingRow(t, s, "NHK_G")
	failed := recordingRow(t, s, "TBS1")
	running := recordingRow(t, s, "asahi")

	if err := s.FinalizeRecording(ctx, done, time.Now(), 1<<20, RecordingStateDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinalizeRecording(ctx, failed, time.Now(), 0, RecordingStateFailed, "no bytes"); err != nil {
		t.Fatal(err)
	}

	queued, err := s.RecordingsToPostprocess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].ID != done {
		t.Fatalf("queued %v, want just the finished recording %d", ids(queued), done)
	}

	// A recording that produced nothing has nothing to convert, and one still
	// in progress is a file that is still being written to.
	rec, _ := s.GetRecording(ctx, failed)
	if rec.PostState != "" {
		t.Fatalf("failed recording queued as %q", rec.PostState)
	}
	rec, _ = s.GetRecording(ctx, running)
	if rec.PostState != "" {
		t.Fatalf("in-progress recording queued as %q", rec.PostState)
	}
}

// 'running' is a job the last daemon was in the middle of. Nothing else is
// going to finish it, so a sweep has to pick it up again.
func TestAnInterruptedJobIsQueuedAgain(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := recordingRow(t, s, "NHK_G")
	if err := s.FinalizeRecording(ctx, id, time.Now(), 1<<20, RecordingStateDone, ""); err != nil {
		t.Fatal(err)
	}

	if err := s.SetPostState(ctx, id, PostStateRunning, ""); err != nil {
		t.Fatal(err)
	}
	if queued, _ := s.RecordingsToPostprocess(ctx); len(queued) != 1 {
		t.Fatalf("a job left running was not picked up: %v", ids(queued))
	}

	if err := s.SetPostState(ctx, id, PostStateDone, ""); err != nil {
		t.Fatal(err)
	}
	if queued, _ := s.RecordingsToPostprocess(ctx); len(queued) != 0 {
		t.Fatalf("a finished job is still queued: %v", ids(queued))
	}
}

// A retry that works must not keep showing the last failure's message.
func TestPostErrorIsClearedWhenTheStateIsNotAFailure(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id := recordingRow(t, s, "NHK_G")

	if err := s.SetPostState(ctx, id, PostStateFailed, "ffmpeg: no such device"); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetRecording(ctx, id)
	if rec.PostState != PostStateFailed || rec.PostError == "" {
		t.Fatalf("state %q err %q", rec.PostState, rec.PostError)
	}

	if err := s.SetPostState(ctx, id, PostStateDone, ""); err != nil {
		t.Fatal(err)
	}
	rec, _ = s.GetRecording(ctx, id)
	if rec.PostError != "" {
		t.Fatalf("stale failure message survived: %q", rec.PostError)
	}
}

func ids(recs []Recording) []int64 {
	out := make([]int64, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}
