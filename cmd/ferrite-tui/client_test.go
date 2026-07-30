package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func testServer(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestChannelsAndStatusDecode(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/channels":
			io.WriteString(w, `[{"name":"asahi","aliases":["テレビ朝日"],"service_id":1064}]`)
		case "/api/status":
			io.WriteString(w, `{"version":"dev","uptime":"1m",
              "adapters":[{"adapter":0,"channel":"TBS1","refs":2,"prio":"record"}],
              "recording":[7]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	chans, err := c.Channels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(chans) != 1 || chans[0].Name != "asahi" || chans[0].ServiceID != 1064 {
		t.Fatalf("channels = %+v", chans)
	}
	if chans[0].Aliases[0] != "テレビ朝日" {
		t.Fatalf("alias = %q", chans[0].Aliases[0])
	}

	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.LiveChannel() != "TBS1" {
		t.Fatalf("LiveChannel = %q", st.LiveChannel())
	}
	if len(st.Recording) != 1 || st.Recording[0] != 7 {
		t.Fatalf("recording = %v", st.Recording)
	}
	if st.Adapters[0].Prio != "record" {
		t.Fatalf("prio = %q", st.Adapters[0].Prio)
	}
}

// Channel names contain spaces and Japanese, so every path segment has to
// be escaped or the daemon sees a different channel (or a 404).
func TestChannelNamesAreEscapedInPaths(t *testing.T) {
	var gotPath string
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		io.WriteString(w, `{"channel":"TOKYO MX1","playlist":"/api/live/TOKYO%20MX1.m3u8"}`)
	})

	if _, err := c.Switch(context.Background(), "TOKYO MX1"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/live/TOKYO%20MX1/switch" {
		t.Fatalf("path = %q", gotPath)
	}

	// And the URL handed to a player must be absolute, not the relative
	// playlist path the daemon reports — mpv runs on this machine.
	want := c.BaseURL + "/api/live/%E3%83%86%E3%83%AC%E3%83%93%E6%9C%9D%E6%97%A5.m3u8"
	if got := c.PlaylistURL("テレビ朝日"); got != want {
		t.Fatalf("PlaylistURL = %q, want %q", got, want)
	}
}

// A busy tuner is the one error the remote must explain, so it has to
// survive as a typed error with the daemon's own message.
func TestBusyTunerSurfacesAsTypedError(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"error":"tuner busy: every adapter is held by a recording"}`)
	})

	_, err := c.Record(context.Background(), "asahi", "", 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if !apiErr.Busy() {
		t.Fatalf("status %d should read as busy", apiErr.Status)
	}
	if apiErr.Error() != "tuner busy: every adapter is held by a recording" {
		t.Fatalf("message = %q", apiErr.Error())
	}
}

func TestNowReturnsNilForEmptyGuide(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `null`)
	})
	ev, err := c.Now(context.Background(), 1064)
	if err != nil {
		t.Fatalf("an empty guide is not an error: %v", err)
	}
	if ev != nil {
		t.Fatalf("event = %+v, want nil", ev)
	}
}

func TestScheduleSendsWindowAndDecodesEvents(t *testing.T) {
	var q url.Values
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		io.WriteString(w, `[{"service_id":1064,"event_id":1,
          "start":"2026-07-30T11:00:00Z","duration_s":1800,"title":"幼女戦記Ⅱ"}]`)
	})

	from := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	to := from.Add(2 * time.Hour)
	events, err := c.Schedule(context.Background(), 1064, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("service") != "1064" {
		t.Fatalf("service = %q", q.Get("service"))
	}
	if q.Get("from") != from.Format(time.RFC3339) {
		t.Fatalf("from = %q", q.Get("from"))
	}
	if len(events) != 1 || events[0].Title != "幼女戦記Ⅱ" {
		t.Fatalf("events = %+v", events)
	}
	if got := events[0].End(); !got.Equal(from.Add(90 * time.Minute)) {
		t.Fatalf("End = %v", got)
	}
	if !events[0].Airing(from.Add(75 * time.Minute)) {
		t.Fatal("event should be airing 15 minutes in")
	}
	if events[0].Airing(events[0].End()) {
		t.Fatal("an event must not be airing at its own end")
	}
}

func TestRecordBodyOmitsEmptyFields(t *testing.T) {
	var body map[string]any
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":3,"channel":"asahi","title":""}`)
	})

	res, err := c.Record(context.Background(), "asahi", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ID != 3 {
		t.Fatalf("id = %d", res.ID)
	}
	if _, ok := body["title"]; ok {
		t.Fatalf("empty title should be omitted, body = %v", body)
	}
	if _, ok := body["duration_s"]; ok {
		t.Fatalf("open-ended recording must not send duration_s, body = %v", body)
	}
}

func TestStopRecordingAcceptsNoContent(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/record/9/stop" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	if err := c.StopRecording(context.Background(), 9); err != nil {
		t.Fatalf("204 should not be an error: %v", err)
	}
}

func TestDeleteRecordingReportsWhetherAFileWent(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/recordings/12" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"id":12,"file_deleted":true}`)
	})
	fileDeleted, err := c.DeleteRecording(context.Background(), 12)
	if err != nil {
		t.Fatal(err)
	}
	if !fileDeleted {
		t.Fatal("file_deleted was not decoded")
	}
}

// The player runs here and the file is on the daemon, so this URL has to be
// absolute — the same trap PlaylistURL exists for.
func TestRecordingFileURLIsAbsolute(t *testing.T) {
	c := NewClient("http://tuner.lan:8010/")
	if got := c.RecordingFileURL(7); got != "http://tuner.lan:8010/api/recordings/7/file" {
		t.Fatalf("RecordingFileURL = %q", got)
	}
}

// A daemon that answers with plain text (a proxy error page, say) must not
// produce an empty, unactionable message.
func TestNonJSONErrorBodyIsKept(t *testing.T) {
	c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "upstream is down\n")
	})
	_, err := c.Status(context.Background())
	if err == nil || err.Error() != "upstream is down" {
		t.Fatalf("err = %v", err)
	}
}
