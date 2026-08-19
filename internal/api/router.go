// Package api wires HTTP handlers onto a chi router.
//
// Endpoints implemented:
//
//	GET  /health                        liveness
//	GET  /api/status                    daemon info, adapter status
//	GET  /api/channels                  channels.json listing
//	GET  /api/epg?service=&from=&to=    EPG events in window (RFC3339)
//	GET  /api/now?service=              currently-airing event
//	GET  /api/schedule                  list schedules
//	POST /api/schedule                  create schedule
//	DELETE /api/schedule/{id}           cancel schedule
//	GET  /api/recordings                list recordings
//	GET  /api/recordings/{id}/file      download/stream a recorded file
//	GET  /api/recordings/{id}/mp4       the transcode, for a browser
//	GET  /api/recordings/{id}/subs.ass  subtitles with ARIB's own placement
//	GET  /api/recordings/{id}/subs.vtt  subtitles as words, for a <track>
//	GET  /api/recordings/{id}/{q}/video.m3u8  the same recording at a live
//	                                    quality tier, transcoded on demand
//	GET  /api/recordings/{id}/{q}/{seg}.ts    a segment of that transcode
//	POST /api/recordings/{id}/postprocess  (re)make all three
//	DELETE /api/recordings/{id}         delete a recording and its file
//	POST /api/record                    start recording now
//	POST /api/record/{id}/stop          stop an in-progress recording
//	GET  /api/scan                      is a channel scan running?
//	POST /api/scan                      sweep the band (SSE progress)
//	GET  /stream.m3u8                   live HLS, whatever is tuned
//	GET  /api/live/{channel}.m3u8       live HLS playlist for one channel
//	GET  /api/live/{channel}/{seg}.ts   live HLS segment
//	POST /api/live/{channel}/stop       tear down a live session
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/DuckFeather10086/ferrite/internal/caption"
	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/hls"
	"github.com/DuckFeather10086/ferrite/internal/netaddr"
	"github.com/DuckFeather10086/ferrite/internal/postprocess"
	"github.com/DuckFeather10086/ferrite/internal/recorder"
	"github.com/DuckFeather10086/ferrite/internal/scan"
	"github.com/DuckFeather10086/ferrite/internal/store"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// StreamPath is the one live URL clients are told about. There is
// deliberately only one: a bookmark must not have to change when the channel
// does. Reported on /api/status as "stream".
const StreamPath = "/stream.m3u8"

// livePrefix is where per-channel playlists and segments live, relative to
// the daemon root (no leading slash — see rebaseSegments).
const livePrefix = "api/live/"

// Deps is what the router needs from the rest of the daemon. Pass a
// value with the fields you have — tests can leave Store / Tuners nil
// and only exercise the static endpoints.
type Deps struct {
	Channels *config.Channels
	Store    *store.Store
	Tuners   *tuner.Pool
	HLS      *hls.Manager
	// VOD re-encodes a recording to one of the live tiers on demand, for
	// watching it somewhere a 6 Mbit/s 1080p MP4 will not go. Nil disables
	// the recording tier endpoints; the MP4 itself is served either way.
	VOD *hls.VOD
	// Recorder serves the "record now" endpoints. Nil disables them
	// (scheduled recordings still run — those go through the scheduler).
	Recorder *recorder.Manager
	// Postprocess is nudged when a recording is asked for by hand. Nil
	// still writes the row's state — a later start sweeps it up — but
	// nothing happens until then, and the response says so.
	Postprocess *postprocess.Runner
	// StorageRoot is the daemon's storage_root. Recording files are served
	// (and deleted) only from inside it, so a bad `path` column can't turn
	// the download endpoint into an arbitrary-file read. Empty skips that
	// check — leave it unset only in tests that never touch files.
	StorageRoot string
	// HTTPPort is the port this daemon listens on. /api/status reports the
	// addresses it can be reached at, and only the daemon can: a remote is
	// looking at its *own* interfaces, and a phone is looking at a screen.
	// 0 omits the list.
	HTTPPort  int
	StartedAt time.Time
	Version   string // build-time injected, optional
	// Web is the embedded static UI (e.g. web.FS()). When non-nil it is
	// served for all non-/api routes with an index.html SPA fallback;
	// nil disables UI serving (tests, headless deployments).
	Web fs.FS
	// Scanner sweeps the band for channels. Nil disables /api/scan.
	Scanner *scan.Runner
	// ChannelsFile is where the scan writes, and where Channels is
	// re-read from afterwards. Empty means a scan's results only reach
	// the daemon on the next restart.
	ChannelsFile string
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

	// The one live URL: bookmark it in VLC or on an iPad and it plays
	// whatever is tuned, now and after the next channel change.
	r.Get(StreamPath, d.handleStreamM3U8)

	r.Route("/api", func(r chi.Router) {
		r.Get("/status", d.handleStatus)
		r.Get("/channels", d.handleChannels)
		r.Get("/epg", d.handleEPG)
		r.Get("/now", d.handleNow)

		r.Get("/schedule", d.handleListSchedules)
		r.Post("/schedule", d.handleCreateSchedule)
		r.Delete("/schedule/{id}", d.handleCancelSchedule)

		r.Get("/recordings", d.handleListRecordings)
		// Each of the four files answers HEAD as well as GET. chi matches on
		// method, so a route registered with Get alone answers 405 to a HEAD —
		// and asking "is this there, and how big?" without pulling the body is
		// exactly what a client does before it commits: the web UI, to know
		// whether a recording has captions before offering to show them, and a
		// player probing what it is about to range-request. http.ServeContent
		// already writes the headers and skips the body for HEAD, so the
		// handlers need nothing.
		getHead := func(pattern string, h http.HandlerFunc) {
			r.Get(pattern, h)
			r.Head(pattern, h)
		}
		getHead("/recordings/{id}/file", d.handleRecordingFile)
		getHead("/recordings/{id}/mp4", d.derivedFile(".mp4", "video/mp4"))
		getHead("/recordings/{id}/subs.ass", d.derivedFile(".ass", "text/x-ssa; charset=utf-8"))
		getHead("/recordings/{id}/subs.vtt", d.derivedFile(".vtt", "text/vtt; charset=utf-8"))
		// The same picture at a lower bitrate, transcoded on demand — the
		// live tiers, with the recording for a source. The tier is a path
		// segment for the same reason it is one under /api/live: the
		// playlist's segment URIs are relative, so they have to resolve
		// inside the tier's own directory.
		getHead("/recordings/{id}/{quality}/"+hls.VODPlaylist, d.handleRecordingPlaylist)
		getHead("/recordings/{id}/{quality}/{segment}", d.handleRecordingSegment)
		r.Post("/recordings/{id}/postprocess", d.handlePostprocessRecording)
		r.Delete("/recordings/{id}", d.handleDeleteRecording)
		r.Post("/record", d.handleRecordNow)
		r.Post("/record/{id}/stop", d.handleRecordStop)

		r.Get("/scan", d.handleScanStatus)
		r.Post("/scan", d.handleScan)

		r.Get("/av-offsets", d.handleListAVOffsets)
		r.Delete("/av-offsets/{channel}", d.handleForgetAVOffset)

		// The quality is a path segment rather than a query parameter
		// because it is a directory on disk and a directory in the URL:
		// ffmpeg writes {channel}/{quality}/streamN.ts, and a relative
		// segment URI in the media playlist has to resolve beside it.
		// Only the master playlist takes ?q=, because that is the one URL
		// a person or a bookmark types.
		r.Get("/live/{channel}.m3u8", d.handleLivePlaylist)
		r.Get("/live/{channel}/{quality}/"+videoPlaylistName, d.handleLiveVideoPlaylist)
		r.Get("/live/{channel}/{quality}/{segment}", d.handleLiveSegment)
		r.Post("/live/{channel}/stop", d.handleLiveStop)
		r.Post("/live/{channel}/switch", d.handleLiveSwitch)
	})

	// Static web UI: everything not matched above falls through to the
	// embedded SPA. chi routes /health and /api/* to their handlers
	// first, so this only catches asset and client-route requests.
	if d.Web != nil {
		r.NotFound(staticHandler(d.Web))
	}

	return r
}

// fontDir is where the UI bundle keeps the caption font (web/public/fonts).
// Named here because its contents are served on different terms from the rest
// of the bundle — see staticHandler.
const fontDir = "fonts"

func init() {
	// Go's built-in table has no entry for .woff2 and a release image need not
	// carry /etc/mime.types, which would leave the caption font sniffed as
	// application/octet-stream. Browsers accept a font regardless, but saying
	// what it is costs one line.
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

// staticHandler serves the embedded UI bundle. Real files are served
// as-is. For paths that don't match a file, it tries the .html suffix
// (Next.js static export). Everything else falls back to index.html for
// client-side SPA routing. Unknown /api/* paths still get a JSON 404
// rather than the SPA shell.
func staticHandler(fsys fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(fsys))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/"+fontDir+"/") {
			// The exception to the line below: a font is a megabyte, its
			// name carries its version (replacing one means renaming it),
			// and the Recordings player asks for it every time a viewer
			// opens a recording with ARIB captions.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Never cache UI assets — they're embedded in the binary and
			// change across restarts.
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && !isFile(fsys, name) {
			// Try .html suffix for static-export routes (e.g. /guide → guide.html).
			if isFile(fsys, name+".html") {
				r.URL.Path = "/" + name + ".html"
			} else {
				// Not a real asset → serve the SPA shell.
				r.URL.Path = "/"
			}
		}
		fileServer.ServeHTTP(w, r)
	}
}

// isFile reports whether name is a regular file in fsys.
//
// The directory case is the whole reason this is not a bare Open: the
// export puts the page for /guide in guide.html *and* a sibling directory
// guide/ holding the RSC payloads that client-side navigation fetches.
// Open("guide") therefore succeeds, and a handler that took that as "the
// asset exists" served the directory — which, having no index.html of its
// own, came back as an autoindex listing of __next.*.txt files. Loading
// /guide, /schedules or /recordings straight from the address bar (a
// bookmark, or a reload) got that listing instead of the page; only
// arriving via a tab click worked, because then the router never asks the
// server for the route at all.
func isFile(fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	return err == nil && !st.IsDir()
}

func (d Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"version": d.Version,
		"started": d.StartedAt.Format(time.RFC3339),
		"uptime":  time.Since(d.StartedAt).Round(time.Second).String(),
		// The single live URL, relative to any of the addresses below.
		// One playlist, whatever is tuned — see handleStreamM3U8.
		"stream": StreamPath,
	}
	if d.HTTPPort > 0 {
		// Recomputed per request rather than cached at boot: an interface
		// can come up later (tailscaled starting after the daemon, DHCP
		// renewing into a new subnet), and this costs one netlink read.
		resp["addresses"] = netaddr.Addresses(d.HTTPPort)
	}
	if d.Tuners != nil {
		resp["adapters"] = d.Tuners.Status()
	}
	if d.HLS != nil {
		// The live quality tiers on offer, first one the default. Reported
		// here rather than on an endpoint of its own because the player
		// already fetches this, and the list is fixed at startup.
		resp["live_qualities"] = d.HLS.QualityList()
	}
	if d.Recorder != nil {
		// Ad-hoc recordings only; scheduled ones are in /api/recordings.
		resp["recording"] = d.Recorder.Active()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d Deps) handleChannels(w http.ResponseWriter, r *http.Request) {
	if d.Channels == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type out struct {
		Name string `json:"name"`
		// DisplayName is what a UI shows; Name is what every other
		// endpoint takes. Computed here so the TUI, the web UI and any
		// agent all label a channel identically instead of each
		// re-deriving it from the alias list and disagreeing.
		DisplayName string   `json:"display_name"`
		Aliases     []string `json:"aliases,omitempty"`
		ServiceID   uint16   `json:"service_id"`
	}
	all := d.Channels.All()
	rows := make([]out, 0, len(all))
	for i := range all {
		c := &all[i]
		rows = append(rows, out{
			Name:        c.Name,
			DisplayName: c.DisplayName(),
			Aliases:     c.Aliases,
			ServiceID:   c.ServiceID(),
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
	writeJSON(w, http.StatusOK, orEmpty(events))
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
	writeJSON(w, http.StatusOK, orEmpty(list))
}

type scheduleCreateReq struct {
	Channel   string `json:"channel"`
	ServiceID uint16 `json:"service_id"`
	Start     string `json:"start"` // RFC3339
	End       string `json:"end"`   // RFC3339
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
	writeJSON(w, http.StatusOK, orEmpty(list))
}

// handleListAVOffsets exposes the cached A/V skew measurements. Mostly
// a debugging window: if one channel's lip-sync looks wrong, this is
// the number to blame, and DELETE forces a fresh measurement.
func (d Deps) handleListAVOffsets(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	list, err := d.Store.ListAudioOffsets(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type out struct {
		Channel  string    `json:"channel"`
		OffsetS  float64   `json:"offset_s"`
		Measured time.Time `json:"measured"`
	}
	rows := make([]out, 0, len(list))
	for _, a := range list {
		rows = append(rows, out{Channel: a.Channel, OffsetS: a.OffsetS, Measured: a.Measured})
	}
	writeJSON(w, http.StatusOK, rows)
}

func (d Deps) handleForgetAVOffset(w http.ResponseWriter, r *http.Request) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	if d.Channels != nil {
		if ch := d.Channels.Find(channel); ch != nil {
			channel = ch.Name
		}
	}
	if err := d.Store.ForgetAudioOffset(r.Context(), channel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type recordNowReq struct {
	Channel string `json:"channel"`
	Title   string `json:"title"`
	// DurationS bounds the recording. 0 means open-ended — it runs until
	// POST /api/record/{id}/stop, capped at recorder.MaxAdhocDuration.
	DurationS int64 `json:"duration_s"`
}

// handleRecordNow starts recording immediately. Returns the recording
// row id, which is also the handle for stopping it.
//
// 201 does not mean bytes are on disk: the tuner is acquired
// asynchronously, and a failure lands in the row's state/error. The
// upfront CanServe check only rejects the case we can already see is
// hopeless, so the caller gets a clean 409 instead of a row that fails
// a second later.
func (d Deps) handleRecordNow(w http.ResponseWriter, r *http.Request) {
	if d.Recorder == nil {
		writeErr(w, http.StatusServiceUnavailable, "recorder not ready")
		return
	}
	var req recordNowReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Channel == "" {
		writeErr(w, http.StatusBadRequest, "channel required")
		return
	}

	channel := req.Channel
	var serviceID uint16
	if d.Channels != nil {
		ch := d.Channels.Find(req.Channel)
		if ch == nil {
			writeErr(w, http.StatusNotFound, "unknown channel "+req.Channel)
			return
		}
		channel = ch.Name
		serviceID = ch.ServiceID()
	}

	if d.Tuners != nil && !d.Tuners.CanServe(channel, tuner.PrioRecord) {
		writeErr(w, http.StatusConflict,
			"tuner busy: every adapter is held by a recording — stop one first")
		return
	}

	// Name the file after what's on air when the caller didn't say.
	title := req.Title
	if title == "" && d.Store != nil && serviceID != 0 {
		if e, err := d.Store.NowPlaying(r.Context(), serviceID, time.Now()); err == nil && e != nil {
			title = e.Title
		}
	}

	id, err := d.Recorder.Start(r.Context(), channel,
		title, time.Duration(req.DurationS)*time.Second)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      id,
		"channel": channel,
		"title":   title,
	})
}

func (d Deps) handleRecordStop(w http.ResponseWriter, r *http.Request) {
	if d.Recorder == nil {
		writeErr(w, http.StatusServiceUnavailable, "recorder not ready")
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := d.Recorder.Stop(id); err != nil {
		if errors.Is(err, recorder.ErrNotRecording) {
			writeErr(w, http.StatusNotFound, "recording not in progress")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d Deps) handleLivePlaylist(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	s, err := d.HLS.Open(r.Context(), channel, requestedQuality(r))
	if err != nil {
		writeTunerErr(w, err)
		return
	}
	// ffmpeg writes stream.m3u8 only once the first segment completes —
	// several seconds after Open returns on a cold tune. Hold the
	// request until the playlist exists; an immediate answer would
	// describe a stream with nothing in it, and most players treat that
	// as fatal.
	if err := waitForFile(r.Context(), s.PlaylistPath, 30*time.Second); err != nil {
		writeErr(w, http.StatusGatewayTimeout, "stream did not start: "+err.Error())
		return
	}
	writePlaylist(w, masterPlaylist(s, d.qualityOf(s), "", subsAnnounced(r.Context(), s)))
}

// handleLiveVideoPlaylist serves the media playlist the master points at.
//
// It exists because the two playlists cannot be the same URL: the master
// references the video rendition, and a master that referenced itself is not a
// playlist a player can follow. ffmpeg writes the segment URIs with its
// -hls_base_url prefix ("{channel}/streamN.ts"), which is correct one directory
// up; served from inside the channel's own path they resolve one level too
// deep, so the prefix comes off here.
func (d Deps) handleLiveVideoPlaylist(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	channel := chi.URLParam(r, "channel")
	// Touch, not Open: a player only learns this URL from a master playlist,
	// which means the session already exists. Opening here would let a stale
	// player re-tune a channel nobody asked for.
	s := d.HLS.Touch(channel, chi.URLParam(r, "quality"))
	if s == nil {
		http.NotFound(w, r)
		return
	}
	playlist, err := os.ReadFile(s.PlaylistPath)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "stream not started yet")
		return
	}
	writePlaylist(w, stripSegmentBase(playlist))
}

// waitForFile polls until path exists and is non-empty, the context is
// canceled, or timeout elapses.
func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no playlist after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
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
	s := d.HLS.Touch(channel, chi.URLParam(r, "quality"))
	if s == nil {
		http.NotFound(w, r)
		return
	}
	// Crude path traversal guard: segments must be flat filenames.
	if strings.ContainsAny(segment, "/\\") || strings.HasPrefix(segment, ".") {
		http.NotFound(w, r)
		return
	}
	// Segments are .ts, but the same directory also holds the subtitle
	// renditions written by internal/caption — its playlist, its .vtt
	// segments, and the .json the ARIB overlay draws from. Serving those as
	// video/mp2t makes a player discard them.
	switch {
	case strings.HasSuffix(segment, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(segment, ".vtt"):
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	case strings.HasSuffix(segment, ".json"):
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "video/mp2t")
	}
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

// handleLiveSwitch is the whole "change channel" step: drop whatever
// else is playing, tune this channel, and don't answer until its
// playlist is on disk so the caller can hand the URL straight to a
// player.
//
// A client can do this by hand (POST the old channel's /stop, then GET
// the new .m3u8), but with a single adapter getting that order wrong
// deadlocks on ErrNoAdapter — two live sessions have equal priority and
// won't evict each other. One endpoint, one correct order.
func (d Deps) handleLiveSwitch(w http.ResponseWriter, r *http.Request) {
	channel := chi.URLParam(r, "channel")
	if d.Channels != nil && d.Channels.Find(channel) == nil {
		writeErr(w, http.StatusNotFound, "unknown channel "+channel)
		return
	}
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}

	quality := requestedQuality(r)
	closed := d.HLS.CloseOthers(channel)
	s, err := d.HLS.Open(r.Context(), channel, quality)
	if errors.Is(err, tuner.ErrNoAdapter) {
		// Between closing the others and claiming the adapter, something can
		// take it back: a player still polling the channel it was watching
		// re-tunes it through GET /api/live/{ch}.m3u8, and live never evicts
		// live. Close again — this time whatever just appeared — and make one
		// more attempt, since the viewer asking for a channel outranks a page
		// nobody is looking at.
		again := d.HLS.CloseOthers(channel)
		if len(again) > 0 {
			slog.Info("live: adapter was taken back mid-switch; closing and retrying",
				"channel", channel, "closed", again)
			closed = append(closed, again...)
			s, err = d.HLS.Open(r.Context(), channel, quality)
		}
	}
	if err != nil {
		writeTunerErr(w, err)
		return
	}
	if err := waitForFile(r.Context(), s.PlaylistPath, 45*time.Second); err != nil {
		writeErr(w, http.StatusGatewayTimeout, "stream did not start: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"channel": s.Channel,
		"quality": s.Quality,
		// Both URLs now serve this channel. "stream" is the one to hand a
		// player: it is stable across channel changes. "playlist" addresses
		// this channel specifically, which is what the web UI's <video> needs.
		"stream":   StreamPath,
		"playlist": livePlaylistPath(s),
		"closed":   closed,
	})
}

// handleStreamM3U8 serves live TV as one fixed URL, whatever is tuned.
//
// This is the contract the legacy live_hls.py had and the one a viewer wants:
// a single bookmark in VLC, on an iPad, in mpv, that keeps working when the
// channel changes. Per-channel playlists still exist at
// /api/live/{channel}.m3u8 — the web UI needs to address a specific channel —
// but nothing outside needs to know a channel name to watch TV.
func (d Deps) handleStreamM3U8(w http.ResponseWriter, r *http.Request) {
	if d.HLS == nil {
		writeErr(w, http.StatusServiceUnavailable, "hls not ready")
		return
	}
	s := d.HLS.LastOpened()
	if s == nil {
		writeErr(w, http.StatusNotFound, "no active HLS session — tune a channel first")
		return
	}
	// A viewer that only polls this URL never touches
	// /api/live/{channel}.m3u8, so without this the idle janitor would
	// close the session under a player that is actively watching it.
	d.HLS.Touch(s.Channel, s.Quality)

	if _, err := os.Stat(s.PlaylistPath); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "stream not started yet")
		return
	}
	writePlaylist(w, masterPlaylist(s, d.qualityOf(s), livePrefix, subsAnnounced(r.Context(), s)))
}

// videoPlaylistName is where a tier's media playlist is served, under the
// tier's own path so its segment URIs resolve beside it.
const videoPlaylistName = "video.m3u8"

// qualityParam is how the one URL a person types asks for a tier:
// /api/live/NHK_G.m3u8?q=720p. Everything below the master playlist takes
// it as a path segment instead — those URLs are ours to write, and a
// relative segment URI has to resolve inside the tier's directory.
const qualityParam = "q"

// requestedQuality reads the tier out of a request. Empty is normal and
// means "whatever the default is"; an unknown name is treated the same
// way rather than refused, because the alternative is a stale bookmark
// getting an error page instead of television.
func requestedQuality(r *http.Request) string {
	return r.URL.Query().Get(qualityParam)
}

// qualityOf is the tier a session is running, as the manifest needs to
// describe it.
func (d Deps) qualityOf(s *hls.Session) hls.QualityInfo {
	if d.HLS != nil {
		for _, q := range d.HLS.QualityList() {
			if q.Name == s.Quality {
				return q
			}
		}
	}
	return hls.QualityInfo{Name: s.Quality, Label: s.Quality, Bandwidth: 6_500_000}
}

// livePlaylistPath is the master playlist URL for a session, tier and all.
func livePlaylistPath(s *hls.Session) string {
	return "/" + livePrefix + url.PathEscape(s.Channel) + ".m3u8?" +
		qualityParam + "=" + url.QueryEscape(s.Quality)
}

// masterPlaylist composes the multivariant playlist: the video rendition, and
// the WebVTT subtitle rendition when internal/caption has produced one.
//
// Composed here rather than written to disk because it is the same manifest
// from two URLs — /stream.m3u8 and /api/live/{channel}.m3u8 — differing only in
// how far the URIs have to reach back to /api/live/. prefix is that distance:
// "" when already under /api/live/, livePrefix from the root. The URIs stay
// relative so the daemon survives being mounted behind a path prefix.
//
// Both URLs carry the captions on purpose: Safari and iOS play HLS natively and
// pick up a subtitle rendition from the manifest, so a bookmark on an iPad
// gets captions without anything of ours running in the browser.
func masterPlaylist(s *hls.Session, q hls.QualityInfo, prefix string, withSubs bool) []byte {
	base := prefix + url.PathEscape(s.Channel) + "/" + url.PathEscape(s.Quality) + "/"
	var b bytes.Buffer
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	if withSubs {
		// DEFAULT=NO: captions are the viewer's choice, as they are on a
		// television. AUTOSELECT=YES lets a player turn them on when the
		// system asks for Japanese subtitles.
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q,LANGUAGE=%q,DEFAULT=NO,AUTOSELECT=YES,URI=%q\n",
			"subs", "日本語", "ja", base+caption.SubsPlaylist)
	}
	// The bitrate and codecs the HLS session encodes to (see internal/hls).
	//
	// One variant, always: this is a tier the viewer chose, not a ladder
	// for the player to climb. Listing the others would make a player
	// switch away from the choice — and standing them all up to be
	// switchable is several simultaneous encodes of the same picture,
	// which is what the on-demand design exists to avoid.
	fmt.Fprintf(&b, `#EXT-X-STREAM-INF:BANDWIDTH=%d,CODECS="avc1.640028,mp4a.40.2"`, q.Bandwidth)
	if withSubs {
		b.WriteString(`,SUBTITLES="subs"`)
	}
	b.WriteString("\n")
	b.WriteString(base + videoPlaylistName + "\n")
	return b.Bytes()
}

// subsWait bounds how long composing a master playlist will wait for a
// session's first subtitle rendition. internal/caption publishes one a tick
// (half a segment) after ffmpeg's first segment appears in the playlist; three
// seconds covers that and stops a session whose decoder died from holding every
// manifest request open.
const subsWait = 3 * time.Second

// subsAnnounced reports whether to name the caption rendition in the master
// playlist, waiting briefly for a session that is about to publish its first.
//
// The wait is what makes the browser's own captions menu usable: a player reads
// the master once and never again, so a manifest composed in the second between
// the video playlist appearing and the first subs.m3u8 leaves that session with
// no captions at all — and now there is no button of ours to turn them on with.
// Announcing a rendition that 404s is the other failure (some players abandon
// the stream), which is why this waits for the file rather than trusting
// Captions on its own.
func subsAnnounced(ctx context.Context, s *hls.Session) bool {
	if s.Captions && !subsReady(s.Dir) {
		_ = waitForFile(ctx, filepath.Join(s.Dir, caption.SubsPlaylist), subsWait)
	}
	return subsReady(s.Dir)
}

// subsReady reports whether a session has a subtitle rendition on disk.
func subsReady(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, caption.SubsPlaylist))
	return err == nil && st.Size() > 0
}

// stripSegmentBase removes ffmpeg's -hls_base_url prefix from each segment URI,
// so they resolve beside the playlist rather than one directory deeper.
func stripSegmentBase(playlist []byte) []byte {
	lines := bytes.Split(playlist, []byte("\n"))
	for i, line := range lines {
		uri := bytes.TrimSpace(line)
		// Tags, blanks, and anything already absolute or fully qualified.
		if len(uri) == 0 || uri[0] == '#' || uri[0] == '/' ||
			bytes.Contains(uri, []byte("://")) {
			continue
		}
		lines[i] = []byte(path.Base(string(uri)))
	}
	return bytes.Join(lines, []byte("\n"))
}

func writePlaylist(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
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

// orEmpty makes a nil slice marshal as [] rather than null.
//
// A nil slice is the same as an empty one in Go, so this is invisible on the
// server — but every non-Go client has to special-case `null` before
// iterating, and forgetting to is a crash rather than an empty list. Wrap
// every list response.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeTunerErr maps a tune failure onto a status code: a busy tuner is
// 409 (the caller can stop something and retry), a channel this box has no
// frontend for is 501, and anything else is a real fault worth surfacing
// as 502.
//
// The 501 is worth its own code rather than folding into the 409. "Tuner
// busy" invites a retry, and a client that retries a BS channel on a
// terrestrial-only box will do so forever; "not implemented" says the
// hardware is missing, which is the actual repair.
func writeTunerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tuner.ErrNoCapableAdapter):
		writeErr(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, tuner.ErrNoAdapter):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusBadGateway, err.Error())
	}
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
