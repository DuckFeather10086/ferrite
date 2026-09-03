package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/store"
)

// ── harness ────────────────────────────────────────────────────────

func del(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, path, nil))
	return rr
}

// fileRouter is a store + storage root with no tuner: enough to exercise
// download and delete against rows we write by hand.
type fileRouter struct {
	h    http.Handler
	st   *store.Store
	root string
}

func newFileRouter(t *testing.T) fileRouter {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root := filepath.Join(dir, "var")
	return fileRouter{
		h: NewRouter(Deps{
			Store:       st,
			StorageRoot: root,
			StartedAt:   time.Now(),
		}),
		st:   st,
		root: root,
	}
}

// addRecording writes a row plus its file and returns the row id.
func (f fileRouter) addRecording(t *testing.T, name, body string, state store.RecordingState) int64 {
	t.Helper()
	path := filepath.Join(f.root, "recordings", "2026-07-30", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return f.addRow(t, path, state)
}

// addRow writes a row for an arbitrary path, with no file behind it.
func (f fileRouter) addRow(t *testing.T, path string, state store.RecordingState) int64 {
	t.Helper()
	id, err := f.st.CreateRecording(context.Background(), store.Recording{
		Channel: "mx",
		Title:   "報道ステーション",
		Path:    path,
		State:   state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func idPath(prefix string, id int64, suffix string) string {
	return prefix + strconv.FormatInt(id, 10) + suffix
}

// ── download ───────────────────────────────────────────────────────

func TestRecordingFile_ServesBytes(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130_報道ステーション.ts", "0123456789", store.RecordingStateDone)

	rr := get(t, f.h, idPath("/api/recordings/", id, "/file"))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "0123456789" {
		t.Fatalf("body = %q", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/mp2t" {
		t.Fatalf("content-type = %q", ct)
	}
	// A Japanese filename must survive as filename*, with an ASCII fallback
	// for clients that only understand the plain parameter.
	cd := rr.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "filename*=UTF-8''mx_2130_%E5%A0%B1%E9%81%93") {
		t.Fatalf("content-disposition = %q, want a percent-encoded filename*", cd)
	}
	if !strings.Contains(cd, `filename="mx_2130_`) {
		t.Fatalf("content-disposition = %q, want an ASCII fallback", cd)
	}
	for _, c := range cd {
		if c > 0x7f {
			t.Fatalf("content-disposition carries raw non-ASCII: %q", cd)
		}
	}
}

// Seeking in a player is a Range request; without it mpv has to stream a
// two-hour file from the start to reach the last minute.
func TestRecordingFile_SupportsRange(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130_x.ts", "0123456789", store.RecordingStateDone)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, idPath("/api/recordings/", id, "/file"), nil)
	req.Header.Set("Range", "bytes=4-6")
	f.h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("%d %s, want 206", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "456" {
		t.Fatalf("body = %q, want 456", rr.Body.String())
	}
}

// A recording that is still rolling is served as the prefix it has so
// far — useful for checking a recording actually contains picture.
func TestRecordingFile_InProgressIsServedUncached(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130_live.ts", "partial", store.RecordingStateRecording)

	rr := get(t, f.h, idPath("/api/recordings/", id, "/file"))
	if rr.Code != http.StatusOK || rr.Body.String() != "partial" {
		t.Fatalf("%d %q", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store for a growing file", cc)
	}
}

// With transcode.delete_source on, a finished recording has no .ts — the
// post-pass's MP4 is all that is left of it. The download endpoint is what the
// UI's Download button and the TUI's mpv open, so it serves that rather than
// reporting a programme the browser can play as no longer on disk.
func TestRecordingFile_FallsBackToTheMP4(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130.ts", "transport stream", store.RecordingStateDone)
	ts := filepath.Join(f.root, "recordings", "2026-07-30", "mx_2130.ts")
	if err := os.WriteFile(strings.TrimSuffix(ts, ".ts")+".mp4", []byte("an-mp4"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ts); err != nil {
		t.Fatal(err)
	}

	rr := get(t, f.h, idPath("/api/recordings/", id, "/file"))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "an-mp4" {
		t.Fatalf("body = %q", rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("content-type = %q, want video/mp4", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "mx_2130.mp4") {
		t.Fatalf("content-disposition = %q, want the .mp4's name", cd)
	}
}

func TestRecordingFile_MissingFileIsGone(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRow(t, filepath.Join(f.root, "recordings", "2026-07-30", "vanished.ts"),
		store.RecordingStateDone)

	rr := get(t, f.h, idPath("/api/recordings/", id, "/file"))
	if rr.Code != http.StatusGone {
		t.Fatalf("%d %s, want 410", rr.Code, rr.Body.String())
	}
}

func TestRecordingFile_UnknownAndBadIDs(t *testing.T) {
	f := newFileRouter(t)
	if rr := get(t, f.h, "/api/recordings/999/file"); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown id: %d %s", rr.Code, rr.Body.String())
	}
	if rr := get(t, f.h, "/api/recordings/abc/file"); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d %s", rr.Code, rr.Body.String())
	}
}

// The `path` column is a filesystem path in a database. One bad row must
// not make this an arbitrary-file read.
func TestRecordingFile_RefusesPathOutsideStorageRoot(t *testing.T) {
	f := newFileRouter(t)
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := f.addRow(t, secret, store.RecordingStateDone)

	rr := get(t, f.h, idPath("/api/recordings/", id, "/file"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("%d %s, want 403", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "PRIVATE KEY") {
		t.Fatal("served a file outside the storage root")
	}
	// Traversal spelled with .. must fail the same way.
	id2 := f.addRow(t, filepath.Join(f.root, "recordings", "..", "..", "etc", "passwd"),
		store.RecordingStateDone)
	if rr := get(t, f.h, idPath("/api/recordings/", id2, "/file")); rr.Code != http.StatusForbidden {
		t.Fatalf("traversal: %d %s, want 403", rr.Code, rr.Body.String())
	}
}

// ── delete ─────────────────────────────────────────────────────────

func TestDeleteRecording_RemovesFileAndRow(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130_x.ts", "data", store.RecordingStateDone)
	path := filepath.Join(f.root, "recordings", "2026-07-30", "mx_2130_x.ts")

	rr := del(t, f.h, idPath("/api/recordings/", id, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		ID          int64 `json:"id"`
		FileDeleted bool  `json:"file_deleted"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != id || !out.FileDeleted {
		t.Fatalf("body = %+v", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
	// The now-empty day directory goes too.
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("day dir still there: %v", err)
	}
	recs, err := f.st.ListRecordings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("rows = %+v, want none", recs)
	}
	if rr := del(t, f.h, idPath("/api/recordings/", id, "")); rr.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d %s, want 404", rr.Code, rr.Body.String())
	}
}

// Deleting a row whose file someone already removed by hand must clear
// the row — that is the whole point of the request.
func TestDeleteRecording_MissingFileStillClearsRow(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRow(t, filepath.Join(f.root, "recordings", "2026-07-30", "vanished.ts"),
		store.RecordingStateDone)

	rr := del(t, f.h, idPath("/api/recordings/", id, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var out struct {
		FileDeleted bool `json:"file_deleted"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.FileDeleted {
		t.Fatal("file_deleted = true for a file that was not there")
	}
	rec, err := f.st.GetRecording(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		t.Fatalf("row survived: %+v", rec)
	}
}

// Deleting under a running recorder would leave it writing to an unlinked
// inode, so an in-progress row is refused until it is stopped — with an
// escape hatch for rows stranded by a hard kill.
func TestDeleteRecording_InProgressNeedsForce(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "mx_2130_live.ts", "partial", store.RecordingStateRecording)

	rr := del(t, f.h, idPath("/api/recordings/", id, ""))
	if rr.Code != http.StatusConflict {
		t.Fatalf("%d %s, want 409", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "/stop") {
		t.Fatalf("the 409 should say how to stop it: %s", rr.Body.String())
	}
	if rec, _ := f.st.GetRecording(context.Background(), id); rec == nil {
		t.Fatal("row was deleted despite the 409")
	}

	if rr := del(t, f.h, idPath("/api/recordings/", id, "?force=1")); rr.Code != http.StatusOK {
		t.Fatalf("force: %d %s", rr.Code, rr.Body.String())
	}
	if rec, _ := f.st.GetRecording(context.Background(), id); rec != nil {
		t.Fatalf("force did not delete the row: %+v", rec)
	}
}

// A row pointing outside the storage root loses its row without anything
// being unlinked.
func TestDeleteRecording_OutsideRootKeepsFile(t *testing.T) {
	f := newFileRouter(t)
	secret := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := f.addRow(t, secret, store.RecordingStateDone)

	rr := del(t, f.h, idPath("/api/recordings/", id, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("file outside the root was unlinked: %v", err)
	}
	if rec, _ := f.st.GetRecording(context.Background(), id); rec != nil {
		t.Fatal("row survived")
	}
}

// Without a store these must say so rather than fall through to the SPA
// 404 (which a client would read as "no such recording").
func TestRecordingFileEndpoints_NoStore(t *testing.T) {
	h, _ := newTestRouter(t, false)
	if rr := get(t, h, "/api/recordings/1/file"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("file: %d %s", rr.Code, rr.Body.String())
	}
	if rr := del(t, h, "/api/recordings/1"); rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
}

// The post-pass writes its files beside the recording, and the endpoints that
// serve them derive the name rather than reading another path column — so a
// row that cannot serve its .ts cannot serve an .mp4 either.
func TestDerivedFilesAreServedFromBesideTheRecording(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "x.ts", "transport stream", store.RecordingStateDone)
	base := filepath.Join(f.root, "recordings", "2026-07-30", "x")
	for name, body := range map[string]string{
		".mp4": "an-mp4",
		".ass": "[Script Info]\nDialogue: 0,0:00:00.00,0:00:01.00,Default,,0,0,0,,x\n",
		".vtt": "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nx\n",
	} {
		if err := os.WriteFile(base+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct{ path, wantType, wantBody string }{
		{"/mp4", "video/mp4", "an-mp4"},
		{"/subs.ass", "text/x-ssa; charset=utf-8", "Dialogue:"},
		{"/subs.vtt", "text/vtt; charset=utf-8", "WEBVTT"},
	} {
		rr := get(t, f.h, "/api/recordings/"+strconv.FormatInt(id, 10)+tc.path)
		if rr.Code != http.StatusOK {
			t.Errorf("GET %s = %d %s", tc.path, rr.Code, rr.Body.String())
			continue
		}
		if ct := rr.Header().Get("Content-Type"); ct != tc.wantType {
			t.Errorf("GET %s: Content-Type %q, want %q", tc.path, ct, tc.wantType)
		}
		if !strings.Contains(rr.Body.String(), tc.wantBody) {
			t.Errorf("GET %s served %q", tc.path, rr.Body.String())
		}
	}
}

// Every file endpoint answers HEAD, because "is it there, and how big?" is a
// question worth asking without pulling the body — the web UI asks it to know
// whether a recording has captions before offering to show them, and a player
// asks it before range-requesting. chi matches on method, so a route
// registered with Get alone answers 405 and the caller reads that as "no".
func TestFileEndpointsAnswerHEAD(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "x.ts", "transport stream", store.RecordingStateDone)
	base := filepath.Join(f.root, "recordings", "2026-07-30", "x")
	if err := os.WriteFile(base+".vtt", []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/file", http.StatusOK},
		{"/subs.vtt", http.StatusOK},
		// No .ass beside it: still an answer, and not a 405.
		{"/subs.ass", http.StatusNotFound},
	} {
		rr := httptest.NewRecorder()
		url := "/api/recordings/" + strconv.FormatInt(id, 10) + tc.path
		f.h.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, url, nil))
		if rr.Code != tc.want {
			t.Errorf("HEAD %s = %d, want %d", tc.path, rr.Code, tc.want)
		}
		if tc.want == http.StatusOK && rr.Body.Len() != 0 {
			t.Errorf("HEAD %s returned a %d-byte body", tc.path, rr.Body.Len())
		}
	}
}

// A 404 here is an answer about the recording, not about the URL, so it has
// to say which: a transcode that has not run yet reads very differently from
// one that failed, and identically over the wire without this.
func TestAMissingDerivedFileSaysWhy(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "x.ts", "ts", store.RecordingStateDone)
	url := "/api/recordings/" + strconv.FormatInt(id, 10) + "/mp4"

	rr := get(t, f.h, url)
	if rr.Code != http.StatusNotFound || !strings.Contains(rr.Body.String(), "postprocess") {
		t.Fatalf("unprocessed: %d %s", rr.Code, rr.Body.String())
	}

	if err := f.st.SetPostState(context.Background(), id, store.PostStateFailed, "no such device"); err != nil {
		t.Fatal(err)
	}
	rr = get(t, f.h, url)
	if !strings.Contains(rr.Body.String(), "no such device") {
		t.Fatalf("failed: %d %s", rr.Code, rr.Body.String())
	}

	if err := f.st.SetPostState(context.Background(), id, store.PostStateRunning, ""); err != nil {
		t.Fatal(err)
	}
	rr = get(t, f.h, url)
	if !strings.Contains(rr.Body.String(), "still being processed") {
		t.Fatalf("running: %d %s", rr.Code, rr.Body.String())
	}
}

// Asking by hand is how a recording made before the post-pass existed gets
// converted, and how a failed one is retried.
func TestPostprocessEndpointQueuesTheRecording(t *testing.T) {
	f := newFileRouter(t)
	done := f.addRecording(t, "x.ts", "ts", store.RecordingStateDone)
	running := f.addRecording(t, "y.ts", "ts", store.RecordingStateRecording)

	rr := post(t, f.h, "/api/recordings/"+strconv.FormatInt(done, 10)+"/postprocess", "")
	if rr.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	rec, _ := f.st.GetRecording(context.Background(), done)
	if rec.PostState != store.PostStatePending {
		t.Fatalf("state %q, want pending", rec.PostState)
	}
	// No runner wired here, so the response has to admit nothing will happen.
	if !strings.Contains(rr.Body.String(), `"queued":false`) {
		t.Fatalf("silently accepted with no runner: %s", rr.Body.String())
	}

	// Nothing to convert until the file stops growing.
	rr = post(t, f.h, "/api/recordings/"+strconv.FormatInt(running, 10)+"/postprocess", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("in-progress recording: %d %s", rr.Code, rr.Body.String())
	}
}

// Deleting a recording takes the post-pass's files with it. They are named
// after it and derived from it, so nothing else would ever come looking — and
// the .mp4 is the biggest file of the set.
func TestDeleteRemovesTheDerivedFilesToo(t *testing.T) {
	f := newFileRouter(t)
	id := f.addRecording(t, "x.ts", "transport stream", store.RecordingStateDone)
	base := filepath.Join(f.root, "recordings", "2026-07-30", "x")
	for _, ext := range derivedExts {
		if err := os.WriteFile(base+ext, []byte("derived"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if rr := del(t, f.h, "/api/recordings/"+strconv.FormatInt(id, 10)); rr.Code != http.StatusOK {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	for _, ext := range append(derivedExts, ".ts") {
		if _, err := os.Stat(base + ext); err == nil {
			t.Errorf("%s survived the delete", ext)
		}
	}
}
