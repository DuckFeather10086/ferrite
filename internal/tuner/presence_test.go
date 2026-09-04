package tuner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/config"
)

func TestSetPresent_AbsentAdapterIsNotOffered(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	if !p.SetPresent(0, false) {
		t.Fatal("SetPresent(false) should report a change")
	}
	if p.SetPresent(0, false) {
		t.Fatal("SetPresent to the state it is already in is not a change")
	}

	_, err := p.Acquire(context.Background(), "mx")
	// The one adapter this box has is configured for ISDBT and simply not
	// there, which is its own error: "no capable adapter" would send someone
	// shopping and "no adapter available" would send them looking for another
	// viewer to kick off.
	if !errors.Is(err, ErrAdapterUnplugged) {
		t.Fatalf("want ErrAdapterUnplugged, got %v", err)
	}
	if errors.Is(err, ErrNoCapableAdapter) {
		t.Fatal("an unplugged adapter must not read as an unsupported one")
	}

	if !p.SetPresent(0, true) {
		t.Fatal("SetPresent(true) should report a change")
	}
	l, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatalf("acquire after replug: %v", err)
	}
	l.Release()
}

func TestSetPresent_UnpluggingTearsDownTheHolder(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	l, err := p.Acquire(context.Background(), "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	if got := p.Status()[0].Channel; got != "mx" {
		t.Fatalf("adapter should be on mx, got %q", got)
	}

	p.SetPresent(0, false)

	// The slot is free again rather than still claiming to serve a channel
	// off hardware that has gone, and the lease has been told.
	st := p.Status()[0]
	if st.Channel != "" {
		t.Fatalf("a detached adapter still reports channel %q", st.Channel)
	}
	if st.Present {
		t.Fatal("status should report the adapter as absent")
	}
	select {
	case <-l.Preempted():
	default:
		t.Fatal("the holder was not told the adapter went away")
	}
}

func TestSetPresent_IgnoresAdaptersNobodyConfigured(t *testing.T) {
	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)
	if p.SetPresent(7, true) {
		t.Fatal("a stick at an unconfigured number is not this pool's to adopt")
	}
	if n := len(p.Status()); n != 1 {
		t.Fatalf("inventory changed: %d adapters", n)
	}
}

func TestDevicePath_IsPerBackend(t *testing.T) {
	for _, tc := range []struct {
		a    config.Adapter
		want string
	}{
		{config.Adapter{N: 0}, "/dev/dvb/adapter0/frontend0"},
		{config.Adapter{N: 2, Backend: "dvb"}, "/dev/dvb/adapter2/frontend0"},
		{config.Adapter{N: 1, Backend: "px4"}, "/dev/px4video1"},
	} {
		if got := DevicePath(tc.a); got != tc.want {
			t.Errorf("adapter %d backend %q: got %s want %s",
				tc.a.N, tc.a.Backend, got, tc.want)
		}
	}
}

// The watcher reports state, not transitions, so it converges on whatever is
// there without needing to have seen the change happen.
func TestPresenceWatcher_FollowsTheDeviceNode(t *testing.T) {
	dir := t.TempDir()
	node := filepath.Join(dir, "frontend0")

	ft := &fakeTuner{makeStream: emptyHold}
	p := newPool(t, 1, ft)

	w := &PresenceWatcher{
		Pool:     p,
		Adapters: []config.Adapter{{N: 0, Systems: []string{"ISDBT"}}},
		Interval: 10 * time.Millisecond,
		path:     func(config.Adapter) string { return node },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	waitPresent := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p.Status()[0].Present == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("adapter never became present=%v", want)
	}

	waitPresent(false)

	if err := os.WriteFile(node, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitPresent(true)

	if err := os.Remove(node); err != nil {
		t.Fatal(err)
	}
	waitPresent(false)
}
