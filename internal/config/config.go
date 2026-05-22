// Package config loads daemon settings and the shared channels.json.
//
// channels.json is the same file consumed by `dvbr` — see
// github.com/DuckFeather10086/dvbr's config crate for the canonical
// schema. Daemon-specific settings (ports, storage paths, adapter
// inventory) live in a separate TOML.
package config

type Daemon struct {
	HTTPPort     int      `toml:"http_port"`
	StorageRoot  string   `toml:"storage_root"`
	Adapters     []int    `toml:"adapters"`
	DvbrBin      string   `toml:"dvbr_bin"`
	B25Bin       string   `toml:"b25_bin"`
	FFmpegBin    string   `toml:"ffmpeg_bin"`
	ChannelsFile string   `toml:"channels_file"`
	EPGCron      string   `toml:"epg_cron"`     // cron expression, e.g. "0 */6 * * *"
	EPGChannels  []string `toml:"epg_channels"` // services to refresh
}

// Load parses the daemon TOML at path. Not implemented.
func Load(path string) (*Daemon, error) {
	panic("not implemented")
}
