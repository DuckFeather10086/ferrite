// Package api wires HTTP handlers onto a chi router.
//
// Endpoints (sketch):
//   GET  /api/channels                  list configured channels
//   GET  /api/epg?service=&from=&to=    EPG events in window
//   GET  /api/now?service=              currently-airing event
//   POST /api/schedule                  create recording schedule
//   GET  /api/schedule                  list schedules
//   DEL  /api/schedule/{id}             cancel
//   GET  /api/recordings                list completed/in-progress
//   GET  /api/recordings/{id}/file      stream the file
//   GET  /api/live/{channel}.m3u8       live HLS playlist
//   GET  /api/live/{channel}/{seg}.ts   live HLS segment
//   GET  /api/status                    adapter usage, signal, sessions
//   GET  /                              static frontend (embedded)
package api

import "net/http"

func NewRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	return mux
}
