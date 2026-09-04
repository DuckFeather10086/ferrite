package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// writeStalledFFmpeg never writes a playlist, so a switch through it stays in
// the wait-for-playlist loop until something else ends it.
func writeStalledFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A channel change that a later one overtakes has to answer at once and say so.
//
// It used to poll for a playlist its own ffmpeg would never write, for the full
// 45s, and then report "stream did not start" — so surfing channels produced
// one working picture and an error message per press behind it, each arriving
// three quarters of a minute after the viewer had moved on.
func TestSwitchAnsweredAtOnceWhenSuperseded(t *testing.T) {
	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
		},
	}
	mgr := &hls.Manager{
		Tuners:     tuner.NewPool(fakeTuner{}, channels, config.ISDBTAdapters(0), 4),
		OutputRoot: t.TempDir(),
		FFmpegBin:  writeStalledFFmpeg(t),
	}
	t.Cleanup(func() { mgr.Close("mx"); mgr.Close("nhk") })
	h := NewRouter(Deps{Channels: channels, HLS: mgr, StartedAt: time.Now()})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/live/mx/switch", nil))
		done <- rr
	}()

	// Let it get as far as holding the adapter with no playlist on disk.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mgr.Touch("mx", "") == nil {
		time.Sleep(5 * time.Millisecond)
	}
	if mgr.Touch("mx", "") == nil {
		t.Fatal("the first switch never opened a session")
	}

	mgr.CloseOthers("nhk")

	select {
	case rr := <-done:
		if rr.Code != http.StatusConflict {
			t.Fatalf("superseded switch answered %d, want 409: %s", rr.Code, rr.Body.String())
		}
		var body struct {
			Error      string `json:"error"`
			Superseded bool   `json:"superseded"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		// The flag, not the prose: it is what lets a client stay quiet about
		// a press the viewer themselves overtook, and tell it apart from a
		// tuner that is genuinely busy — which is also a 409.
		if !body.Superseded {
			t.Fatalf("response does not mark itself superseded: %s", rr.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a superseded switch is still waiting; it must not sit out its timeout")
	}
}
