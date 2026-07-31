package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/netaddr"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// writeFakeFFmpeg drops in a shell script that writes the playlist real
// ffmpeg would write — segment URIs relative to the playlist, prefixed with
// the channel directory (-hls_base_url) — plus one segment to fetch.
func writeFakeFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := `#!/bin/sh
for last; do :; done
outdir="$(dirname "$last")"
channel="$(basename "$outdir")"
mkdir -p "$outdir"
printf 'segment-bytes' > "$outdir/stream0.ts"
{
  echo '#EXTM3U'
  echo '#EXT-X-VERSION:3'
  echo '#EXT-X-TARGETDURATION:2'
  echo '#EXTINF:2.000000,'
  echo "$channel/stream0.ts"
} > "$last"
cat > /dev/null
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newStreamRouter(t *testing.T) (http.Handler, *hls.Manager) {
	t.Helper()
	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
		},
	}
	mgr := &hls.Manager{
		Tuners:     tuner.NewPool(fakeTuner{}, channels, []int{0}, 4),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeFakeFFmpeg(t),
	}
	t.Cleanup(func() { mgr.Close("mx") })
	h := NewRouter(Deps{
		Channels:  channels,
		HLS:       mgr,
		HTTPPort:  8010,
		StartedAt: time.Now(),
		Version:   "test",
	})
	return h, mgr
}

// The whole point of the single URL: hand /stream.m3u8 to a player and every
// segment it asks for has to resolve. ffmpeg writes the URIs relative to
// /api/live/, so serving that file unchanged from the root produces a
// playlist whose segments 404 (or, in production, come back as the SPA's
// HTML — which a player reports as a corrupt stream).
func TestStreamM3U8IsPlayableFromTheRoot(t *testing.T) {
	h, _ := newStreamRouter(t)

	if rr := post(t, h, "/api/live/mx/switch", ""); rr.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rr.Code, rr.Body.String())
	}

	rr := get(t, h, StreamPath)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", StreamPath, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("Content-Type = %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("not a playlist:\n%s", body)
	}

	var uris int
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		uris++
		// Resolved the way a player resolves it: relative to /stream.m3u8.
		seg := get(t, h, "/"+line)
		if seg.Code != http.StatusOK {
			t.Fatalf("GET /%s = %d (segment URI does not resolve from the root)",
				line, seg.Code)
		}
		if got := seg.Body.String(); got != "segment-bytes" {
			t.Fatalf("GET /%s served %q", line, got)
		}
	}
	if uris == 0 {
		t.Fatalf("playlist has no segment URIs:\n%s", body)
	}
}

// The per-channel playlist keeps its own contract: its URIs are relative to
// /api/live/ and must not be rewritten.
func TestPerChannelPlaylistKeepsItsOwnBase(t *testing.T) {
	h, _ := newStreamRouter(t)

	rr := get(t, h, "/api/live/mx.m3u8")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\nmx/stream0.ts") {
		t.Fatalf("segment URI was rewritten:\n%s", rr.Body.String())
	}
	// And it resolves against that playlist's URL.
	if seg := get(t, h, "/api/live/mx/stream0.ts"); seg.Code != http.StatusOK {
		t.Fatalf("GET segment = %d", seg.Code)
	}
}

func TestStreamM3U8WithNothingTuned(t *testing.T) {
	h, _ := newStreamRouter(t)
	rr := get(t, h, StreamPath)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("%d %s, want 404", rr.Code, rr.Body.String())
	}
}

// /api/status is where a remote learns where to send a viewer. It has to
// come from the daemon: a TUI on a laptop enumerating its own interfaces
// would report addresses that have nothing to do with the tuner box.
func TestStatusReportsWhereToWatch(t *testing.T) {
	h, _ := newStreamRouter(t)
	rr := get(t, h, "/api/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d", rr.Code)
	}
	var out struct {
		Stream    string            `json:"stream"`
		Addresses []netaddr.Address `json:"addresses"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Stream != StreamPath {
		t.Fatalf("stream = %q, want %q", out.Stream, StreamPath)
	}
	if len(out.Addresses) == 0 || out.Addresses[0].Kind != netaddr.Local {
		t.Fatalf("addresses = %+v, want localhost first", out.Addresses)
	}
	if got := out.Addresses[0].URL(out.Stream); got != "http://localhost:8010/stream.m3u8" {
		t.Fatalf("first watch URL = %q", got)
	}
}

// A daemon that wasn't told its port says nothing rather than guessing.
func TestStatusOmitsAddressesWithoutAPort(t *testing.T) {
	h, _ := newTestRouter(t, false)
	rr := get(t, h, "/api/status")
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["addresses"]; ok {
		t.Fatalf("addresses reported without a port: %v", out["addresses"])
	}
}

func TestRebaseSegments(t *testing.T) {
	in := "#EXTM3U\n#EXTINF:2.0,\nmx/stream0.ts\n#EXTINF:2.0,\n/already/absolute.ts\n" +
		"#EXTINF:2.0,\nhttp://elsewhere/seg.ts\n"
	want := "#EXTM3U\n#EXTINF:2.0,\napi/live/mx/stream0.ts\n#EXTINF:2.0,\n/already/absolute.ts\n" +
		"#EXTINF:2.0,\nhttp://elsewhere/seg.ts\n"
	if got := string(rebaseSegments([]byte(in))); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
