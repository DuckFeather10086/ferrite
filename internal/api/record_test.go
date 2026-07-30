package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/recorder"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// ── harness ────────────────────────────────────────────────────────

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	h.ServeHTTP(rr, req)
	return rr
}

// fakeTuner stands in for dvbr: emits a little TS then holds until torn
// down, so a recording gets real bytes without hardware.
type fakeTuner struct{}

func (fakeTuner) Tune(ctx context.Context, _ int, _ string) (tuner.TsStream, error) {
	return &holdStream{ctx: ctx, body: make([]byte, 4096), done: make(chan struct{})}, nil
}

type holdStream struct {
	ctx  context.Context
	body []byte
	off  int
	done chan struct{}
	once sync.Once
}

func (h *holdStream) Read(p []byte) (int, error) {
	if h.off < len(h.body) {
		n := copy(p, h.body[h.off:])
		h.off += n
		return n, nil
	}
	select {
	case <-h.ctx.Done():
	case <-h.done:
	}
	return 0, io.EOF
}

func (h *holdStream) Close() error {
	h.once.Do(func() { close(h.done) })
	return nil
}

// newRecordRouter wires the full record-now path: channels → pool →
// runner → manager → HTTP.
func newRecordRouter(t *testing.T) (http.Handler, *store.Store, *recorder.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	channels := &config.Channels{
		Version: 1,
		Channels: []config.Channel{
			{Name: "mx", Aliases: []string{"TOKYO MX1"},
				Tuning: map[string]string{"SERVICE_ID": "23608"}},
			{Name: "nhk", Tuning: map[string]string{"SERVICE_ID": "1024"}},
		},
	}
	pool := tuner.NewPool(fakeTuner{}, channels, []int{0}, 4)
	mgr := &recorder.Manager{Runner: &recorder.Runner{
		Tuners:         pool,
		Store:          st,
		StorageRoot:    t.TempDir(),
		StartupTimeout: time.Second,
		StallTimeout:   time.Second,
	}}
	t.Cleanup(mgr.StopAll)

	h := NewRouter(Deps{
		Channels:  channels,
		Store:     st,
		Tuners:    pool,
		Recorder:  mgr,
		StartedAt: time.Now(),
		Version:   "test",
	})
	return h, st, mgr
}

// ── tests ──────────────────────────────────────────────────────────

func TestRecordNow_StartAndStop(t *testing.T) {
	h, st, _ := newRecordRouter(t)

	rr := post(t, h, "/api/record", `{"channel":"mx","title":"Manual"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID      int64  `json:"id"`
		Channel string `json:"channel"`
		Title   string `json:"title"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Channel != "mx" || created.Title != "Manual" {
		t.Fatalf("body = %+v", created)
	}

	// The daemon reports what's rolling, which is what a remote needs to
	// render a REC indicator.
	var status struct {
		Recording []int64 `json:"recording"`
	}
	if err := json.Unmarshal(get(t, h, "/api/status").Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Recording) != 1 || status.Recording[0] != created.ID {
		t.Fatalf("status.recording = %v, want [%d]", status.Recording, created.ID)
	}

	time.Sleep(100 * time.Millisecond)
	stop := post(t, h, "/api/record/"+strconv.FormatInt(created.ID, 10)+"/stop", "")
	if stop.Code != http.StatusNoContent {
		t.Fatalf("stop: %d %s", stop.Code, stop.Body.String())
	}

	// Row lands as done with bytes.
	deadline := time.Now().Add(3 * time.Second)
	for {
		recs, err := st.ListRecordings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(recs) == 1 && recs[0].State == store.RecordingStateDone {
			if !recs[0].SizeBytes.Valid || recs[0].SizeBytes.Int64 == 0 {
				t.Fatalf("size = %v, want non-zero", recs[0].SizeBytes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording never finished: %+v", recs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An alias must record the same channel, not a second one.
func TestRecordNow_ResolvesAlias(t *testing.T) {
	h, _, _ := newRecordRouter(t)
	rr := post(t, h, "/api/record", `{"channel":"TOKYO MX1"}`)
	if rr.Code != http.StatusCreated {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		Channel string `json:"channel"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.Channel != "mx" {
		t.Fatalf("channel = %q, want the canonical mx", created.Channel)
	}
}

func TestRecordNow_BadRequests(t *testing.T) {
	h, _, _ := newRecordRouter(t)

	if rr := post(t, h, "/api/record", `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing channel: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/record", `{"channel":"nope"}`); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown channel: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/record", `not json`); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad json: %d", rr.Code)
	}
}

// Recording a second channel while the only adapter is already
// recording must be refused up front, not accepted and failed later.
func TestRecordNow_BusyTunerIsRejected(t *testing.T) {
	h, _, _ := newRecordRouter(t)

	if rr := post(t, h, "/api/record", `{"channel":"mx"}`); rr.Code != http.StatusCreated {
		t.Fatalf("first: %d %s", rr.Code, rr.Body.String())
	}
	// Give the first job time to actually hold the adapter.
	time.Sleep(100 * time.Millisecond)

	rr := post(t, h, "/api/record", `{"channel":"nhk"}`)
	if rr.Code != http.StatusConflict {
		t.Fatalf("second: %d %s, want 409", rr.Code, rr.Body.String())
	}
	// Same channel is fine — it shares the tune.
	if rr := post(t, h, "/api/record", `{"channel":"mx"}`); rr.Code != http.StatusCreated {
		t.Fatalf("same channel: %d %s", rr.Code, rr.Body.String())
	}
}

func TestRecordStop_NotRecording(t *testing.T) {
	h, _, _ := newRecordRouter(t)
	if rr := post(t, h, "/api/record/999/stop", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/record/abc/stop", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

// Without a recorder the endpoints must say so rather than 404 into the
// SPA fallback.
func TestRecordEndpoints_NoRecorder(t *testing.T) {
	h, _ := newTestRouter(t, false)
	if rr := post(t, h, "/api/record", `{"channel":"mx"}`); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/record/1/stop", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

// The switch route validates its channel before it needs a running HLS
// manager, so a typo'd channel is a 404 rather than a misleading 503.
func TestLiveSwitch_Validation(t *testing.T) {
	h, _, _ := newRecordRouter(t) // HLS intentionally nil

	if rr := post(t, h, "/api/live/nope/switch", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown channel: %d %s", rr.Code, rr.Body.String())
	}
	if rr := post(t, h, "/api/live/mx/switch", ""); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("hls unavailable: %d %s", rr.Code, rr.Body.String())
	}
}

// Empty list endpoints must answer `[]`, never `null`.
//
// A nil slice is indistinguishable from an empty one in Go, so this is
// invisible from the server side — but every other client has to special-case
// null before iterating. The TS agent crashed on `null.map` for a channel with
// no guide data, which is why this is pinned.
func TestEmptyListsAreArraysNotNull(t *testing.T) {
	h, _, _ := newRecordRouter(t)

	for _, path := range []string{
		"/api/epg?service=1064",
		"/api/recordings",
		"/api/schedule",
		"/api/channels",
		"/api/av-offsets",
	} {
		rr := get(t, h, path)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rr.Code, rr.Body.String())
		}
		body := strings.TrimSpace(rr.Body.String())
		if body == "null" {
			t.Fatalf("%s returned null; a client iterating this crashes", path)
		}
		var list []any
		if err := json.Unmarshal([]byte(body), &list); err != nil {
			t.Fatalf("%s: not a JSON array: %s", path, body)
		}
		if list == nil {
			t.Fatalf("%s decoded to a nil slice: %s", path, body)
		}
	}
}
