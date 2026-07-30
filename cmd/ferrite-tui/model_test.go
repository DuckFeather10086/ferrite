package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestModel() Model {
	// A player binary that is never reachable because the environment has no
	// display: Play always declines with ErrNoDisplay, so tests never spawn
	// anything.
	m := NewModel(NewClient("http://tuner.test:8010"), &Player{Bin: "mpv", Env: []string{}})
	m.ready = true
	m.width, m.height = 100, 30
	return m
}

func withChannels(m Model, names ...string) Model {
	chans := make([]Channel, len(names))
	for i, n := range names {
		chans[i] = Channel{Name: n, ServiceID: uint16(1000 + i)}
	}
	next, _ := m.Update(channelsMsg{channels: chans})
	return next.(Model)
}

func key(m Model, k string) (Model, tea.Cmd) {
	var msg tea.KeyMsg
	switch k {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, cmd := m.Update(msg)
	return next.(Model), cmd
}

func TestCursorStaysInBounds(t *testing.T) {
	m := withChannels(newTestModel(), "a", "b", "c")

	m, _ = key(m, "k") // already at the top
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", m.cursor)
	}
	for i := 0; i < 10; i++ {
		m, _ = key(m, "j")
	}
	if m.cursor != 2 {
		t.Fatalf("cursor = %d, want 2 (last)", m.cursor)
	}
	if ch, ok := m.selected(); !ok || ch.Name != "c" {
		t.Fatalf("selected = %+v %v", ch, ok)
	}
}

// An empty channel list must not make selection or rendering panic.
func TestEmptyChannelListIsSafe(t *testing.T) {
	m := newTestModel()
	if _, ok := m.selected(); ok {
		t.Fatal("nothing should be selected")
	}
	m, _ = key(m, "j")
	m, _ = key(m, "enter") // must be a no-op, not a nil deref
	if m.busy != "" {
		t.Fatalf("busy = %q, want empty", m.busy)
	}
	_ = m.View()
}

// Pressing enter twice must not fire two tunes; the second is ignored while
// the first is in flight.
func TestEnterIsIgnoredWhileBusy(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")

	m, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("first enter should start a switch")
	}
	if !strings.Contains(m.busy, "asahi") {
		t.Fatalf("busy = %q", m.busy)
	}

	m2, cmd2 := key(m, "enter")
	if cmd2 != nil {
		t.Fatal("second enter should be ignored while busy")
	}
	if m2.busy != m.busy {
		t.Fatalf("busy changed to %q", m2.busy)
	}
}

// The daemon started the stream; only local playback failed. That must read
// as information plus a usable URL, not as an error.
func TestNoDisplayReportsURLInsteadOfFailing(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m.busy = "tuning asahi"

	next, _ := m.Update(switchMsg{result: SwitchResult{Channel: "asahi"}})
	m = next.(Model)

	if m.failure != "" {
		t.Fatalf("failure = %q, want none", m.failure)
	}
	if !strings.Contains(m.message, "http://tuner.test:8010/api/live/asahi.m3u8") {
		t.Fatalf("message = %q, want the absolute stream URL", m.message)
	}
	if m.busy != "" {
		t.Fatalf("busy = %q, want cleared", m.busy)
	}
}

func TestBusyTunerIsExplained(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m.busy = "tuning asahi"

	err := &APIError{Status: http.StatusConflict, Message: "tuner busy: held by a recording"}
	next, _ := m.Update(switchMsg{err: err})
	m = next.(Model)

	if !strings.HasPrefix(m.failure, "tuner busy:") {
		t.Fatalf("failure = %q", m.failure)
	}
	if m.busy != "" {
		t.Fatalf("busy = %q, want cleared", m.busy)
	}
}

// R with nothing recording should say so rather than posting a stop for a
// made-up id.
func TestStopRecordingWithNothingActive(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m, cmd := key(m, "R")
	if cmd != nil {
		t.Fatal("no request should be issued")
	}
	if m.failure != "nothing is recording" {
		t.Fatalf("failure = %q", m.failure)
	}
}

// With several active recordings, R targets the newest.
func TestStopTargetPicksNewestActive(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	next, _ := m.Update(statusMsg{status: Status{Recording: []int64{4, 9, 7}}})
	m = next.(Model)

	id, ok := m.stopTarget()
	if !ok || id != 9 {
		t.Fatalf("stopTarget = %d %v, want 9", id, ok)
	}
}

// In the recordings view, R targets the highlighted row — and refuses rows
// that are already finished.
func TestStopTargetInRecordingsView(t *testing.T) {
	m := newTestModel()
	m.view = viewRecordings
	next, _ := m.Update(recordingsMsg{recordings: []Recording{
		{ID: 11, State: "recording"},
		{ID: 10, State: "done"},
	}})
	m = next.(Model)

	if id, ok := m.stopTarget(); !ok || id != 11 {
		t.Fatalf("stopTarget = %d %v, want 11", id, ok)
	}
	m.recCursor = 1
	if _, ok := m.stopTarget(); ok {
		t.Fatal("a finished recording must not be stoppable")
	}
}

// Shrinking the list must not leave the cursor pointing past the end.
func TestRecordingsCursorClampsWhenListShrinks(t *testing.T) {
	m := newTestModel()
	m.view = viewRecordings
	next, _ := m.Update(recordingsMsg{recordings: []Recording{{ID: 1}, {ID: 2}, {ID: 3}}})
	m = next.(Model)
	m.recCursor = 2

	next, _ = m.Update(recordingsMsg{recordings: []Recording{{ID: 1}}})
	m = next.(Model)
	if m.recCursor != 0 {
		t.Fatalf("recCursor = %d, want 0", m.recCursor)
	}
	_ = m.View()
}

// The list shows the daemon's label; every request still carries the
// canonical name. Both matter: channels.json is full of keys like
// `NHKEFl1El5~` that nobody should have to read.
func TestChannelListShowsDisplayNamesButActsOnNames(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(channelsMsg{channels: []Channel{
		{Name: "NHKEFl1El5~", DisplayName: "NHKEテレ1東京", ServiceID: 1032},
		{Name: "TBS1", ServiceID: 1048}, // no display_name: falls back
	}})
	m = next.(Model)

	out := m.View()
	if !strings.Contains(out, "NHKEテレ1東京") {
		t.Fatalf("the readable name is missing:\n%s", out)
	}
	if !strings.Contains(out, "TBS1") {
		t.Fatalf("a channel without display_name must still render:\n%s", out)
	}
	// The canonical name stays visible next to the guide, since that is
	// what an API call or `dvb-rs tune` takes.
	if !strings.Contains(out, "NHKEFl1El5~") {
		t.Fatalf("the canonical name should still be on screen:\n%s", out)
	}

	m2, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("enter should switch")
	}
	if !strings.Contains(m2.busy, "NHKEテレ1東京") {
		t.Fatalf("busy = %q, want the readable name", m2.busy)
	}
	// switchTo got the canonical name — the daemon would 404 on the label
	// unless it happens to be an alias.
	if got := m.client.PlaylistURL(m.channels[0].Name); !strings.Contains(got, "NHKEFl1El5") {
		t.Fatalf("request URL = %q", got)
	}
}

// Adapter status and recording rows come back keyed by canonical name;
// they have to be relabelled or the status bar disagrees with the list.
func TestStatusAndRecordingsUseDisplayNames(t *testing.T) {
	m := newTestModel()
	next, _ := m.Update(channelsMsg{channels: []Channel{
		{Name: "asahi", DisplayName: "テレビ朝日", ServiceID: 1064},
	}})
	m = next.(Model)
	next, _ = m.Update(statusMsg{status: Status{
		Adapters: []Adapter{{Adapter: 0, Channel: "asahi", Refs: 1, Prio: "live"}},
	}})
	m = next.(Model)

	if bar := m.renderStatusBar(); !strings.Contains(bar, "テレビ朝日") {
		t.Fatalf("status bar = %q", bar)
	}

	m = withRecordings(m, Recording{ID: 1, State: "done", Channel: "asahi", Title: "報道"})
	if out := m.View(); !strings.Contains(out, "テレビ朝日") {
		t.Fatalf("recordings view is missing the label:\n%s", out)
	}

	// A recording naming a channel that has left channels.json still shows.
	m = withRecordings(m, Recording{ID: 2, State: "done", Channel: "gone-channel"})
	if out := m.View(); !strings.Contains(out, "gone-channel") {
		t.Fatalf("unknown channel should pass through:\n%s", out)
	}
}

func withRecordings(m Model, recs ...Recording) Model {
	m.view = viewRecordings
	next, _ := m.Update(recordingsMsg{recordings: recs})
	return next.(Model)
}

// Deleting unlinks a file, so it takes two keystrokes. The first only arms
// the prompt — no request may leave until y.
func TestDeleteAsksBeforeDeleting(t *testing.T) {
	m := withRecordings(newTestModel(), Recording{ID: 42, State: "done", Channel: "mx"})

	m, cmd := key(m, "d")
	if cmd != nil {
		t.Fatal("d alone must not issue a delete")
	}
	if m.pendingDelete != 42 {
		t.Fatalf("pendingDelete = %d, want 42", m.pendingDelete)
	}
	if !strings.Contains(m.View(), "delete recording 42") {
		t.Fatalf("the prompt is not on screen:\n%s", m.View())
	}

	m, cmd = key(m, "y")
	if cmd == nil {
		t.Fatal("y should issue the delete")
	}
	if m.pendingDelete != 0 {
		t.Fatalf("pendingDelete = %d, want cleared", m.pendingDelete)
	}
}

// Any other key cancels — and is swallowed, so answering the prompt with
// "r" can't start a recording by accident.
func TestDeleteConfirmSwallowsOtherKeys(t *testing.T) {
	m := withRecordings(newTestModel(), Recording{ID: 42, State: "done"})
	m = withChannels(m, "asahi")
	m.view = viewRecordings

	m, _ = key(m, "d")
	m, cmd := key(m, "r")
	if cmd != nil {
		t.Fatal("the cancel keystroke must not also run a command")
	}
	if m.pendingDelete != 0 {
		t.Fatal("pendingDelete should be cleared")
	}
	if m.message != "delete canceled" {
		t.Fatalf("message = %q", m.message)
	}
}

// d only means anything where a recording is visible; in the channel list
// the cursor is on a channel.
func TestDeleteIsInertInChannelsView(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m = withRecordings(m, Recording{ID: 42, State: "done"})
	m.view = viewChannels

	m, cmd := key(m, "d")
	if cmd != nil || m.pendingDelete != 0 {
		t.Fatalf("d armed a delete from the channels view: pending=%d", m.pendingDelete)
	}
}

// The daemon refuses to delete a running recording; that refusal is the
// actionable one, so it is shown verbatim.
func TestDeleteConflictIsExplained(t *testing.T) {
	m := withRecordings(newTestModel(), Recording{ID: 42, State: "recording"})
	m.busy = "deleting recording 42"

	err := &APIError{Status: http.StatusConflict, Message: "recording 42 is still running — POST /api/record/42/stop first"}
	next, _ := m.Update(deleteRecMsg{id: 42, err: err})
	m = next.(Model)

	if !strings.HasPrefix(m.failure, "cannot delete:") ||
		!strings.Contains(m.failure, "/stop") {
		t.Fatalf("failure = %q", m.failure)
	}
	if m.busy != "" {
		t.Fatalf("busy = %q, want cleared", m.busy)
	}
}

// A row whose file was already gone still counts as deleted — the message
// says which so the file isn't left looking lost.
func TestDeleteReportsWhetherAFileWentWithIt(t *testing.T) {
	m := withRecordings(newTestModel(), Recording{ID: 42, State: "done"})

	next, _ := m.Update(deleteRecMsg{id: 42, fileDeleted: false})
	got := next.(Model)
	if got.failure != "" || !strings.Contains(got.message, "no file") {
		t.Fatalf("message = %q failure = %q", got.message, got.failure)
	}

	next, _ = m.Update(deleteRecMsg{id: 42, fileDeleted: true})
	got = next.(Model)
	if !strings.Contains(got.message, "and its file") {
		t.Fatalf("message = %q", got.message)
	}
}

// In the recordings view enter plays the highlighted file — it must not
// fall through to tuning whatever the channel cursor is on.
func TestEnterInRecordingsViewPlaysTheFile(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m = withRecordings(m, Recording{ID: 7, State: "done", Channel: "mx", Title: "報道ステーション"})

	m, cmd := key(m, "enter")
	if cmd != nil {
		t.Fatal("playing a file is local; no daemon request should be issued")
	}
	if m.busy != "" {
		t.Fatalf("busy = %q — enter must not have started a tune", m.busy)
	}
	// No display in tests, so the URL is reported instead of spawned.
	if !strings.Contains(m.message, "http://tuner.test:8010/api/recordings/7/file") {
		t.Fatalf("message = %q, want the absolute file URL", m.message)
	}
}

// An empty recording has nothing to play; say why rather than opening a
// player on zero bytes.
func TestPlayEmptyRecordingIsRefused(t *testing.T) {
	var zero int64
	m := withRecordings(newTestModel(), Recording{
		ID: 8, State: "failed", SizeBytes: &zero, Error: "startup watchdog: no chunks within 15s",
	})

	m, _ = key(m, "enter")
	if !strings.Contains(m.failure, "empty") || !strings.Contains(m.failure, "watchdog") {
		t.Fatalf("failure = %q", m.failure)
	}
}

func TestNowNextSplitsTheSchedule(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	now := time.Now()
	next, _ := m.Update(scheduleMsg{serviceID: 1000, events: []Event{
		{EventID: 1, Start: now.Add(-2 * time.Hour), DurationS: 1800, Title: "over"},
		{EventID: 2, Start: now.Add(-10 * time.Minute), DurationS: 3600, Title: "current"},
		{EventID: 3, Start: now.Add(time.Hour), DurationS: 1800, Title: "later"},
	}})
	m = next.(Model)

	current, upcoming := m.nowNext(1000)
	if current == nil || current.Title != "current" {
		t.Fatalf("current = %+v", current)
	}
	if len(upcoming) != 1 || upcoming[0].Title != "later" {
		t.Fatalf("upcoming = %+v", upcoming)
	}
}

// A guide fetch failure must leave whatever was cached rather than blanking
// the pane — losing EPG shouldn't look like losing the channel.
func TestScheduleErrorKeepsCachedEvents(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	next, _ := m.Update(scheduleMsg{serviceID: 1000, events: []Event{{Title: "kept"}}})
	m = next.(Model)

	next, _ = m.Update(scheduleMsg{serviceID: 1000, err: http.ErrHandlerTimeout})
	m = next.(Model)

	if got := m.schedules[1000]; len(got) != 1 || got[0].Title != "kept" {
		t.Fatalf("schedules = %+v", got)
	}
}

func TestViewRendersChannelsGuideAndStatus(t *testing.T) {
	m := withChannels(newTestModel(), "asahi", "TBS1")
	next, _ := m.Update(statusMsg{status: Status{
		Adapters:  []Adapter{{Adapter: 0, Channel: "asahi", Refs: 1, Prio: "live"}},
		Recording: []int64{5},
	}})
	m = next.(Model)
	next, _ = m.Update(scheduleMsg{serviceID: 1000, events: []Event{
		{Start: time.Now().Add(-time.Minute), DurationS: 3600, Title: "報道ステーション"},
	}})
	m = next.(Model)

	out := m.View()
	for _, want := range []string{"asahi", "TBS1", "報道ステーション", "REC", "a0"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view is missing %q:\n%s", want, out)
		}
	}
}

// --player none is an explicit opt-out, so a successful switch reports the
// URL rather than an error.
func TestPlaybackDisabledReportsURL(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	m.player = &Player{Bin: "", Env: []string{"DISPLAY=:0"}}
	m.busy = "tuning asahi"

	next, _ := m.Update(switchMsg{result: SwitchResult{Channel: "asahi"}})
	m = next.(Model)

	if m.failure != "" {
		t.Fatalf("failure = %q, want none", m.failure)
	}
	if !strings.Contains(m.message, "/api/live/asahi.m3u8") {
		t.Fatalf("message = %q", m.message)
	}
}

func TestViewBeforeFirstWindowSize(t *testing.T) {
	m := NewModel(NewClient("http://tuner.test:8010"), &Player{Env: []string{}})
	if !strings.Contains(m.View(), "tuner.test") {
		t.Fatalf("view = %q", m.View())
	}
}

func TestQuitStopsThePlayer(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	_, cmd := key(m, "q")
	if cmd == nil {
		t.Fatal("q should quit")
	}
	if msg := cmd(); msg == nil {
		t.Fatal("expected a quit message")
	}
}
