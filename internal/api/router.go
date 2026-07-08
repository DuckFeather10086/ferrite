// Package api wires HTTP handlers onto a chi router.
//
// Endpoints implemented:
//   GET  /health                        liveness
//   GET  /api/status                    daemon info, adapter status
//   GET  /api/channels                  channels.json listing
//   GET  /api/epg?service=&from=&to=    EPG events in window (RFC3339)
//   GET  /api/now?service=              currently-airing event
//   GET  /api/schedule                  list schedules
//   POST /api/schedule                  create schedule
//   DELETE /api/schedule/{id}           cancel schedule
//   GET  /api/recordings                list recordings
//
// Future:
//   GET  /api/live/{channel}.m3u8       live HLS playlist
//   GET  /api/live/{channel}/{seg}.ts   live HLS segment
//   GET  /api/recordings/{id}/file      stream a recorded file
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// Deps is what the router needs from the rest of the daemon. Pass a
// value with the fields you have — tests can leave Store / Tuners nil
// and only exercise the static endpoints.
type Deps struct {
	Channels  *config.Channels
	Store     *store.Store
	Tuners    *tuner.Pool
	HLS       *hls.Manager
	StartedAt time.Time
	Version   string // build-time injected, optional
	// Web is the embedded static UI (e.g. web.FS()). When non-nil it is
	// served for all non-/api routes with an index.html SPA fallback;
	// nil disables UI serving (tests, headless deployments).
	Web fs.FS
}

// NewRouter returns an http.Handler with all endpoints wired.
func NewRouter(d Deps) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(slogRequestLogger)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	// /stream.m3u8 is a convenience shortcut that serves the most
	// recently opened/touched HLS session. Bookmark this in VLC/iPad
	// for one-tap live TV without browsing to the web UI.
	r.Get("/stream.m3u8", d.handleStreamM3U8)

	r.Route("/api", func(r chi.Router) {
		r.Get("/status", d.handleStatus)
		r.Get("/channels", d.handleChannels)
		r.Get("/epg", d.handleEPG)
		r.Get("/now", d.handleNow)

		r.Get("/schedule", d.handleListSchedules)
		r.Post("/schedule", d.handleCreateSchedule)
		r.Delete("/schedule/{id}", d.handleCancelSchedule)

		r.Get("/recordings", d.handleListRecordings)

		r.Get("/live/{channel}.m3u8", d.handleLivePlaylist)
		r.Get("/live/{channel}/{segment}", d.handleLiveSegment)
		r.Post("/live/{channel}/stop", d.handleLiveStop)
	})

	// Static web UI: everything not matched above falls through to the
	// embedded SPA. chi routes /health and /api/* to their handlers
	// first, so this only catches asset and client-route requests.
	if d.Web != nil {
		r.NotFound(staticHandler(d.Web))
	}

	return r
}

// staticHandler serves the embedded UI bundle. Real files are served
// as-is. For paths that don't match an exact file, it also tries the
// .html suffix (Next.js static export). Everything else falls back to
// index.html for client-side SPA routing. Unknown /api/* paths still
// get a JSON 404 rather than the SPA shell.
func staticHandler(fsys fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(fsys))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		// Never cache UI assets — they're embedded in the binary and
		// change across restarts.
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if _, err := fsys.Open(name); err != nil {
				// Try .html suffix for static-export routes (e.g. /guide → guide.html).
				if _, err2 := fsys.Open(name + ".html"); err2 == nil {
					r.URL.Path = "/" + name + ".html"
				} else {
					// Not a real asset → serve the SPA shell.
					r.URL.Path = "/"
				}
			}
		}
		fileServer.ServeHTTP(w, r)
	}
}

func (d Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"version": d.Version,
		"started": d.StartedAt.Format(time.RFC3339),
		"uptime":  time.Since(d.StartedAt).Round(time.Second).String(),
	}
	if d.Tuners != nil {
		resp["adapters"] = d.Tuners.Status()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleChannels(w http.ResponseWriter, r *http.Request) {
	if d.Channels == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type out struct {
		Name      string   `json:"name"`
		Aliases   []string `json:"aliases,omitempty"`
		ServiceID uint16   `json:"service_id"`
	}
	rows := make([]out, 0, len(d.Channels.Channels))
	for i := range d.Channels.Channels {
		c := &d.Channels.Channels[i]
		rows = append(rows, out{
			Name:      c.Name,
			Aliases:   c.Aliases,
			ServiceID: c.ServiceID(),
		})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (d Deps) handleEPG(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	serviceID, err := parseService(r.URL.Query().Get("service"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	from, to, err := parseWindow(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	events, err := d.Store.EPGBetween(r.Context(), serviceID, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (d Deps) handleNow(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	serviceID, err := parseService(r.URL.Query().Get("service"))
	if err != nil || serviceID == 0 {
		writeErr(w, http.StatusBadRequest, "service query parameter is required")
		return
	}
	e, err := d.Store.NowPlaying(r.Context(), serviceID, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if e == nil {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (d Deps) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	list, err := d.Store.ListSchedules(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

type scheduleCreateReq struct {
	Channel   string `json:"channel"`
	ServiceID uint16 `json:"service_id"`
	Start     string `json:"start"`  // RFC3339
	End       string `json:"end"`    // RFC3339
	LeadS     int64  `json:"lead_s"`
	TrailS    int64  `json:"trail_s"`
}

func (d Deps) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	var req scheduleCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	start, err := time.Parse(time.RFC3339, req.Start)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "start: "+err.Error())
		return
	}
	end, err := time.Parse(time.RFC3339, req.End)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "end: "+err.Error())
		return
	}
	if !end.After(start) {
		writeErr(w, http.StatusBadRequest, "end must be after start")
		return
	}
	if req.Channel == "" {
		writeErr(w, http.StatusBadRequest, "channel required")
		return
	}
	lead := time.Duration(req.LeadS) * time.Second
	if lead == 0 {
		lead = 30 * time.Second
	}
	trail := time.Duration(req.TrailS) * time.Second
	if trail == 0 {
		trail = time.Minute
	}
	id, err := d.Store.CreateSchedule(r.Context(), store.Schedule{
		Channel:   req.Channel,
		ServiceID: req.ServiceID,
		Start:     start,
		End:       end,
		Lead:      lead,
		Trail:     trail,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

func (d Deps) handleCancelSchedule(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := d.Store.UpdateScheduleState(r.Context(), id, store.ScheduleStateCanceled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleListRecordings(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	list, err := d.Store.ListRecordings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (d Deps) handleLivePlaylist(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	s, err := d.HLS.Open(r.Context(), channel)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, s.PlaylistPath)
}

func (d Deps) handleLiveSegment(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	segment := chi.URLParam(r, "segment")
	// Touch keeps the session alive while segments are still being
	// pulled; Open is unnecessary here because a viewer that's
	// requesting segments must have already hit .m3u8 to learn their
	// names. If they bypass that, return 404 rather than auto-tuning.
	s := d.HLS.Touch(channel)
	if s == nil {
		http.NotFound(w, r)
		return
	}
	// Crude path traversal guard: segments must be flat filenames.
	if strings.ContainsAny(segment, "/\\") || strings.HasPrefix(segment, ".") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(s.Dir, segment))
}

func (d Deps) handleLiveStop(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	d.HLS.Close(channel)
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleStreamM3U8(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	s := d.HLS.LastOpened()
	if s == nil {
		writeErr(w, http.StatusNotFound, "no active HLS session — open /api/live/{channel}.m3u8 first")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, s.PlaylistPath)
}

// ── helpers ────────────────────────────────────────────────────────

func parseService(s string) (uint16, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, errors.New("service must be uint16")
	}
	return uint16(n), nil
}

func parseWindow(fromS, toS string) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(12 * time.Hour)
	if fromS != "" {
		t, err := time.Parse(time.RFC3339, fromS)
		if err != nil {
			return from, to, errors.New("from: " + err.Error())
		}
		from = t
	}
	if toS != "" {
		t, err := time.Parse(time.RFC3339, toS)
		if err != nil {
			return from, to, errors.New("to: " + err.Error())
		}
		to = t
	}
	return from, to, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func slogRequestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		slog.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"dur", time.Since(start).String(),
		)
	})
}
