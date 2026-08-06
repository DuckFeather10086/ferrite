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
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Daemon mirrors configs/isdbd.example.toml.
type Daemon struct {
	HTTPPort    int    `toml:"http_port"`
	StorageRoot string `toml:"storage_root"`
	// Adapters is the original `adapters = [0]` form: bare numbers, every
	// one of them assumed to be a terrestrial frontend. Still accepted, and
	// still what most installs will write. AdapterSpecs is the same list
	// with the assumption spelled out; see AdapterList.
	Adapters []int `toml:"adapters"`
	// AdapterSpecs is the `[[adapter]]` form, which says what each frontend
	// can actually tune. Mutually exclusive with Adapters.
	AdapterSpecs []Adapter `toml:"adapter"`
	DvbrBin      string    `toml:"dvbr_bin"`
	B25Bin       string    `toml:"b25_bin"`
	FFmpegBin    string    `toml:"ffmpeg_bin"`
	FFprobeBin   string    `toml:"ffprobe_bin"`
	// AribCaptionBin decodes the ARIB caption PID into a WebVTT rendition
	// for live HLS. Empty disables captions.
	AribCaptionBin string   `toml:"arib_caption_bin"`
	ChannelsFile   string   `toml:"channels_file"`
	EPGCron        string   `toml:"epg_cron"`
	EPGChannels    []string `toml:"epg_channels"`

	// HLSRoot is where live sessions write their segments and playlists.
	// Empty keeps them under storage_root (StoragePath("hls")), which is
	// where they have always gone.
	//
	// The reason to move them is write volume, not latency: live HLS is a
	// rolling write-and-delete of the last dozen segments — at 6 Mbit/s
	// that is on the order of 65 GB a day — and it lands on the same disk,
	// interleaved with a recording's long sequential write. Pointing this
	// at a tmpfs takes that off the SSD entirely. Latency is decided by
	// segment length, not by which filesystem the segment is written to.
	//
	// $VARs are expanded, which is how a unit file gets to name its own
	// tmpfs without the config hardcoding a uid: RuntimeDirectory=ferrite
	// makes systemd both create the directory and export
	// $RUNTIME_DIRECTORY pointing at it — /run/ferrite for a system unit,
	// /run/user/{uid}/ferrite for a user one. See LiveRoot for what
	// happens when the variable is not set.
	HLSRoot string `toml:"hls_root"`

	// Live HLS A/V sync. ISDB-T muxes interleave audio ahead of the
	// first decodable video frame, so HLS comes up with a constant
	// audio/video offset. Before starting ffmpeg, the HLS session probes
	// the first audio + video PTS and shifts audio by their difference
	// (mirrors the legacy live_hls.py auto-offset). Probing is enabled
	// when FFprobeBin is non-empty and ProbeSeconds > 0.
	ProbeSeconds    float64 `toml:"probe_seconds"`     // ffprobe sampling window; default 5
	AudioOffsetBias float64 `toml:"audio_offset_bias"` // seconds added to the measured offset

	Transcode Transcode `toml:"transcode"`
	Live      Live      `toml:"live"`
}

// Live configures the on-demand quality tiers of live playback.
//
// Separate from Transcode, which is the post-pass over a finished
// recording: the two encodes want opposite things. A live encode is
// zerolatency, no B-frames, HLS-segmented; a recording's is faststart
// MP4 with the whole file to plan against. Sharing one table would mean
// arguments that are wrong for one of them whichever way it is written.
//
// Empty means one tier, hls.DefaultQuality — the encode this daemon did
// before tiers existed.
type Live struct {
	// InputArgs go before ffmpeg's -i, and are shared by every tier: a
	// hardware decode is set up once for the machine, not once per
	// bitrate. Empty is software decode.
	InputArgs []string `toml:"input_args"`

	// Quality is the tier table, keyed by the name that appears in URLs
	// and directories (`[live.quality.720p]`).
	Quality map[string]LiveQuality `toml:"quality"`
}

// LiveQuality is one tier.
type LiveQuality struct {
	// OutputArgs are ffmpeg's arguments after -i: filter chain and
	// encoder, as one setting for the reason internal/postprocess gives —
	// they go together, and naming only a codec produces an ffmpeg that
	// fails at runtime. Empty means the default software encode.
	//
	// Do not set -g here. It is computed from the segment length, because
	// every HLS segment has to begin with an IDR frame.
	OutputArgs []string `toml:"output_args"`

	// Label is what a viewer sees; defaults to the table key.
	Label string `toml:"label"`
	// Bandwidth is what the manifest advertises, in bits per second. It
	// tells a player what to expect rather than limiting the encoder, but
	// a wrong one makes it misjudge whether it can keep up.
	Bandwidth int `toml:"bandwidth"`
	// Order sorts the tiers for display and picks the default (lowest
	// first). Ties break by name, so a table that sets none is at least
	// stable across restarts.
	Order int `toml:"order"`

	// name is the table key, filled in by LiveQualities.
	name string
}

// LiveQualities returns the tiers in display order, the first being the
// default. Empty when the config declares none, which the HLS manager
// reads as its single built-in tier.
func (d *Daemon) LiveQualities() []LiveQuality {
	names := make([]string, 0, len(d.Live.Quality))
	for name := range d.Live.Quality {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := d.Live.Quality[names[i]], d.Live.Quality[names[j]]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return names[i] < names[j]
	})
	out := make([]LiveQuality, 0, len(names))
	for _, name := range names {
		q := d.Live.Quality[name]
		q.name = name
		out = append(out, q)
	}
	return out
}

// Name is the tier's key in the config table — its URL and directory
// component. Set by LiveQualities.
func (q LiveQuality) Name() string { return q.name }

// Adapter is one DVB adapter and the delivery systems its frontend can
// tune, as `[[adapter]]` in the TOML:
//
//	[[adapter]]
//	n       = 0
//	systems = ["ISDBT"]
//
// The systems list exists for mixed cards. A PT3 is 2×T + 2×S on one
// board, and a scheduler that does not know which is which will hand a BS
// channel to a terrestrial frontend, wait out the lock timeout, and report
// a weak signal — a failure that looks like an aerial problem and is not.
// Naming the systems turns that into an immediate, accurate refusal.
type Adapter struct {
	N int `toml:"n"`
	// Systems are DVB delivery-system names as they appear in
	// channels.json's DELIVERY_SYSTEM ("ISDBT", "ISDBS"). Empty means
	// terrestrial, which is what the bare `adapters = [n]` form has always
	// implied.
	Systems []string `toml:"systems"`
}

// DefaultDeliverySystem is what an adapter (or a channel) is taken to mean
// when it does not say: this is a terrestrial box until told otherwise.
const DefaultDeliverySystem = "ISDBT"

// AdapterList is the adapter inventory in one shape, whichever form the
// config used. `[[adapter]]` wins when present; otherwise the bare
// `adapters = [...]` list is read as terrestrial frontends.
//
// Names are upper-cased here so a config saying "isdbt" matches a
// channels.json saying "ISDBT" — the two files are written by different
// hands and nothing else forces them to agree on case.
func (d *Daemon) AdapterList() []Adapter {
	if len(d.AdapterSpecs) > 0 {
		out := make([]Adapter, 0, len(d.AdapterSpecs))
		for _, a := range d.AdapterSpecs {
			systems := make([]string, 0, len(a.Systems))
			for _, s := range a.Systems {
				if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
					systems = append(systems, s)
				}
			}
			if len(systems) == 0 {
				systems = []string{DefaultDeliverySystem}
			}
			out = append(out, Adapter{N: a.N, Systems: systems})
		}
		return out
	}
	out := make([]Adapter, 0, len(d.Adapters))
	for _, n := range d.Adapters {
		out = append(out, Adapter{N: n, Systems: []string{DefaultDeliverySystem}})
	}
	return out
}

// ISDBTAdapters is AdapterList's answer for a plain terrestrial box, for
// callers building a Pool without a config file (tests, one-off tools).
func ISDBTAdapters(ns ...int) []Adapter {
	out := make([]Adapter, 0, len(ns))
	for _, n := range ns {
		out = append(out, Adapter{N: n, Systems: []string{DefaultDeliverySystem}})
	}
	return out
}

// Transcode is the post-pass that runs after a recording finishes: the MP4 a
// browser can play, and the subtitle sidecars beside it.
type Transcode struct {
	// Enable is off by default. It is real work on a box whose first job is
	// to keep recording, so it is opted into rather than out of.
	Enable bool `toml:"enable"`

	// InputArgs go before ffmpeg's -i and OutputArgs after it: hardware
	// decode setup, then the filter chain and the encoder. They are one
	// setting rather than a codec name because the two go together — a
	// VAAPI encode wants deinterlace_vaapi and scale_vaapi, a software one
	// wants yadif and scale, and naming only the encoder gets you an
	// ffmpeg that fails at runtime.
	//
	// Empty means postprocess.DefaultTranscodeArgs: software H.264.
	InputArgs  []string `toml:"input_args"`
	OutputArgs []string `toml:"output_args"`

	// DeleteSource removes the .ts once the MP4 is written. Off by default:
	// the .ts is what came off the air, everything else is derived from it,
	// and it is the only one of them that cannot be made again.
	DeleteSource bool `toml:"delete_source"`
}

// Defaults returns a daemon config with sensible defaults for fields
// not present in the TOML.
func Defaults() Daemon {
	return Daemon{
		HTTPPort:       8010,
		StorageRoot:    "./var",
		Adapters:       []int{0},
		DvbrBin:        "dvb-rs",
		B25Bin:         "b25-rs",
		FFmpegBin:      "ffmpeg",
		FFprobeBin:     "ffprobe",
		AribCaptionBin: "arib-caption",
		EPGCron:        "0 */6 * * *",
		ProbeSeconds:   5,
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
	// The two adapter forms are exclusive, and the check has to ask the
	// TOML rather than the struct: Defaults() puts [0] in Adapters, so a
	// file that only writes [[adapter]] still arrives here with both set.
	if len(d.AdapterSpecs) > 0 {
		if meta.IsDefined("adapters") {
			return nil, fmt.Errorf("load config %s: set either `adapters` or `[[adapter]]`, "+
				"not both — [[adapter]] is the same list with each frontend's "+
				"delivery systems named", path)
		}
		d.Adapters = nil
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
	// The path is required; the file existing is not. A fresh install has
	// no channel list and gets one by scanning the band at first start
	// (internal/scan) — refusing to boot without the file would mean the
	// only way to produce it is to already have it. A path that is set and
	// unreadable still fails, one step later and with a better message,
	// when LoadChannels tries to read it.
	if d.ChannelsFile == "" {
		errs = append(errs, "channels_file is required")
	}
	if d.StorageRoot == "" {
		errs = append(errs, "storage_root is required")
	}
	if len(d.AdapterList()) == 0 {
		errs = append(errs, "adapters (or [[adapter]]) must list at least one adapter")
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

// LiveRoot returns the directory live HLS sessions write under, creating
// it if missing: hls_root when set, otherwise storage_root/hls.
//
// An hls_root naming an environment variable that is not set falls back to
// storage_root rather than failing. The variable to name is
// $RUNTIME_DIRECTORY, which exists only under systemd — so the same config
// has to keep working when the daemon is started by hand from the checkout,
// which is how it is run while developing.
func (d *Daemon) LiveRoot() (string, error) {
	if d.HLSRoot == "" {
		return d.StoragePath("hls")
	}
	root, ok := expandEnv(d.HLSRoot)
	if !ok {
		slog.Warn("config: hls_root names an environment variable that is not set; "+
			"live segments are going under storage_root instead",
			"hls_root", d.HLSRoot, "storage_root", d.StorageRoot)
		return d.StoragePath("hls")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("hls_root %q: %w", root, err)
	}
	return root, nil
}

// expandEnv substitutes $VAR / ${VAR} in s, reporting false if any of them
// is unset. os.ExpandEnv alone would turn "$RUNTIME_DIRECTORY/hls" into
// "/hls" — an absolute path in the wrong place, silently.
func expandEnv(s string) (string, bool) {
	ok := true
	out := os.Expand(s, func(name string) string {
		v, found := os.LookupEnv(name)
		if !found || v == "" {
			ok = false
		}
		return v
	})
	return out, ok
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
