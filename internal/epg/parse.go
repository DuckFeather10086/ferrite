package epg

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/store"
)

// jst is the timezone dvbr emits EIT times in (ARIB EIT carries JST
// natively per ARIB STD-B10). We convert to UTC for storage so all
// queries in the rest of the daemon use a single timezone.
var jst = time.FixedZone("JST", 9*3600)

// rawEvent is the wire shape produced by `dvbr epg --json`. See
// dvbr/src/main.rs::print_events_json for the writer.
type rawEvent struct {
	// ServiceID is which service the event belongs to. One EPG pass
	// harvests every service on the tuned mux — EIT on PID 0x0012
	// describes the whole transport stream — so this is per event and not
	// per pass. A pointer to tell "absent" from 0: an older dvbr emitted
	// no service_id at all, and those events do belong to the channel that
	// was asked for.
	ServiceID     *uint16  `json:"service_id"`
	EventID       uint16   `json:"event_id"`
	Start         string   `json:"start"`         // "YYYY-MM-DD HH:MM:SS" in JST
	Duration      string   `json:"duration"`      // "HH:MM:SS"
	RunningStatus int      `json:"running_status"`
	Title         string   `json:"title"`
	Text          string   `json:"text"`
	Genres        []string `json:"genres"`
}

// Parse decodes a `dvbr epg --json` payload (a JSON array of events) into
// store.EPGEvents.
//
// serviceID is the fallback for events that don't name their own — i.e.
// the channel the pass was run for. Events that do carry a service_id keep
// it, which is what lets one tune fill in a broadcaster's subchannels as
// well as its main one. Stamping them all with the requested service
// instead would file another channel's programmes under this one.
//
// Times are parsed as JST and stored as UTC. Events with unparseable
// start/duration are skipped (logged by the caller); ARIB sometimes
// emits placeholder all-0xFF "undefined" timestamps that dvbr emits
// as empty strings.
func Parse(r io.Reader, serviceID uint16) ([]store.EPGEvent, []error) {
	var raws []rawEvent
	dec := json.NewDecoder(r)
	if err := dec.Decode(&raws); err != nil {
		return nil, []error{fmt.Errorf("epg: decode: %w", err)}
	}

	out := make([]store.EPGEvent, 0, len(raws))
	now := time.Now().UTC()
	var skipped []error

	for _, e := range raws {
		if e.Start == "" || e.Duration == "" {
			skipped = append(skipped,
				fmt.Errorf("event %d skipped: empty start/duration", e.EventID))
			continue
		}
		start, err := time.ParseInLocation("2006-01-02 15:04:05", e.Start, jst)
		if err != nil {
			skipped = append(skipped,
				fmt.Errorf("event %d: bad start %q: %w", e.EventID, e.Start, err))
			continue
		}
		dur, err := parseHMS(e.Duration)
		if err != nil {
			skipped = append(skipped,
				fmt.Errorf("event %d: bad duration %q: %w", e.EventID, e.Duration, err))
			continue
		}
		sid := serviceID
		if e.ServiceID != nil {
			sid = *e.ServiceID
		}
		raw, _ := json.Marshal(e)
		out = append(out, store.EPGEvent{
			ServiceID:  sid,
			EventID:    e.EventID,
			Start:      start.UTC(),
			Duration:   dur,
			Title:      e.Title,
			Synopsis:   e.Text,
			Raw:        string(raw),
			IngestedAt: now,
		})
	}
	return out, skipped
}

func parseHMS(s string) (time.Duration, error) {
	var h, m, sec int
	n, err := fmt.Sscanf(s, "%d:%d:%d", &h, &m, &sec)
	if err != nil || n != 3 {
		return 0, fmt.Errorf("expected HH:MM:SS, got %q", s)
	}
	return time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second, nil
}
