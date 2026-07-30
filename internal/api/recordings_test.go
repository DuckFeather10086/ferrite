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
