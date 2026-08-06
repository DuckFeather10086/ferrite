package epg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// fakeDvbr writes a shell script that stands in for the dvbr binary:
// it runs `body` with the real argv, so tests can make it dump JSON,
// hang, or fail.
func fakeDvbr(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dvbr")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testChannels() *config.Channels {
	return &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
		},
	}
}

// holdTuner hands back a stream that produces nothing and blocks — good
// enough to occupy an adapter.
type holdTuner struct{}

func (holdTuner) Tune(ctx context.Context, _ int, _ string) (tuner.TsStream, error) {
	return &blockedStream{ctx: ctx}, nil
}

type blockedStream struct{ ctx context.Context }

func (b *blockedStream) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, os.ErrClosed
}
func (b *blockedStream) Close() error { return nil }

func newRefresher(t *testing.T, script string, pool *tuner.Pool) (*Refresher, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Refresher{
		DvbrBin:      fakeDvbr(t, script),
		ChannelsFile: filepath.Join(t.TempDir(), "channels.json"),
		Adapter:      0,
		Channels:     testChannels(),
		ChannelNames: []string{"mx"},
		Store:        st,
		Tuners:       pool,
	}, st
}

const oneEventJSON = `echo '[{"event_id":1,"start":"2026-07-30 20:00:00",` +
	`"duration":"00:30:00","running_status":0,"title":"Fake Show","text":"","genres":[]}]'`

// A pass tunes each mux once. dvbr harvests the EIT of the whole transport
// stream, so a second channel on a frequency already visited would spend
// another minute of tuner time re-collecting the same guide.
func TestRefresher_TunesEachMuxOnce(t *testing.T) {
	countFile := filepath.Join(t.TempDir(), "runs")
	script := "echo \"$@\" >> " + countFile + "\n" + oneEventJSON
	r, _ := newRefresher(t, script, nil)
	r.Tuners = nil // no hardware needed: the fake dvbr is the whole pass
	r.Channels = &config.Channels{Version: 1, Channels: []config.Channel{
		{Name: "asahi", Tuning: map[string]string{"SERVICE_ID": "1064", "FREQUENCY": "539142857"}},
		{Name: "asahi2", Tuning: map[string]string{"SERVICE_ID": "1065", "FREQUENCY": "539142857"}},
		{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024", "FREQUENCY": "557142857"}},
	}}
	r.ChannelNames = []string{"asahi", "asahi2", "nhk"}

	n, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	runs, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := 0
	for _, ln := range strings.Split(strings.TrimSpace(string(runs)), "\n") {
		if ln != "" {
			lines++
		}
	}
	if lines != 2 {
		t.Fatalf("dvbr ran %d times, want 2 (one per mux):\n%s", lines, runs)
	}
	if strings.Contains(string(runs), "asahi2") {
		t.Fatalf("asahi2 shares 539142857 with asahi and should have been skipped:\n%s", runs)
	}
	// Sanity: the pass still ingested, once per mux.
	if n != 2 {
		t.Fatalf("ingested %d events, want 2 (one per mux)", n)
	}
}

// A channel with no FREQUENCY must still be attempted — being unable to
// group it is not a reason to silently drop it from the pass.
func TestRefresher_ChannelWithoutFrequencyStillRuns(t *testing.T) {
	r, _ := newRefresher(t, oneEventJSON, nil)
	r.Tuners = nil
	r.Channels = testChannels() // no FREQUENCY keys at all
	r.ChannelNames = []string{"mx", "nhk"}

	n, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ingested %d events, want one per channel", n)
	}
}

// A normal pass reserves the adapter, runs dvbr, ingests, and gives the
// adapter back — so a live claim right after does not have to preempt.
func TestRefresher_ReservesAndReleases(t *testing.T) {
	pool := tuner.NewPool(holdTuner{}, testChannels(), config.ISDBTAdapters(0), 4)
	r, st := newRefresher(t, oneEventJSON, pool)

	n, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("ingested %d events, want 1", n)
	}
	if st := pool.Status(); st[0].Reserved {
		t.Fatalf("adapter still reserved after the pass: %+v", st)
	}

	events, err := st.EPGBetween(context.Background(), 23608,
		time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Title != "Fake Show" {
		t.Fatalf("events = %+v", events)
	}
}

// The regression this change is about: a long EPG scan must yield to a
// live claim instead of blocking it on dvbr's flock. The child is
// killed, the pass reports ErrPreempted, and the adapter is handed over.
func TestRefresher_YieldsToLiveClaim(t *testing.T) {
	// `exec` so the script process *is* the long-running one, matching
	// dvbr (a single process that dies when killed). The leaked-pipe
	// variant is covered below.
	testYieldsToLiveClaim(t, "exec sleep 30")
}

// Same yield, but the child leaves a grandchild holding the inherited
// stdout pipe. cmd.Wait would block on that pipe forever; WaitDelay is
// what keeps a leaky child from wedging the adapter.
func TestRefresher_YieldsWhenChildLeaksPipe(t *testing.T) {
	testYieldsToLiveClaim(t, "sleep 30")
}

func testYieldsToLiveClaim(t *testing.T, script string) {
	t.Helper()
	pool := tuner.NewPool(holdTuner{}, testChannels(), config.ISDBTAdapters(0), 4)
	r, _ := newRefresher(t, script, pool)

	done := make(chan error, 1)
	go func() {
		_, err := r.RefreshOnce(context.Background())
		done <- err
	}()

	// Wait until the scan actually holds the adapter.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if st := pool.Status(); len(st) > 0 && st[0].Reserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("EPG never reserved the adapter")
		}
		time.Sleep(10 * time.Millisecond)
	}

	lease, err := pool.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("live Acquire should have preempted the EPG scan: %v", err)
	}
	defer lease.Release()

	select {
	case err := <-done:
		if !errors.Is(err, ErrPreempted) {
			t.Fatalf("RefreshOnce err = %v, want ErrPreempted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EPG pass never returned after being preempted")
	}

	if st := pool.Status(); st[0].Channel != "mx" {
		t.Fatalf("adapter should now be on mx: %+v", st)
	}
}

// When the tuner is occupied by something EPG can't preempt, the pass
// reports it as a yield rather than a hard failure — nothing is broken,
// EPG just doesn't get a turn.
func TestRefresher_BusyTunerReportsPreempted(t *testing.T) {
	pool := tuner.NewPool(holdTuner{}, testChannels(), config.ISDBTAdapters(0), 4)
	lease, err := pool.AcquireAt(context.Background(), "mx", tuner.PrioRecord)
	if err != nil {
		t.Fatalf("AcquireAt: %v", err)
	}
	defer lease.Release()

	r, _ := newRefresher(t, oneEventJSON, pool)
	if _, err := r.RefreshOnce(context.Background()); !errors.Is(err, ErrPreempted) {
		t.Fatalf("err = %v, want ErrPreempted", err)
	}
}

// A failing dvbr is a per-channel warning, not a yield: the pass
// continues and the adapter is still returned.
func TestRefresher_ChildFailureReleasesAdapter(t *testing.T) {
	pool := tuner.NewPool(holdTuner{}, testChannels(), config.ISDBTAdapters(0), 4)
	r, _ := newRefresher(t, "echo boom >&2; exit 3", pool)
	r.ChannelNames = []string{"mx", "nhk"}

	n, err := r.RefreshOnce(context.Background())
	if err != nil {
		t.Fatalf("RefreshOnce should tolerate child failures, got %v", err)
	}
	if n != 0 {
		t.Fatalf("ingested %d, want 0", n)
	}
	if st := pool.Status(); st[0].Reserved {
		t.Fatalf("adapter leaked: %+v", st)
	}
}

// Without a Reserver the old direct-spawn path still works (one-shot
// tools, tests) — it just has no arbitration.
func TestRefresher_NoPoolStillRuns(t *testing.T) {
	r, _ := newRefresher(t, oneEventJSON, nil)
	r.Tuners = nil
	if n, err := r.RefreshOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
