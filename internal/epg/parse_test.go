package epg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/store"
)

func TestParse_AsahiFixture(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "asahi.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	events, skipped := Parse(f, 23608)
	if len(events) == 0 {
		t.Fatal("no events parsed")
	}
	t.Logf("parsed %d events, skipped %d", len(events), len(skipped))

	first := events[0]
	if first.ServiceID != 23608 {
		t.Fatalf("ServiceID not propagated: %d", first.ServiceID)
	}
	if first.Title == "" {
		t.Fatalf("empty title on first event")
	}
	if first.Duration == 0 {
		t.Fatalf("zero duration on first event")
	}
	// First event of TV Asahi morning is in 2026-05-13 JST → UTC
	// should be 2026-05-13 01:40:00.
	if first.Start.Year() != 2026 || first.Start.Location() != time.UTC {
		t.Fatalf("Start tz/year wrong: %v", first.Start)
	}
}

// One pass covers the whole mux, so events name their own service and must
// be filed under it. Stamping them all with the channel that was asked for
// is how every sibling channel ended up with an empty guide — or worse,
// with another channel's programmes.
func TestParse_KeepsEachEventsOwnService(t *testing.T) {
	body := `[
		{"service_id":1064,"event_id":1,"start":"2026-05-23 10:00:00","duration":"00:30:00","title":"asahi prog"},
		{"service_id":1065,"event_id":1,"start":"2026-05-23 10:00:00","duration":"00:30:00","title":"subchannel prog"},
		{"event_id":9,"start":"2026-05-23 11:00:00","duration":"00:30:00","title":"no service_id"}
	]`
	events, skipped := Parse(strings.NewReader(body), 1064)
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %v", skipped)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}

	// event_id 1 appears once per service: two different programmes that
	// must not collapse into one. The last event names no service and
	// belongs to the channel the pass ran for — that is what an older dvbr
	// emitted for every event.
	type row struct {
		sid   uint16
		title string
	}
	got := map[row]bool{}
	for _, e := range events {
		got[row{e.ServiceID, e.Title}] = true
	}
	for _, want := range []row{
		{1064, "asahi prog"},
		{1065, "subchannel prog"},
		{1064, "no service_id"},
	} {
		if !got[want] {
			t.Errorf("missing %d → %q; got %v", want.sid, want.title, got)
		}
	}
}

func TestParse_BadJSON(t *testing.T) {
	_, errs := Parse(strings.NewReader("not json"), 1)
	if len(errs) == 0 {
		t.Fatal("expected error")
	}
}

func TestParse_SkipsEmptyStartDuration(t *testing.T) {
	body := `[
		{"event_id":1,"start":"","duration":"","title":"skipme"},
		{"event_id":2,"start":"2026-05-23 10:00:00","duration":"00:30:00","title":"keep","text":"x"}
	]`
	events, skipped := Parse(strings.NewReader(body), 7)
	if len(events) != 1 || events[0].EventID != 2 {
		t.Fatalf("got %v", events)
	}
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip, got %d", len(skipped))
	}
}

func TestParse_JSTConversion(t *testing.T) {
	body := `[{"event_id":1,"start":"2026-05-23 10:00:00","duration":"01:00:00","title":"t"}]`
	events, _ := Parse(strings.NewReader(body), 1)
	if len(events) != 1 {
		t.Fatal("parse failed")
	}
	want := time.Date(2026, 5, 23, 1, 0, 0, 0, time.UTC)
	if !events[0].Start.Equal(want) {
		t.Fatalf("JST->UTC wrong: got %v want %v", events[0].Start, want)
	}
}

func TestParse_RoundTripsThroughStore(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "asahi.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	events, _ := Parse(f, 23608)

	s, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.UpsertEPGEvents(ctx, events); err != nil {
		t.Fatal(err)
	}

	// Query a wide window that should encompass everything in the
	// fixture.
	from := events[0].Start.Add(-24 * time.Hour)
	to := events[len(events)-1].Start.Add(24 * time.Hour)
	got, err := s.EPGBetween(ctx, 23608, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(events) {
		t.Fatalf("round-trip lost events: stored %d, got %d back", len(events), len(got))
	}
}
