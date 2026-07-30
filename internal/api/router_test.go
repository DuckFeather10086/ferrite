package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/store"
)

func newTestRouter(t *testing.T, withStore bool) (http.Handler, *store.Store) {
	t.Helper()
	deps := Deps{
		Channels: &config.Channels{
			Version: 1,
			Channels: []config.Channel{
				{Name: "mx", Aliases: []string{"TOKYO MX1"},
					Tuning: map[string]string{"SERVICE_ID": "23608"}},
			},
		},
		StartedAt: time.Now(),
		Version:   "test",
	}
	var s *store.Store
	if withStore {
		path := filepath.Join(t.TempDir(), "x.db")
		var err error
		s, err = store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		deps.Store = s
	}
	return NewRouter(deps), s
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealth(t *testing.T) {
	h, _ := newTestRouter(t, false)
	rr := get(t, h, "/health")
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}

func TestStatus(t *testing.T) {
	h, _ := newTestRouter(t, false)
	rr := get(t, h, "/api/status")
	if rr.Code != 200 {
		t.Fatalf("%d", rr.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["version"] != "test" {
		t.Fatalf("got %v", out)
	}
}

func TestChannels(t *testing.T) {
	h, _ := newTestRouter(t, false)
	rr := get(t, h, "/api/channels")
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0]["name"] != "mx" {
		t.Fatalf("got %v", out)
	}
	if int(out[0]["service_id"].(float64)) != 23608 {
		t.Fatalf("service_id: %v", out[0]["service_id"])
	}
	// display_name is always present, so a client can render it without
	// re-deriving a label from the alias list (and disagreeing with the
	// other clients when it does).
	if out[0]["display_name"] != "mx" {
		t.Fatalf("display_name = %v, want the name when no alias reads better",
			out[0]["display_name"])
	}
}

// The label a UI shows is chosen server-side. A record whose own name is
// legacy mojibake must come back with the readable alias.
func TestChannels_DisplayNamePrefersTheReadableAlias(t *testing.T) {
	h := NewRouter(Deps{
		Channels: &config.Channels{Version: 1, Channels: []config.Channel{
			{Name: "NHKEFl1El5~", Aliases: []string{"NHKEテレ1東京"}},
			{Name: "asahi", Aliases: []string{"|ÆìÓD+F|", "テレビ朝日"}},
			{Name: "J：COMテレビ", Aliases: []string{"J!'COM|ÆìÓ"}},
		}},
		StartedAt: time.Now(),
	})
	var out []struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
	}
	if err := json.Unmarshal(get(t, h, "/api/channels").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"NHKEFl1El5~": "NHKEテレ1東京",
		"asahi":       "テレビ朝日",
		"J：COMテレビ":    "J：COMテレビ", // its own name already reads fine
	}
	for _, row := range out {
		if got := want[row.Name]; row.DisplayName != got {
			t.Errorf("%q → display_name %q, want %q", row.Name, row.DisplayName, got)
		}
	}
}

func TestEPG_NoStoreReturns503(t *testing.T) {
	h, _ := newTestRouter(t, false)
	rr := get(t, h, "/api/epg?service=23608")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestEPG_WindowQuery(t *testing.T) {
	h, s := newTestRouter(t, true)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := s.UpsertEPGEvents(ctx, []store.EPGEvent{
		{ServiceID: 23608, EventID: 1, Start: t0, Duration: time.Hour, Title: "Now"},
	}); err != nil {
		t.Fatal(err)
	}
	from := t0.Add(-time.Minute).Format(time.RFC3339)
	to := t0.Add(2 * time.Hour).Format(time.RFC3339)
	rr := get(t, h, "/api/epg?service=23608&from="+from+"&to="+to)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Now") {
		t.Fatalf("body missing event: %s", rr.Body.String())
	}
}

func TestNow_RequiresService(t *testing.T) {
	h, _ := newTestRouter(t, true)
	rr := get(t, h, "/api/now")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestSchedules_CreateAndList(t *testing.T) {
	h, _ := newTestRouter(t, true)
	start := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	end := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"channel":"mx","service_id":23608,"start":"` + start + `","end":"` + end + `"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/schedule",
		strings.NewReader(body)))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}

	listRR := get(t, h, "/api/schedule")
	if listRR.Code != 200 {
		t.Fatalf("list: %d", listRR.Code)
	}
	if !strings.Contains(listRR.Body.String(), `"channel":"mx"`) {
		t.Fatalf("body: %s", listRR.Body.String())
	}
}

func TestSchedules_BadJSON(t *testing.T) {
	h, _ := newTestRouter(t, true)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/schedule",
		strings.NewReader(`not json`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestWebUI_ServesAndFallsBack(t *testing.T) {
	web := fstest.MapFS{
		"index.html": {Data: []byte("<html>SPA-SHELL</html>")},
		"app.js":     {Data: []byte("console.log('app')")},
	}
	h := NewRouter(Deps{
		Channels:  &config.Channels{},
		StartedAt: time.Now(),
		Web:       web,
	})

	// Root serves index.html.
	if rr := get(t, h, "/"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "SPA-SHELL") {
		t.Fatalf("root: %d %s", rr.Code, rr.Body.String())
	}
	// Real asset is served verbatim.
	if rr := get(t, h, "/app.js"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "console.log") {
		t.Fatalf("asset: %d %s", rr.Code, rr.Body.String())
	}
	// Unknown non-API path falls back to the SPA shell (client routing).
	if rr := get(t, h, "/guide/123"); rr.Code != 200 || !strings.Contains(rr.Body.String(), "SPA-SHELL") {
		t.Fatalf("spa fallback: %d %s", rr.Code, rr.Body.String())
	}
	// Unknown API path stays a JSON 404, not the HTML shell.
	rr := get(t, h, "/api/nonsense")
	if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "SPA-SHELL") {
		t.Fatalf("api 404: %d %s", rr.Code, rr.Body.String())
	}
	if !json.Valid(rr.Body.Bytes()) {
		t.Fatalf("api 404 not JSON: %s", rr.Body.String())
	}
}

func TestRecordings_Empty(t *testing.T) {
	h, _ := newTestRouter(t, true)
	rr := get(t, h, "/api/recordings")
	if rr.Code != 200 {
		t.Fatalf("%d", rr.Code)
	}
	// Just verify it's valid JSON; can be null or [].
	if !json.Valid(rr.Body.Bytes()) {
		t.Fatalf("invalid JSON: %s", rr.Body.String())
	}
}
