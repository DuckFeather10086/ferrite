package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a ferrite daemon over HTTP.
//
// The daemon may be on another machine — the usual setup is the TUI on a
// laptop and the tuner box in a corner — so every URL this builds is
// absolute against BaseURL. That matters most for PlaylistURL: the switch
// endpoint answers with a *relative* path, which a local mpv cannot use.
type Client struct {
	BaseURL string // e.g. "http://tuner.lan:8010"
	HTTP    *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		// A cold channel change runs the frontend lock plus ffmpeg's first
		// segment, so the switch call legitimately takes several seconds.
		HTTP: &http.Client{Timeout: 90 * time.Second},
	}
}

// ── wire types ─────────────────────────────────────────────────────

type Channel struct {
	Name      string   `json:"name"`
	Aliases   []string `json:"aliases,omitempty"`
	ServiceID uint16   `json:"service_id"`
}

type Adapter struct {
	Adapter  int    `json:"adapter"`
	Channel  string `json:"channel,omitempty"`
	Refs     int    `json:"refs"`
	Prio     string `json:"prio,omitempty"`
	Reserved bool   `json:"reserved,omitempty"`
}

type Status struct {
	Version   string    `json:"version"`
	Uptime    string    `json:"uptime"`
	Adapters  []Adapter `json:"adapters"`
	Recording []int64   `json:"recording"`
}

// LiveChannel reports the channel currently tuned for viewing, if any.
func (s Status) LiveChannel() string {
	for _, a := range s.Adapters {
		if a.Channel != "" {
			return a.Channel
		}
	}
	return ""
}

type Event struct {
	ServiceID uint16    `json:"service_id"`
	EventID   uint16    `json:"event_id"`
	Start     time.Time `json:"start"`
	DurationS int64     `json:"duration_s"`
	Title     string    `json:"title"`
	Synopsis  string    `json:"synopsis,omitempty"`
}

func (e Event) End() time.Time { return e.Start.Add(time.Duration(e.DurationS) * time.Second) }

// Airing reports whether e covers t.
func (e Event) Airing(t time.Time) bool {
	return !t.Before(e.Start) && t.Before(e.End())
}

type Recording struct {
	ID        int64      `json:"id"`
	Channel   string     `json:"channel"`
	Title     string     `json:"title,omitempty"`
	Start     time.Time  `json:"start"`
	End       *time.Time `json:"end"`
	Path      string     `json:"path"`
	SizeBytes *int64     `json:"size_bytes"`
	State     string     `json:"state"`
	Error     string     `json:"error,omitempty"`
}

type SwitchResult struct {
	Channel  string   `json:"channel"`
	Playlist string   `json:"playlist"` // relative to the daemon root
	Closed   []string `json:"closed"`
}

type RecordResult struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	Title   string `json:"title"`
}

// ── calls ──────────────────────────────────────────────────────────

func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var out []Channel
	err := c.do(ctx, http.MethodGet, "/api/channels", nil, &out)
	return out, err
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var out Status
	err := c.do(ctx, http.MethodGet, "/api/status", nil, &out)
	return out, err
}

// Now returns what is airing on serviceID, or nil when the EPG has
// nothing for it (an empty guide is normal, not an error).
func (c *Client) Now(ctx context.Context, serviceID uint16) (*Event, error) {
	var out *Event
	path := "/api/now?service=" + strconv.FormatUint(uint64(serviceID), 10)
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Schedule returns events for serviceID within [from, to].
func (c *Client) Schedule(ctx context.Context, serviceID uint16, from, to time.Time) ([]Event, error) {
	q := url.Values{}
	q.Set("service", strconv.FormatUint(uint64(serviceID), 10))
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	var out []Event
	err := c.do(ctx, http.MethodGet, "/api/epg?"+q.Encode(), nil, &out)
	return out, err
}

// Switch changes the live channel: the daemon closes any other session,
// tunes this one, and answers once its playlist exists.
func (c *Client) Switch(ctx context.Context, channel string) (SwitchResult, error) {
	var out SwitchResult
	path := "/api/live/" + url.PathEscape(channel) + "/switch"
	err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
}

// Record starts recording channel now. A zero duration is open-ended.
func (c *Client) Record(ctx context.Context, channel, title string, dur time.Duration) (RecordResult, error) {
	body := map[string]any{"channel": channel}
	if title != "" {
		body["title"] = title
	}
	if dur > 0 {
		body["duration_s"] = int64(dur.Seconds())
	}
	var out RecordResult
	err := c.do(ctx, http.MethodPost, "/api/record", body, &out)
	return out, err
}

func (c *Client) StopRecording(ctx context.Context, id int64) error {
	path := "/api/record/" + strconv.FormatInt(id, 10) + "/stop"
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

func (c *Client) Recordings(ctx context.Context) ([]Recording, error) {
	var out []Recording
	err := c.do(ctx, http.MethodGet, "/api/recordings", nil, &out)
	return out, err
}

// PlaylistURL is the absolute URL a player should open for channel.
func (c *Client) PlaylistURL(channel string) string {
	return c.BaseURL + "/api/live/" + url.PathEscape(channel) + ".m3u8"
}

// ── plumbing ───────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: errorMessage(resp.Body)}
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// APIError carries the daemon's own error text, which is usually the
// actionable part ("tuner busy: …") rather than the status code.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return e.Message
}

// Busy reports the daemon refusing because the tuner is occupied — the
// one error a remote is expected to explain rather than just show.
func (e *APIError) Busy() bool { return e.Status == http.StatusConflict }

func errorMessage(r io.Reader) string {
	var payload struct {
		Error string `json:"error"`
	}
	raw, err := io.ReadAll(io.LimitReader(r, 8<<10))
	if err != nil {
		return ""
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(raw))
}
