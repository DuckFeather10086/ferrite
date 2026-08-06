package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Channels is the on-disk shape of channels.json — kept in lockstep
// with dvbr's config crate so a single file feeds both. We do not
// interpret the `tuning` map (dvbr is the only consumer of those
// keys); we just need name + aliases for lookup.
type Channels struct {
	// mu guards the two fields below, which a channel scan replaces
	// wholesale while the daemon is running. Read them through Find or
	// All; a direct read of the slice is only safe before anything is
	// serving.
	mu sync.RWMutex

	Version  int       `json:"version"`
	Channels []Channel `json:"channels"`
}

type Channel struct {
	Name             string            `json:"name"`
	Aliases          []string          `json:"aliases,omitempty"`
	LegacyZapSection string            `json:"legacy_zap_section,omitempty"`
	Tuning           map[string]string `json:"tuning"`
}

// LoadChannels reads channels.json at path.
func LoadChannels(path string) (*Channels, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read channels file: %w", err)
	}
	var c Channels
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse channels file: %w", err)
	}
	return &c, nil
}

// Find returns the channel matching needle by Name, any alias, or
// LegacyZapSection. Returns nil if no match.
//
// This is the Go mirror of dvbr's config::find_entry — keep the two
// in sync or channel lookup will diverge between the daemon and the
// tuner CLI.
func (c *Channels) Find(needle string) *Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.Channels {
		ch := &c.Channels[i]
		if ch.Name == needle || ch.LegacyZapSection == needle {
			return ch
		}
		for _, a := range ch.Aliases {
			if a == needle {
				return ch
			}
		}
	}
	return nil
}

// All returns the channel list for iteration. The slice is not copied —
// a scan replaces the whole backing array rather than mutating it, so a
// range over what this hands back is reading a consistent snapshot even
// if the file is rewritten underneath.
func (c *Channels) All() []Channel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Channels
}

// Len is All()'s length without materializing anything.
func (c *Channels) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.Channels)
}

// ReloadFrom re-reads path and swaps its contents in, so that everything
// already holding this *Channels sees the new list. A channel scan writes
// the file from a subprocess, which is the only reason this exists: the
// daemon's copy is otherwise loaded once at boot.
//
// Pointers a caller took from Find stay valid and stay stale — they point
// into the old backing array, which is not modified. That is the right
// trade here, since what holds one is a tune in flight and it should
// finish against the channel it started on.
func (c *Channels) ReloadFrom(path string) error {
	next, err := LoadChannels(path)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.Version, c.Channels = next.Version, next.Channels
	c.mu.Unlock()
	return nil
}

// DisplayName is the label to show a person. Every API call still takes
// Name — this is presentation only.
//
// The two are different because of how this file came to be. Records
// migrated from the legacy dvbv5 `.conf` carry the broadcast name as raw
// ARIB bytes read as Latin-1, so `name` can be mojibake
// (`NHKEFl1El5~`, `NHK7HBS2`) with the real name sitting in `aliases`
// where `scan --merge` put it. Other records are the reverse: a curated
// ASCII key (`asahi`, `NHK_G`) whose readable name is an alias, or a good
// `name` (`J：COMテレビ`) whose *alias* is the mojibake. And `_2`/`_3`
// suffixes that keep four services on one mux apart live on `name`.
//
// So: prefer this record's own name when it contains kana or kanji,
// otherwise the first alias that does, otherwise the name unchanged.
// Nothing tries to *detect* mojibake — picking the candidate that reads
// as Japanese gets every real case right, and a name that is deliberately
// ASCII (`TBS1`, `tvk1`) or a placeholder with no better alias
// (`515.14MHz#23864`) is simply left alone.
//
// Note this deliberately ignores U+3000 (ideographic space) and other
// CJK *punctuation*: `TOKYO MX1` has an alias `TOKYO　MX1` that differs
// only by that space, and it is not an improvement.
func (c *Channel) DisplayName() string {
	if c == nil {
		return ""
	}
	if hasJapanese(c.Name) {
		return c.Name
	}
	for _, a := range c.Aliases {
		if hasJapanese(a) {
			return a
		}
	}
	return c.Name
}

// hasJapanese reports whether s contains hiragana, katakana or a CJK
// ideograph — the marker of a real broadcast service name.
func hasJapanese(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x3041 && r <= 0x309f, // hiragana
			r >= 0x30a0 && r <= 0x30ff, // katakana
			r >= 0x4e00 && r <= 0x9fff: // CJK unified ideographs
			return true
		}
	}
	return false
}

// Frequency returns c's FREQUENCY tuning value verbatim ("" if absent).
//
// It is the mux identity: services sharing it share one transport stream,
// so they tune together and their EIT arrives together. Compared as a
// string on purpose — it is only ever used as a grouping key, and
// channels.json is the sole writer.
func (c *Channel) Frequency() string {
	if c == nil {
		return ""
	}
	return c.Tuning["FREQUENCY"]
}

// DeliverySystem returns c's DELIVERY_SYSTEM ("ISDBT", "ISDBS"),
// upper-cased, or "" when the record does not carry one.
//
// "" means *unconstrained*, not terrestrial: the tuner Pool uses this to
// pick a frontend that can receive the channel, and a record that does not
// say what it needs must not be locked out of the only adapter there is.
// Every record dvbr writes has the field; a hand-written one might not.
func (c *Channel) DeliverySystem() string {
	if c == nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(c.Tuning["DELIVERY_SYSTEM"]))
}

// ServiceID returns the parsed SERVICE_ID for c, or 0 if absent /
// unparseable. Used by callers that need to pass `-map 0:p:N` to
// ffmpeg.
func (c *Channel) ServiceID() uint16 {
	if c == nil {
		return 0
	}
	v, ok := c.Tuning["SERVICE_ID"]
	if !ok {
		return 0
	}
	var id uint16
	if _, err := fmt.Sscanf(v, "%d", &id); err != nil {
		return 0
	}
	return id
}
