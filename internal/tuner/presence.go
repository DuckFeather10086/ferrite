package tuner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
)

// DefaultPresenceInterval is how often the watcher looks for the device nodes
// of the configured adapters.
//
// Two seconds because this is a television: the cost of noticing late is that
// a channel change during those two seconds fails with "no adapter available"
// instead of "the tuner is not attached", and the cost of noticing often is
// one stat per adapter. There is nothing to gain from being quicker and
// nothing to save by being slower.
const DefaultPresenceInterval = 2 * time.Second

// PresenceWatcher keeps a Pool's idea of which adapters are attached in step
// with what is actually on the filesystem.
//
// Polling rather than udev, deliberately. The inventory is the config's — this
// only has to answer "is adapter N there?" for a handful of known numbers, so
// it is a couple of stats every two seconds, against a netlink socket, a
// uevent parser and a device-to-adapter mapping that would have to be written
// twice because the px4 backend does not use /dev/dvb at all. It also reports
// the state rather than the transitions, which means it recovers by itself
// from a missed event and needs nothing at startup.
type PresenceWatcher struct {
	Pool *Pool
	// Adapters is the configured inventory, which is what decides both the
	// numbers to look for and the path shape to look for them at.
	Adapters []config.Adapter
	// Interval is the poll period; zero means DefaultPresenceInterval.
	Interval time.Duration
	// path resolves an adapter to the node to stat. Nil means DevicePath,
	// which is what production wants; tests point it at a file they can
	// create and remove, there being no way to unplug a USB stick from a
	// unit test.
	path func(config.Adapter) string
}

func (w *PresenceWatcher) nodeFor(a config.Adapter) string {
	if w.path != nil {
		return w.path(a)
	}
	return DevicePath(a)
}

// DevicePath is the node whose existence means this adapter is attached.
//
// One node per backend, because the two do not share a device model: the DVB
// API puts a directory per adapter under /dev/dvb and the out-of-tree px4_drv
// exposes a flat chardev. The frontend rather than the adapter directory on
// the DVB side, because an adapter directory can outlive the hardware for as
// long as something holds a file in it open.
func DevicePath(a config.Adapter) string {
	if a.BackendName() == "px4" {
		return fmt.Sprintf("/dev/px4video%d", a.N)
	}
	return fmt.Sprintf("/dev/dvb/adapter%d/frontend0", a.N)
}

// Present reports whether an adapter's device node is there now.
func Present(a config.Adapter) bool {
	_, err := os.Stat(DevicePath(a))
	return err == nil
}

// Run polls until ctx is canceled. The first pass happens immediately, so a
// daemon that starts with the stick unplugged says so rather than waiting out
// a tick and then failing the first tune with the wrong reason.
func (w *PresenceWatcher) Run(ctx context.Context) error {
	if w.Pool == nil || len(w.Adapters) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}
	every := w.Interval
	if every <= 0 {
		every = DefaultPresenceInterval
	}

	w.sweep(true)

	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			w.sweep(false)
		}
	}
}

// sweep tells the Pool the current state of every configured adapter. first
// says this is the startup pass, whose only job is to report — an adapter that
// was never there has nothing to tear down, and logging it as a detachment
// would be a lie.
func (w *PresenceWatcher) sweep(first bool) {
	for _, a := range w.Adapters {
		node := w.nodeFor(a)
		_, err := os.Stat(node)
		present := err == nil
		if first && !present {
			slog.Error("tuner: a configured adapter is not attached",
				"adapter", a.N, "path", node, "backend", a.BackendName())
		}
		w.Pool.setPresent(a.N, present, !first)
	}
}

// Missing is the configured adapters that are not attached right now, for
// /api/status. Returned in config order.
func Missing(adapters []config.Adapter) []config.Adapter {
	var out []config.Adapter
	for _, a := range adapters {
		if !Present(a) {
			out = append(out, a)
		}
	}
	return out
}
