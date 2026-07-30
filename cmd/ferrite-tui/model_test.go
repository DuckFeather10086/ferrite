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
