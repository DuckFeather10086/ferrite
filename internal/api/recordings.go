package api

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/DuckFeather10086/ferrite/internal/store"
)

// handleRecordingFile serves a recording's raw MPEG-TS.
//
// http.ServeContent does the work, which is what makes this usable: it
// answers Range requests, so mpv and VLC can seek instead of streaming
// the whole file from the top.
//
// A row still in state 'recording' is a file that is still growing. That
// is served too — the bytes so far are a valid TS prefix and play fine —
// but only the length seen at request time, so a viewer that wants the
// rest asks again.
func (d Deps) handleRecordingFile(w http.ResponseWriter, r *http.Request) {
	rec, ok := d.lookupRecording(w, r)
	if !ok {
		return
	}
	path, err := d.recordingPath(rec)
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The row outlived its file. 410 rather than 404: the recording
			// is a real thing that is listed, it just has nothing to serve.
			writeErr(w, http.StatusGone,
				"the file for recording "+strconv.FormatInt(rec.ID, 10)+
					" is no longer on disk — DELETE the recording to clear the row")
			return
		}
		writeErr(w, http.StatusInternalServerError, "open recording: "+err.Error())
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stat recording: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Content-Disposition", contentDisposition(filepath.Base(path)))
	if rec.State == store.RecordingStateRecording {
		// Still growing: a cached copy would be a truncated recording.
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
}

// derivedExts are the files internal/postprocess writes beside a recording.
// Listed here because deleting a recording has to take them with it.
var derivedExts = []string{".mp4", ".ass", ".vtt"}

// derivedFile serves one of the files the post-pass made beside a recording.
//
// The name is derived from the recording's own path rather than stored, so
// these inherit its storage-root check instead of adding three more
// filesystem paths from a database to guard. Nothing here can name a file the
// .ts could not already name.
func (d Deps) derivedFile(ext, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec, ok := d.lookupRecording(w, r)
		if !ok {
			return
		}
		src, err := d.recordingPath(rec)
		if err != nil {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		path := strings.TrimSuffix(src, filepath.Ext(src)) + ext

		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Not an error about this request so much as an answer about
				// the recording: say which, because "404" alone cannot tell a
				// transcode that has not run from one that never will.
				writeErr(w, http.StatusNotFound, describeMissingDerived(rec, ext))
				return
			}
			writeErr(w, http.StatusInternalServerError, "open: "+err.Error())
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "stat: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", contentType)
		// ServeContent answers Range requests, which is what lets a browser
		// seek in the MP4 instead of downloading it to play the last minute.
		http.ServeContent(w, r, filepath.Base(path), fi.ModTime(), f)
	}
}

func describeMissingDerived(rec store.Recording, ext string) string {
	id := strconv.FormatInt(rec.ID, 10)
	switch rec.PostState {
	case store.PostStateDone:
		// The pass ran and this file is not there: the recording had no
		// captions in it, which is ordinary and not worth an error.
		return "recording " + id + " has no " + ext + " file"
	case store.PostStateFailed:
		return "the post-pass for recording " + id + " failed: " + rec.PostError
	case store.PostStatePending, store.PostStateRunning:
		return "recording " + id + " is still being processed (" + string(rec.PostState) + ")"
	default:
		return "recording " + id + " has not been processed — " +
			"POST /api/recordings/" + id + "/postprocess to ask for it"
	}
}

// handlePostprocessRecording asks for a recording to be (re)processed.
//
// The queue is a column, so this only has to write it: the runner sweeps at
// startup and is nudged here. It is how a recording made before the post-pass
// existed gets converted, and how a failed one is retried.
func (d Deps) handlePostprocessRecording(w http.ResponseWriter, r *http.Request) {
	rec, ok := d.lookupRecording(w, r)
	if !ok {
		return
	}
	if rec.State != store.RecordingStateDone {
		writeErr(w, http.StatusConflict,
			"recording "+strconv.FormatInt(rec.ID, 10)+" is "+string(rec.State)+
				", not done — nothing to process yet")
		return
	}
	if err := d.Store.SetPostState(r.Context(), rec.ID, store.PostStatePending, ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if d.Postprocess != nil {
		d.Postprocess.Enqueue(rec.ID)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":         rec.ID,
		"post_state": store.PostStatePending,
		// Say so rather than accepting silently: with no runner the row now
		// says 'pending' and nothing is ever going to move it.
		"queued": d.Postprocess != nil,
	})
}

// handleDeleteRecording removes a recording: the file first, then the row.
//
// A recording in progress is refused. Unlinking the file underneath the
// recorder would leave it writing to an inode nobody can reach and then
// finalizing a row that no longer exists — so stop it first. `?force=1`
// overrides that, for rows stranded in state 'recording' by a hard kill
// (the daemon only finalizes them on a graceful shutdown).
//
// A file that is already gone is not an error: the row still goes, which
// is what the caller wanted.
func (d Deps) handleDeleteRecording(w http.ResponseWriter, r *http.Request) {
	rec, ok := d.lookupRecording(w, r)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") != "" && r.URL.Query().Get("force") != "0"
	if rec.State == store.RecordingStateRecording && !force {
		writeErr(w, http.StatusConflict,
			"recording "+strconv.FormatInt(rec.ID, 10)+
				" is still running — POST /api/record/"+strconv.FormatInt(rec.ID, 10)+
				"/stop first, or repeat with ?force=1 if the daemon was killed mid-recording")
		return
	}

	fileDeleted := false
	if path, err := d.recordingPath(rec); err != nil {
		// Outside the storage root, or no path at all. Don't unlink
		// anything, but the row is still the caller's to remove.
		slog.Warn("api: recording row has an unusable path; deleting the row only",
			"id", rec.ID, "path", rec.Path, "err", err)
	} else {
		// The post-pass's files go with it. They are derived from this
		// recording and named after it, so nothing else will ever look for
		// them — and the .mp4 is the largest single file of the set, which
		// makes leaving it behind the expensive kind of orphan.
		base := strings.TrimSuffix(path, filepath.Ext(path))
		for _, ext := range derivedExts {
			_ = os.Remove(base + ext)
		}
		switch err := os.Remove(path); {
		case err == nil:
			fileDeleted = true
			// Day directories accumulate otherwise. Fails harmlessly while
			// other recordings from that day are still there.
			_ = os.Remove(filepath.Dir(path))
		case errors.Is(err, fs.ErrNotExist):
			// Already deleted by hand, or the job died before writing.
		default:
			writeErr(w, http.StatusInternalServerError, "delete file: "+err.Error())
			return
		}
	}

	deleted, err := d.Store.DeleteRecording(r.Context(), rec.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		// Lost a race with another delete. The file is gone either way.
		writeErr(w, http.StatusNotFound, "no such recording")
		return
	}
	slog.Info("api: recording deleted", "id", rec.ID, "file_deleted", fileDeleted, "path", rec.Path)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":           rec.ID,
		"file_deleted": fileDeleted,
	})
}

// lookupRecording resolves the {id} URL param, writing the error response
// itself. ok=false means a response has already been sent.
func (d Deps) lookupRecording(w http.ResponseWriter, r *http.Request) (store.Recording, bool) {
	if d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "store not ready")
		return store.Recording{}, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return store.Recording{}, false
	}
	rec, err := d.Store.GetRecording(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return store.Recording{}, false
	}
	if rec == nil {
		writeErr(w, http.StatusNotFound, "no such recording")
		return store.Recording{}, false
	}
	return *rec, true
}

// recordingPath resolves a row's file and refuses anything outside the
// storage root.
//
// The recorder writes that column, so in normal operation the check never
// fires. It is here because the column is a filesystem path in a database:
// one hand-edited or corrupted row is otherwise the difference between a
// download endpoint and an arbitrary-file read (and, for DELETE, an
// arbitrary-file unlink). An empty StorageRoot skips the check — tests
// that don't serve files leave it unset.
func (d Deps) recordingPath(rec store.Recording) (string, error) {
	if rec.Path == "" {
		return "", errors.New("recording has no file")
	}
	if d.StorageRoot == "" {
		return rec.Path, nil
	}
	root, err := filepath.Abs(d.StorageRoot)
	if err != nil {
		return "", err
	}
	p, err := filepath.Abs(rec.Path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recording path %q is outside the storage root", rec.Path)
	}
	return p, nil
}

// contentDisposition names the download.
//
// Recording filenames keep the programme title, so they are usually
// Japanese — and a raw UTF-8 header value is not portable. Send both
// forms per RFC 6266: an ASCII-transliterated `filename` for old clients
// and the real name in `filename*`.
func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		asciiOnly(name), pctEncode(name))
}

// asciiOnly replaces every non-ASCII (or quoting-unsafe) byte with '_' so
// the result is safe inside a quoted-string.
func asciiOnly(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			b.WriteByte('_')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// pctEncode percent-encodes everything outside RFC 5987's attr-char set.
// net/url's escapers all leave some character unencoded that is legal in a
// URL but not in a header parameter, so this spells the set out.
func pctEncode(s string) string {
	const unreserved = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(unreserved, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
