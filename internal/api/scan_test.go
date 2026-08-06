package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/scan"
)

// Without a scanner configured the endpoint has to say so rather than
// 404, so the UI can hide the button instead of offering one that fails.
func TestScan_UnavailableWithoutAScanner(t *testing.T) {
	r := NewRouter(Deps{})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/scan", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /api/scan = %d, want 503", w.Code)
	}

	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/scan", nil))
	var st struct{ Available, Running bool }
	if err := json.NewDecoder(w.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Available {
		t.Error("no scanner configured, but the status says one is available")
	}
}

// The sweep streams progress and, when it finishes, the daemon's own
// channel list reflects what was written — a scan whose results only
// arrive after a restart is not much of a feature.
func TestScan_StreamsProgressAndReloadsTheChannelList(t *testing.T) {
	dir := t.TempDir()
	channelsFile := filepath.Join(dir, "channels.json")
	if err := os.WriteFile(channelsFile, []byte(`{"version":1,"channels":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	channels, err := config.LoadChannels(channelsFile)
	if err != nil {
		t.Fatal(err)
	}

	// A stand-in for `dvb-rs scan` that adds one record per invocation.
	stub := filepath.Join(dir, "dvb-rs")
	script := `#!/bin/sh
out="channels.json"
freq=0
while [ $# -gt 0 ]; do
  case "$1" in
    --output) out="$2"; shift 2 ;;
    --frequency) freq="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"version":1,"channels":[{"name":"svc-%s","tuning":{"SERVICE_ID":"1","FREQUENCY":"%s"}}]}' "$freq" "$freq" > "$out"
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	d := Deps{
		Channels:     channels,
		ChannelsFile: channelsFile,
		Scanner: &scan.Runner{
			DvbrBin:      stub,
			ChannelsFile: channelsFile,
			First:        13, Last: 13,
		},
	}
	w := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/scan", nil))

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q, want text/event-stream", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "data: ") {
		t.Fatalf("no SSE frames in the response:\n%s", body)
	}
	if !strings.Contains(body, `"finished":true`) {
		t.Errorf("the stream should end with a finished event:\n%s", body)
	}

	// The subprocess rewrote the file; the in-memory list must have been
	// swapped in before the response ended.
	if ch := channels.Find("svc-473142857"); ch == nil {
		t.Fatalf("the scanned channel is not selectable without a restart (%d records)",
			channels.Len())
	}
}
