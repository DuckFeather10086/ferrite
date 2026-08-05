package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// Runs only when FERRITE_LIVE points at a daemon: proves the wire types in
// client.go match what the daemon actually emits.
func TestLiveWireTypes(t *testing.T) {
	base := os.Getenv("FERRITE_LIVE")
	if base == "" {
		t.Skip("set FERRITE_LIVE=http://host:port to run")
	}
	c := NewClient(base)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	chans, err := c.Channels(ctx)
	if err != nil || len(chans) == 0 {
		t.Fatalf("channels: %v (%d)", err, len(chans))
	}
	t.Logf("channels: %d, first = %q sid=%d aliases=%v",
		len(chans), chans[0].Name, chans[0].ServiceID, chans[0].Aliases)

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version == "" || len(st.Adapters) == 0 {
		t.Fatalf("status decoded empty: %+v", st)
	}
	t.Logf("status: version=%s adapters=%+v recording=%v", st.Version, st.Adapters, st.Recording)

	// A channel that has guide data, to exercise Event decoding.
	var withGuide int
	for _, ch := range chans {
		ev, err := c.Now(ctx, ch.ServiceID)
		if err != nil {
			t.Fatalf("now(%d): %v", ch.ServiceID, err)
		}
		if ev != nil {
			withGuide++
			if ev.Title == "" || ev.Start.IsZero() || ev.DurationS == 0 {
				t.Fatalf("event decoded with holes: %+v", ev)
			}
			if withGuide == 1 {
				t.Logf("now on %s: %q %s (%ds)", ch.Name, ev.Title,
					ev.Start.Local().Format("15:04"), ev.DurationS)
			}
		}
	}
	if withGuide == 0 {
		t.Log("no channel had now-playing data (empty EPG?)")
	}

	recs, err := c.Recordings(ctx)
	if err != nil {
		t.Fatalf("recordings: %v", err)
	}
	t.Logf("recordings: %d", len(recs))

	// The file endpoint, without downloading a whole recording: one Range
	// request proves the URL resolves and that seeking will work, which is
	// what playback from the TUI depends on.
	for _, rec := range recs {
		if rec.State != "done" || rec.SizeBytes == nil || *rec.SizeBytes == 0 {
			continue
		}
		url := c.RecordingFileURL(rec.ID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-187")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("%s: %d, want 206 (Range unsupported?)", url, resp.StatusCode)
		}
		if len(body) != 188 || body[0] != 0x47 {
			t.Fatalf("%s: %d bytes, first = %#x, want 188 starting with a TS sync byte",
				url, len(body), body[0])
		}
		t.Logf("file endpoint ok: recording %d, %d bytes on disk", rec.ID, *rec.SizeBytes)
		break
	}
}
