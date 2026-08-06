package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/DuckFeather10086/ferrite/internal/config"
	"github.com/DuckFeather10086/ferrite/internal/tuner"
)

// The frequency table is the part a person cannot check by looking at it,
// and getting it wrong makes every scan find nothing.
func TestFrequencyOf(t *testing.T) {
	cases := map[int]int{
		13: 473_142_857, // the anchor
		14: 479_142_857,
		27: 557_142_857, // NHK総合 in Kantō
		62: 767_142_857, // the top of the plan
	}
	for physical, want := range cases {
		if got := FrequencyOf(physical); got != want {
			t.Errorf("FrequencyOf(%d) = %d, want %d", physical, got, want)
		}
	}
}

// stubDvbr writes a shell script standing in for `dvb-rs scan`: it
// appends a record for the frequencies in `withSignal` and exits with a
// lock timeout for the rest, which is what the band really looks like.
func stubDvbr(t *testing.T, withSignal ...int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	var arms strings.Builder
	for i, hz := range withSignal {
		fmt.Fprintf(&arms, "  %d) sid=%d ;;\n", hz, 1000+i)
	}
	script := `#!/bin/sh
freq=""
out="channels.json"
while [ $# -gt 0 ]; do
  case "$1" in
    --frequency) freq="$2"; shift 2 ;;
    --output) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
sid=""
case "$freq" in
` + arms.String() + `  *) ;;
esac
if [ -z "$sid" ]; then
  echo "Error: LockTimeout" >&2
  exit 1
fi
python3 - "$out" "$freq" "$sid" <<'EOF'
import json, sys
path, freq, sid = sys.argv[1], sys.argv[2], sys.argv[3]
doc = json.load(open(path))
doc["channels"].append({
    "name": "svc" + sid,
    "tuning": {"DELIVERY_SYSTEM": "ISDBT", "FREQUENCY": freq,
               "BANDWIDTH_HZ": "6000000", "SERVICE_ID": sid},
})
json.dump(doc, open(path, "w"))
EOF
`
	path := filepath.Join(t.TempDir(), "dvb-rs-stub")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The whole point: start with nothing, end with a channel list.
func TestRun_BuildsAListFromNothing(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "channels.json")
	// Physical 13 and 15 carry a transport; 14 and 16 are empty.
	r := &Runner{
		DvbrBin:      stubDvbr(t, FrequencyOf(13), FrequencyOf(15)),
		ChannelsFile: out,
		First:        13, Last: 16,
	}

	var seen []Progress
	res, err := r.Run(context.Background(), func(p Progress) { seen = append(seen, p) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Attempted != 4 || res.Locked != 2 || res.Services != 2 {
		t.Fatalf("got %+v, want 4 attempted / 2 locked / 2 services", res)
	}

	chans, err := config.LoadChannels(out)
	if err != nil {
		t.Fatalf("the scan must leave a loadable document: %v", err)
	}
	if chans.Len() != 2 {
		t.Fatalf("channels.json holds %d records, want 2", chans.Len())
	}

	// One event per transport plus the terminator, and an empty
	// frequency must not be reported as an error — most of the band is
	// empty and a scan full of red is a scan nobody trusts.
	if len(seen) != 5 {
		t.Fatalf("got %d progress events, want 5", len(seen))
	}
	for _, p := range seen {
		if p.Error != "" {
			t.Errorf("physical %d reported an error for a quiet frequency: %s", p.Physical, p.Error)
		}
	}
	if !seen[len(seen)-1].Finished {
		t.Error("the last event should be marked finished")
	}
}

// A second sweep over a list somebody has curated must not disturb it.
// The merge itself is dvb-rs's (and tested there); this checks the Runner
// does not create, truncate or replace the document around it.
func TestRun_KeepsAnExistingDocument(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "channels.json")
	existing := `{"version":1,"channels":[{"name":"asahi","aliases":["テレビ朝日"],` +
		`"tuning":{"SERVICE_ID":"1064","FREQUENCY":"539142857"}}]}`
	if err := os.WriteFile(out, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Runner{
		DvbrBin:      stubDvbr(t, FrequencyOf(13)),
		ChannelsFile: out,
		First:        13, Last: 13,
	}
	if _, err := r.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	chans, err := config.LoadChannels(out)
	if err != nil {
		t.Fatal(err)
	}
	if ch := chans.Find("asahi"); ch == nil {
		t.Fatal("the curated record is gone")
	} else if len(ch.Aliases) != 1 || ch.Aliases[0] != "テレビ朝日" {
		t.Fatalf("aliases changed: %v", ch.Aliases)
	}
	if chans.Len() != 2 {
		t.Fatalf("expected the curated record plus the scanned one, got %d", chans.Len())
	}
}

// Every transport is claimed through the Pool, at background priority, so
// live playback and recordings can take the adapter back mid-sweep.
// Spawning dvb-rs directly would take dvbr's flock behind the Pool's back
// and starve them for the ten minutes a sweep runs.
func TestRun_ReservesThePoolPerTransport(t *testing.T) {
	pool := tuner.NewPool(nil, &config.Channels{}, config.ISDBTAdapters(0), 4)
	rec := &countingReserver{Reserver: pool}

	dir := t.TempDir()
	r := &Runner{
		DvbrBin:      stubDvbr(t, FrequencyOf(13)),
		ChannelsFile: filepath.Join(dir, "channels.json"),
		Tuners:       rec,
		First:        13, Last: 15,
	}
	if _, err := r.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if rec.calls != 3 {
		t.Fatalf("reserved %d times for 3 transports; one reservation for the whole "+
			"sweep would hold the adapter for its full duration", rec.calls)
	}
	if rec.system != config.DefaultDeliverySystem {
		t.Errorf("reserved for %q, want %q", rec.system, config.DefaultDeliverySystem)
	}
	// Every reservation released: the next sweep, and everything else,
	// needs the adapter back.
	if st := pool.Status(); st[0].Reserved {
		t.Error("the adapter is still reserved after the sweep")
	}
}

// Being outranked stops the sweep rather than fighting for the frontend —
// and what was already found stays on disk.
func TestRun_StopsWhenTheAdapterIsTakenAway(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "channels.json")
	r := &Runner{
		DvbrBin:      stubDvbr(t, FrequencyOf(13), FrequencyOf(14)),
		ChannelsFile: out,
		// Busy from the second transport on.
		Tuners: &busyAfter{n: 1},
		First:  13, Last: 20,
	}

	var last Progress
	res, err := r.Run(context.Background(), func(p Progress) { last = p })
	if !errors.Is(err, ErrPreempted) {
		t.Fatalf("Run = %v, want ErrPreempted", err)
	}
	if res.Attempted != 2 {
		t.Fatalf("attempted %d transports, want to have stopped after 2", res.Attempted)
	}
	if !last.Finished || last.Error == "" {
		t.Errorf("the final event should say the sweep stopped and why: %+v", last)
	}
	// The first transport's services were merged before the sweep ended.
	if chans, err := config.LoadChannels(out); err != nil || chans.Len() != 1 {
		t.Fatalf("progress before the preemption was lost: %v %v", chans, err)
	}
}

// Two sweeps at once would fight over the adapter and over channels.json.
func TestRun_RefusesASecondSweep(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{
		DvbrBin:      stubDvbr(t),
		ChannelsFile: filepath.Join(dir, "channels.json"),
		First:        13, Last: 14,
	}

	started := make(chan struct{})
	var second error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(context.Background(), func(Progress) {
			select {
			case <-started:
			default:
				close(started)
				// Hold here so the second Run below overlaps.
				_, second = r.Run(context.Background(), nil)
			}
		})
	}()
	<-started
	<-done

	if !errors.Is(second, ErrBusy) {
		t.Fatalf("the overlapping Run returned %v, want ErrBusy", second)
	}
}

// A dvbr_bin pointing at nothing must fail loudly. Every transport would
// otherwise look exactly like a quiet frequency, and the sweep would
// report the whole band empty — which is indistinguishable from a
// disconnected aerial and sends you looking in the wrong place.
func TestRun_FailsFastWhenTheScannerIsMissing(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{
		DvbrBin:      filepath.Join(dir, "not-installed"),
		ChannelsFile: filepath.Join(dir, "channels.json"),
		First:        13, Last: 62,
	}
	res, err := r.Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "could not be run") {
		t.Fatalf("Run = %v, want a could-not-run error", err)
	}
	if res.Attempted != 1 {
		t.Fatalf("attempted %d transports; it should have given up on the first", res.Attempted)
	}
}

type countingReserver struct {
	Reserver
	calls  int
	system string
}

func (c *countingReserver) Reserve(ctx context.Context, prio tuner.Priority, system string) (*tuner.Reservation, error) {
	if prio != tuner.PrioBackground {
		panic("a scan must not outrank anything: " + prio.String())
	}
	c.calls++
	c.system = system
	return c.Reserver.Reserve(ctx, prio, system)
}

// busyAfter grants n reservations and then reports the adapter busy.
type busyAfter struct {
	n    int
	seen int
}

func (b *busyAfter) Reserve(ctx context.Context, prio tuner.Priority, system string) (*tuner.Reservation, error) {
	if b.seen >= b.n {
		return nil, tuner.ErrNoAdapter
	}
	b.seen++
	pool := tuner.NewPool(nil, &config.Channels{}, config.ISDBTAdapters(0), 4)
	return pool.Reserve(ctx, prio, system)
}
