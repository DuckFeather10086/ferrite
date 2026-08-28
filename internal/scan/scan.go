// Package scan builds channels.json by sweeping the terrestrial band.
//
// This is the step that decides whether anybody but its author can install
// this thing. The channels.json in the repo is a hand-kept frequency table
// for Kantō, and someone in Osaka — or someone in Tokyo with a different
// aerial — has nothing to start from and no obvious way to make one.
//
// The scan itself is `dvb-rs scan`, once per physical channel, merged into
// the same document each time. Everything here is the orchestration around
// it: which frequencies to try, how to hold the adapter, and how to report
// progress to something watching.
//
// Two things it deliberately does not do. It does not spawn dvb-rs behind
// the tuner Pool's back — a sweep occupies the frontend for ten minutes or
// more, and live playback and recordings have to be able to take it back
// (Pool.Reserve at background priority, one reservation per mux, so a
// preemption is never more than one transport away). And it does not write
// the document itself: `--merge --add-new` is what folds a transport in,
// which is what keeps a curated name from being overwritten by the
// broadcast one.
package scan

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// Japan's terrestrial UHF plan: physical channels 13–62, 6 MHz apart,
// with channel 13 centred at 473.142857 MHz. (The .142857 is 1/7 MHz —
// the ISDB-T carrier spacing puts the centre a seventh of a megahertz
// above the round number.)
const (
	FirstPhysical = 13
	LastPhysical  = 62
	channelWidth  = 6_000_000
	baseFrequency = 473_142_857
)

// FrequencyOf is the centre frequency in Hz of a physical channel number.
func FrequencyOf(physical int) int {
	return baseFrequency + (physical-FirstPhysical)*channelWidth
}

// muxTimeout bounds one transport. dvb-rs waits up to 15s for a frontend
// lock and then up to 5s per SI table, so a mux that is really there
// finishes in a few seconds and an empty frequency costs the full lock
// wait. This is only the backstop for a child that wedges.
const muxTimeout = 45 * time.Second

// ErrBusy is returned when a scan is already running. One at a time: they
// would fight over the adapter and over channels.json.
var ErrBusy = errors.New("scan: a channel scan is already running")

// ErrPreempted means something outranking the scan wanted the adapter.
// The sweep stops where it is; everything merged so far is already on
// disk, so a re-run picks up a complete document and only adds what is
// missing.
var ErrPreempted = errors.New("scan: the adapter was taken by something more important")

// errCannotRun is the dvb-rs binary not being there. Its own error, and
// its own exit from the sweep: a scan that cannot start its scanner
// otherwise reports fifty quiet frequencies and an empty band.
var errCannotRun = errors.New("scan: the dvb-rs binary could not be run")

// Reserver is the tuner.Pool seam (tests pass a fake).
type Reserver interface {
	Reserve(ctx context.Context, prio tuner.Priority, system string) (*tuner.Reservation, error)
}

// Progress is one step of a sweep, as it is reported to a watcher.
type Progress struct {
	// Physical is the UHF channel number being tried, and Frequency its
	// centre in Hz.
	Physical  int `json:"physical"`
	Frequency int `json:"frequency_hz"`
	// Done counts transports attempted, out of Total.
	Done  int `json:"done"`
	Total int `json:"total"`
	// Locked says whether this transport answered. Services is how many
	// records channels.json holds now — the number that actually tells a
	// person whether the scan is working.
	Locked   bool `json:"locked"`
	Services int  `json:"services"`
	// Error is set when this transport failed for a reason worth showing.
	// A frequency with nothing on it is not an error; it is the normal
	// case for most of the band.
	Error string `json:"error,omitempty"`
	// Finished marks the last event of a sweep.
	Finished bool `json:"finished,omitempty"`
}

// Runner sweeps the band. Zero First/Last means the whole UHF plan.
type Runner struct {
	DvbrBin      string
	ChannelsFile string

	// Tuners arbitrates the adapter. Required in the running daemon; nil
	// is for the first-run bootstrap, where nothing else exists yet and
	// Adapter says which frontend to use directly.
	Tuners  Reserver
	Adapter int

	// Backends maps adapter number → dvb-rs backend name ("dvb" |
	// "px4"), for the `--backend` flag on every dvb-rs scan. Absent
	// adapters are dvb. Set from config.BackendMap.
	Backends map[int]string

	First, Last int

	running sync.Mutex
	busy    bool
}

// Result summarizes a finished sweep.
type Result struct {
	Attempted int `json:"attempted"`
	Locked    int `json:"locked"`
	Services  int `json:"services"`
}

// Running reports whether a sweep is in flight.
func (r *Runner) Running() bool {
	r.running.Lock()
	defer r.running.Unlock()
	return r.busy
}

// Run sweeps the band, merging each transport that locks into
// ChannelsFile. onProgress, if non-nil, is called once per transport from
// Run's own goroutine — keep it quick or buffer inside it.
//
// The document is written after every transport rather than at the end,
// so a sweep that is preempted, cancelled or crashes still leaves behind
// everything it had found.
func (r *Runner) Run(ctx context.Context, onProgress func(Progress)) (Result, error) {
	if r.DvbrBin == "" || r.ChannelsFile == "" {
		return Result{}, errors.New("scan: DvbrBin and ChannelsFile are required")
	}
	r.running.Lock()
	if r.busy {
		r.running.Unlock()
		return Result{}, ErrBusy
	}
	r.busy = true
	r.running.Unlock()
	defer func() {
		r.running.Lock()
		r.busy = false
		r.running.Unlock()
	}()

	first, last := r.First, r.Last
	if first == 0 {
		first = FirstPhysical
	}
	if last == 0 {
		last = LastPhysical
	}
	if last < first {
		return Result{}, fmt.Errorf("scan: physical channel range %d–%d is empty", first, last)
	}

	// `--merge` reads the document before writing it, so there has to be
	// one. On a fresh install there is not, and an empty list is the
	// honest starting point.
	if err := ensureDocument(r.ChannelsFile); err != nil {
		return Result{}, err
	}

	total := last - first + 1
	var res Result
	slog.Info("scan: sweeping the UHF band",
		"from", first, "to", last, "output", r.ChannelsFile)

	for physical := first; physical <= last; physical++ {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		freq := FrequencyOf(physical)
		res.Attempted++

		locked, err := r.scanOne(ctx, freq)
		if errors.Is(err, ErrPreempted) || errors.Is(err, context.Canceled) ||
			errors.Is(err, errCannotRun) {
			// Stop the whole sweep rather than immediately re-reserving
			// for the next transport: whatever took the adapter wants it
			// for longer than the gap between two muxes.
			res.Services = countServices(r.ChannelsFile)
			report(onProgress, Progress{
				Physical: physical, Frequency: freq,
				Done: res.Attempted, Total: total,
				Services: res.Services, Error: err.Error(), Finished: true,
			})
			return res, err
		}
		if locked {
			res.Locked++
		}
		res.Services = countServices(r.ChannelsFile)

		p := Progress{
			Physical: physical, Frequency: freq,
			Done: res.Attempted, Total: total,
			Locked: locked, Services: res.Services,
		}
		// A frequency with no transmitter on it is the normal case for
		// most of the band, and reporting it as an error would make a
		// working scan look broken.
		if err != nil && locked {
			p.Error = err.Error()
		}
		report(onProgress, p)
	}

	slog.Info("scan: sweep complete",
		"attempted", res.Attempted, "locked", res.Locked, "services", res.Services)
	report(onProgress, Progress{
		Done: res.Attempted, Total: total, Services: res.Services, Finished: true,
	})
	return res, nil
}

// scanOne holds the adapter for one transport and runs dvb-rs against it.
// Reports whether the frontend locked.
func (r *Runner) scanOne(ctx context.Context, freq int) (bool, error) {
	adapter := r.Adapter
	var res *tuner.Reservation
	if r.Tuners != nil {
		var err error
		// Background priority, one reservation per transport. Live
		// playback and recordings outrank it, and holding the whole sweep
		// under a single reservation would mean the frontend is
		// unavailable for the ten minutes the sweep takes.
		res, err = r.Tuners.Reserve(ctx, tuner.PrioBackground, config.DefaultDeliverySystem)
		if err != nil {
			if errors.Is(err, tuner.ErrNoAdapter) {
				return false, fmt.Errorf("%w (adapter busy)", ErrPreempted)
			}
			return false, fmt.Errorf("scan: reserve adapter: %w", err)
		}
		defer res.Release()
		adapter = res.Adapter
	}

	cmdCtx, cancel := context.WithTimeout(ctx, muxTimeout)
	defer cancel()

	// Kill the child the instant something outranks us. The reservation
	// is released only after Run returns — i.e. after the process has
	// been reaped — which is what guarantees its flock on the adapter is
	// gone before the preemptor tunes.
	preempted := false
	if res != nil {
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-res.Preempted():
				preempted = true
				cancel()
			case <-watchDone:
			}
		}()
	}

	args := []string{
		"scan",
		"--backend", config.BackendFor(r.Backends, adapter),
		"--adapter", strconv.Itoa(adapter),
		"--frequency", strconv.Itoa(freq),
		"--bandwidth-hz", strconv.Itoa(channelWidth),
		"--delivery", config.DefaultDeliverySystem,
		"--output", r.ChannelsFile,
		// Fold this transport into the document: keep the names anyone
		// has curated, add the broadcast name as an alias, and create
		// records for services the document does not have yet. Without
		// --add-new a sweep over an empty document finds everything and
		// writes nothing.
		"--merge", "--add-new",
	}
	cmd := exec.CommandContext(cmdCtx, r.DvbrBin, args...)
	// Without this a grandchild holding the inherited stdout pipe keeps
	// Wait blocked and wedges the adapter for the next transport.
	cmd.WaitDelay = 3 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if preempted {
		return false, ErrPreempted
	}
	if err != nil {
		// The child never started — a dvbr_bin pointing at nothing. Fatal
		// for the sweep, because the alternative is fifty transports that
		// each look exactly like a quiet frequency and a scan that
		// cheerfully reports the whole band empty.
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			return false, fmt.Errorf("%w: cannot run %s: %w", errCannotRun, r.DvbrBin, err)
		}
		// A frequency with no transmitter times out waiting for a lock.
		// That is most of the band and it is not a fault.
		if isLockTimeout(stderr.String()) {
			slog.Debug("scan: nothing on this frequency", "hz", freq)
			return false, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("scan: dvb-rs at %d Hz: %w: %s",
			freq, err, lastLine(stderr.String()))
	}
	slog.Info("scan: transport found", "hz", freq, "adapter", adapter)
	return true, nil
}

// isLockTimeout recognizes dvb-rs's "no signal here" failure. Matched on
// the message because the exit status does not distinguish it from a real
// error, and the difference is between a scan that looks like it is
// working and one that looks like fifty failures.
//
// Both spellings, because dvb-rs's main returns its Result and Rust prints
// the Debug form ("Error: LockTimeout") while anything that formats the
// error itself gets the Display one ("frontend did not lock before
// timeout").
func isLockTimeout(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "locktimeout") ||
		strings.Contains(s, "lock timeout") ||
		strings.Contains(s, "did not lock")
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// ensureDocument creates an empty channels.json when there is none, so
// `scan --merge` has something to merge into.
func ensureDocument(path string) error {
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if err := os.WriteFile(path, []byte("{\n  \"version\": 1,\n  \"channels\": []\n}\n"), 0o644); err != nil {
		return fmt.Errorf("scan: creating %s: %w", path, err)
	}
	slog.Info("scan: created an empty channel list to merge into", "path", path)
	return nil
}

// countServices reports how many records the document holds now. Best
// effort: this only ever feeds a progress number.
func countServices(path string) int {
	c, err := config.LoadChannels(path)
	if err != nil {
		return 0
	}
	return c.Len()
}

func report(fn func(Progress), p Progress) {
	if fn != nil {
		fn(p)
	}
}
