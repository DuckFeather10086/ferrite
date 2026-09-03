package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Preflight is what the daemon found when it looked for the programs it
// spawns, at startup, before anything needed them.
//
// The daemon does not do its own work: it tunes through dvb-rs, descrambles
// through b25-rs, decodes captions through arib-caption and encodes through
// ffmpeg. A missing one of those is not a startup error — the daemon starts
// fine, serves its UI, answers its API — it is a *runtime* error, discovered
// hours later by whatever first tries to spawn it, in a warning line addressed
// to that subsystem.
//
// Which is how this box lost three days. `cargo clean` took target/ with it,
// the EPG refresher reported `fork/exec ./target/release/dvb-rs: no such file
// or directory` once per channel every six hours, and nothing else ever said
// anything: the web UI looked normal, /api/status looked normal, and the guide
// simply stopped getting newer. So the check happens once, at the start, where
// it can be stated as a fact about the daemon rather than as the failure of a
// single EPG pass — and it is carried on /api/status so a client can show it
// without reading the journal.
type Preflight struct {
	// Problems are the missing or unusable programs, worst first: what the
	// setting is called, what it points at, and what stops working.
	Problems []Problem `json:"problems,omitempty"`
}

// Problem is one program the daemon cannot spawn.
type Problem struct {
	Setting string `json:"setting"` // the TOML key, e.g. "dvbr_bin"
	Path    string `json:"path"`    // what it is set to
	Err     string `json:"error"`   // why it cannot be spawned
	Breaks  string `json:"breaks"`  // what stops working without it
	Fatal   bool   `json:"fatal"`   // nothing works at all without this one
}

func (p Problem) String() string {
	return fmt.Sprintf("%s (%s): %s — %s", p.Setting, p.Path, p.Err, p.Breaks)
}

// OK reports whether every program the daemon spawns is there.
func (p Preflight) OK() bool { return len(p.Problems) == 0 }

// Fatal reports whether one of the missing programs is one the daemon cannot
// do anything at all without.
func (p Preflight) Fatal() bool {
	for _, x := range p.Problems {
		if x.Fatal {
			return true
		}
	}
	return false
}

// Summary is the one-line form, for a banner or a log line.
func (p Preflight) Summary() string {
	if p.OK() {
		return ""
	}
	names := make([]string, 0, len(p.Problems))
	for _, x := range p.Problems {
		names = append(names, x.Setting)
	}
	return strings.Join(names, ", ") + ": not runnable — see the daemon log"
}

// CheckBinaries looks for every program the daemon spawns.
//
// A bare name (`ffmpeg`) is looked up on $PATH, the way exec.Command would; a
// path with a separator in it is checked where it points. An empty setting is
// not a problem — an empty b25_bin means "no card, do not descramble" and an
// empty arib_caption_bin means "no captions", both of which are configurations
// rather than faults.
func (d *Daemon) CheckBinaries() Preflight {
	var pf Preflight
	for _, c := range []struct {
		setting string
		path    string
		breaks  string
		fatal   bool
	}{
		// Without dvb-rs there is no television: no live, no recording, no
		// EPG, no scan. Everything else is a degradation.
		{"dvbr_bin", d.DvbrBin, "everything — no live, no recordings, no EPG, no scan", true},
		{"ffmpeg_bin", d.FFmpegBin, "live HLS and the recording transcode", true},
		{"b25_bin", d.B25Bin, "descrambling — scrambled channels play as noise", false},
		{"arib_caption_bin", d.AribCaptionBin, "subtitles, live and recorded", false},
		{"ffprobe_bin", d.FFprobeBin, "the A/V offset measurement and caption timing", false},
	} {
		if c.path == "" {
			continue
		}
		if err := runnable(c.path); err != nil {
			pf.Problems = append(pf.Problems, Problem{
				Setting: c.setting,
				Path:    c.path,
				Err:     err.Error(),
				Breaks:  c.breaks,
				Fatal:   c.fatal,
			})
		}
	}
	return pf
}

// runnable reports whether path names something this process could exec.
func runnable(path string) error {
	if !strings.ContainsRune(path, filepath.Separator) {
		if _, err := exec.LookPath(path); err != nil {
			return fmt.Errorf("not on $PATH")
		}
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no such file")
		}
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("is a directory")
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("not executable")
	}
	return nil
}
