package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/scan"
)

// handleScan sweeps the band for channels, streaming progress as
// server-sent events.
//
// SSE rather than a job id to poll, because a sweep is ten minutes of
// steady output with no state worth resuming: the client wants to watch it
// happen, and if the page is closed the scan carries on and its results
// are already on disk. Each event is one `data:` line of scan.Progress.
//
// The request context is deliberately *not* what bounds the scan. A
// browser tab closing must not abandon the sweep half-done — the adapter
// would be released mid-transport and channels.json left describing part
// of the band, with nothing to say so.
func (d Deps) handleScan(w http.ResponseWriter, r *http.Request) {
	if d.Scanner == nil {
		writeErr(w, http.StatusServiceUnavailable, "channel scanning is not configured (no dvbr_bin)")
		return
	}
	if d.Scanner.Running() {
		writeErr(w, http.StatusConflict, scan.ErrBusy.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Take the write deadline off this response. The server sets
	// WriteTimeout to 120s so that a wedged handler cannot hold a
	// connection open forever, and net/http applies it once, at the start
	// of the response — which for a stream that runs the length of a band
	// sweep means the socket is closed from under it partway through.
	// Measured: the client stopped receiving at transport 19 of 50 while
	// the sweep carried on to the end, so the page showed a scan frozen
	// two minutes in and no error anywhere.
	//
	// Deadline removed rather than extended: the sweep's own bound is the
	// per-transport timeout in internal/scan, and a stream with nothing
	// left to say ends when the sweep does.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		// Not fatal — an http.ResponseWriter that cannot do this (a test
		// recorder) has no deadline to worry about either.
		slog.Debug("scan: could not clear the write deadline", "err", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Nginx and friends buffer a response until it is complete, which for
	// a ten-minute stream means the client sees nothing at all.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Progress arrives on the scan's own goroutine; the response is
	// written from this one. A buffered channel keeps a slow client from
	// stalling the sweep, and a full one drops the event rather than
	// blocking — the next event carries the same running totals.
	events := make(chan scan.Progress, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		_, err := d.Scanner.Run(context.WithoutCancel(r.Context()), func(p scan.Progress) {
			select {
			case events <- p:
			default:
			}
		})
		if err != nil && !errors.Is(err, scan.ErrPreempted) {
			slog.Warn("scan: sweep ended early", "err", err)
		}
	}()

	// A heartbeat so a proxy (or a phone putting the tab to sleep) does
	// not decide an idle connection is dead. A transport can take 20s.
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case p, open := <-events:
			if !open {
				<-done
				// The channel list the daemon serves is loaded once at
				// boot, and a subprocess has just rewritten the file.
				// Swap it in so the new channels are selectable without a
				// restart.
				if d.Channels != nil && d.ChannelsFile != "" {
					if err := d.Channels.ReloadFrom(d.ChannelsFile); err != nil {
						slog.Warn("scan: could not reload the channel list",
							"path", d.ChannelsFile, "err", err)
					} else {
						slog.Info("scan: channel list reloaded", "n", d.Channels.Len())
					}
				}
				return
			}
			if !writeEvent(w, flusher, p) {
				// The client is gone. The sweep keeps running.
				return
			}
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// handleScanStatus answers whether a sweep is in flight, so a page loaded
// mid-scan can say so instead of offering to start a second one.
func (d Deps) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	running := d.Scanner != nil && d.Scanner.Running()
	writeJSON(w, http.StatusOK, map[string]any{
		"available": d.Scanner != nil,
		"running":   running,
	})
}

func writeEvent(w http.ResponseWriter, flusher http.Flusher, p scan.Progress) bool {
	body, err := json.Marshal(p)
	if err != nil {
		return true
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
