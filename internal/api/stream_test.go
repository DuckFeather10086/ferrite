package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path"
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

// walkPlaylists follows every URI a player would follow, from `at` down to the
// segments, and returns what the leaves served. Relative URIs are resolved the
// way a player resolves them: against the URL the playlist came from.
func walkPlaylists(t *testing.T, h http.Handler, at string, depth int) []string {
	t.Helper()
	rr := get(t, h, at)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", at, rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/vnd.apple.mpegurl" {
		t.Fatalf("GET %s: Content-Type = %q", at, ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "#EXTM3U") {
		t.Fatalf("GET %s is not a playlist:\n%s", at, body)
	}
	if depth == 0 {
		t.Fatalf("playlists nested deeper than a player follows, at %s", at)
	}

	var leaves []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		next := path.Join(path.Dir(at), line)
		if strings.HasSuffix(line, ".m3u8") {
			leaves = append(leaves, walkPlaylists(t, h, next, depth-1)...)
			continue
		}
		seg := get(t, h, next)
		if seg.Code != http.StatusOK {
			t.Fatalf("GET %s = %d (URI %q from %s does not resolve)",
				next, seg.Code, line, at)
		}
		leaves = append(leaves, seg.Body.String())
	}
	return leaves
}

// The whole point of the single URL: hand /stream.m3u8 to a player and
// everything it asks for has to resolve — the video rendition and, through it,
// the segments. ffmpeg writes the segment URIs relative to /api/live/, so
// nothing here can be served unchanged from the root: from there they would
// resolve to /{channel}/streamN.ts, which is not a route (in production the SPA
// fallback answers HTML, which a player reports as a corrupt stream).
func TestStreamM3U8IsPlayableFromTheRoot(t *testing.T) {
	h, _ := newStreamRouter(t)

	if rr := post(t, h, "/api/live/mx/switch", ""); rr.Code != http.StatusOK {
		t.Fatalf("switch: %d %s", rr.Code, rr.Body.String())
	}

	segments := walkPlaylists(t, h, StreamPath, 3)
	if len(segments) == 0 {
		t.Fatal("no segments reachable from " + StreamPath)
	}
	for _, got := range segments {
		if got != "segment-bytes" {
			t.Fatalf("segment served %q", got)
		}
	}
}

// The per-channel URL is the same manifest from a different place: its URIs
// reach /api/live/ from inside it rather than from the root.
func TestPerChannelPlaylistIsPlayableToo(t *testing.T) {
	h, _ := newStreamRouter(t)

	segments := walkPlaylists(t, h, "/api/live/mx.m3u8", 3)
	if len(segments) == 0 {
		t.Fatal("no segments reachable from the per-channel playlist")
	}
}

// A subtitle rendition is announced only when one exists. A manifest naming a
// playlist that 404s makes some players abandon the stream altogether.
func TestMasterAnnouncesCaptionsOnlyWhenPresent(t *testing.T) {
	h, mgr := newStreamRouter(t)

	rr := get(t, h, "/api/live/mx.m3u8")
	if strings.Contains(rr.Body.String(), "SUBTITLES") {
		t.Fatalf("captions announced with no rendition:\n%s", rr.Body.String())
	}

	// What internal/caption writes once it has cues.
	s := mgr.Touch("mx")
	if s == nil {
		t.Fatal("no session")
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "subs.m3u8"),
		[]byte("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2.0,\nsub0.vtt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "sub0.vtt"),
		[]byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nこんにちは\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rr = get(t, h, "/api/live/mx.m3u8")
	body := rr.Body.String()
	if !strings.Contains(body, `TYPE=SUBTITLES`) || !strings.Contains(body, `SUBTITLES="subs"`) {
		t.Fatalf("captions not announced:\n%s", body)
	}
	// And the rendition resolves from the manifest that named it.
	if sub := get(t, h, "/api/live/mx/subs.m3u8"); sub.Code != http.StatusOK {
		t.Fatalf("subtitle playlist = %d", sub.Code)
	}
	if vtt := get(t, h, "/api/live/mx/sub0.vtt"); vtt.Code != http.StatusOK ||
		vtt.Header().Get("Content-Type") != "text/vtt; charset=utf-8" {
		t.Fatalf("vtt segment = %d %q", vtt.Code, vtt.Header().Get("Content-Type"))
	}
}

// The media playlist keeps ffmpeg's own base off: served from inside the
// channel's path, its segment URIs are bare names.
func TestVideoPlaylistServesBareSegmentNames(t *testing.T) {
	h, _ := newStreamRouter(t)

	// The master is what tunes; this endpoint only serves a session that
	// exists, so that a player left polling it cannot re-tune a channel.
	if rr := get(t, h, "/api/live/mx.m3u8"); rr.Code != http.StatusOK {
		t.Fatalf("master: %d %s", rr.Code, rr.Body.String())
	}

	rr := get(t, h, "/api/live/mx/video.m3u8")
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "\nstream0.ts") {
		t.Fatalf("segment URI still carries ffmpeg's base:\n%s", rr.Body.String())
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

func TestStripSegmentBase(t *testing.T) {
	in := "#EXTM3U\n#EXTINF:2.0,\nmx/stream0.ts\n#EXTINF:2.0,\n/already/absolute.ts\n" +
		"#EXTINF:2.0,\nhttp://elsewhere/seg.ts\n"
	want := "#EXTM3U\n#EXTINF:2.0,\nstream0.ts\n#EXTINF:2.0,\n/already/absolute.ts\n" +
		"#EXTINF:2.0,\nhttp://elsewhere/seg.ts\n"
	if got := string(stripSegmentBase([]byte(in))); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
