package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Channels is the on-disk shape of channels.json — kept in lockstep
// with dvbr's config crate so a single file feeds both. We do not
// interpret the `tuning` map (dvbr is the only consumer of those
// keys); we just need name + aliases for lookup.
type Channels struct {
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
