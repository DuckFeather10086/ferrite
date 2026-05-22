// Package config loads daemon settings and the shared channels.json.
//
// channels.json is the same file consumed by dvbr — see channels.go
// and dvbr's config crate for the canonical schema. Daemon-specific
// settings (ports, storage paths, adapter inventory) live in a
// separate TOML.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Daemon mirrors configs/isdbd.example.toml.
type Daemon struct {
	HTTPPort     int      `toml:"http_port"`
	StorageRoot  string   `toml:"storage_root"`
	Adapters     []int    `toml:"adapters"`
	DvbrBin      string   `toml:"dvbr_bin"`
	B25Bin       string   `toml:"b25_bin"`
	FFmpegBin    string   `toml:"ffmpeg_bin"`
	ChannelsFile string   `toml:"channels_file"`
	EPGCron      string   `toml:"epg_cron"`
	EPGChannels  []string `toml:"epg_channels"`
}

// Defaults returns a daemon config with sensible defaults for fields
// not present in the TOML.
func Defaults() Daemon {
	return Daemon{
		HTTPPort:    8010,
		StorageRoot: "./var",
		Adapters:    []int{0},
		DvbrBin:     "dvbr",
		B25Bin:      "b25",
		FFmpegBin:   "ffmpeg",
		EPGCron:     "0 */6 * * *",
	}
}

// Load parses the daemon TOML at path. Missing fields fall back to
// Defaults. Returns an error if the file cannot be opened or has a
// type mismatch.
func Load(path string) (*Daemon, error) {
	d := Defaults()
	meta, err := toml.DecodeFile(path, &d)
	if err != nil {
		return nil, fmt.Errorf("load config %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		// Don't fail on unknown keys; just surface for visibility.
		// (Future addition for stricter loading: return an error here.)
		_ = undecoded
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

// Validate checks for empty required fields and existence of paths
// the daemon will need at startup.
func (d *Daemon) Validate() error {
	var errs []string
	if d.HTTPPort <= 0 || d.HTTPPort > 65535 {
		errs = append(errs, fmt.Sprintf("http_port %d out of range", d.HTTPPort))
	}
	if d.ChannelsFile == "" {
		errs = append(errs, "channels_file is required")
	} else if _, err := os.Stat(d.ChannelsFile); err != nil {
		errs = append(errs, fmt.Sprintf("channels_file %q: %v", d.ChannelsFile, err))
	}
	if d.StorageRoot == "" {
		errs = append(errs, "storage_root is required")
	}
	if len(d.Adapters) == 0 {
		errs = append(errs, "adapters must list at least one adapter id")
	}
	if len(errs) > 0 {
		return errors.New("config: " + joinLines(errs))
	}
	return nil
}

// StoragePath returns d.StorageRoot joined with name. The directory
// is created if missing.
func (d *Daemon) StoragePath(name string) (string, error) {
	dir := filepath.Join(d.StorageRoot, filepath.Dir(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(d.StorageRoot, name), nil
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "; "
		}
		out += s
	}
	return out
}
