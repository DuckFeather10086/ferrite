package main

import (
	"strings"
	"testing"
	"time"
)

func withWatchAddresses(m Model) Model {
	next, _ := m.Update(statusMsg{status: Status{
		Version: "v0.1.0",
		Uptime:  "3h12m",
		Stream:  "/stream.m3u8",
		Addresses: []Address{
			{Kind: "local", Host: "localhost", Base: "http://localhost:8010"},
			{Kind: "lan", Host: "192.168.1.42", Base: "http://192.168.1.42:8010", Iface: "br0"},
			{Kind: "tailscale", Host: "100.101.102.103", Base: "http://100.101.102.103:8010", Iface: "tailscale0"},
		},
		Adapters: []Adapter{{Adapter: 0, Channel: "asahi", Refs: 1, Prio: "live"}},
	}})
	return next.(Model)
}

// The header says what the program is, and the block under it says where to
// watch — the whole reason this remote is usable from a box with no display.
func TestHeaderAndWatchURLsAreOnScreen(t *testing.T) {
	m := withWatchAddresses(withChannels(newTestModel(), "asahi", "TBS1"))

	out := m.View()
	for _, want := range []string{
		wordmark[0], tagline, "tuner.test:8010", "v0.1.0", "up 3h12m",
		"http://localhost:8010/stream.m3u8",
		"http://192.168.1.42:8010/stream.m3u8",
		"http://100.101.102.103:8010/stream.m3u8",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("view is missing %q:\n%s", want, out)
		}
	}
	// One playlist for live TV: no channel name belongs in a watch URL.
	if strings.Contains(out, "/api/live/") {
		t.Fatalf("a per-channel playlist URL leaked into the header:\n%s", out)
	}
}

// An older daemon reports no addresses. The URL this remote was pointed at is
// still one that works, so the block must not go blank.
func TestWatchURLsFallBackToTheHostFlag(t *testing.T) {
	m := withChannels(newTestModel(), "asahi")
	if out := m.View(); !strings.Contains(out, "http://tuner.test:8010/stream.m3u8") {
		t.Fatalf("view is missing a fallback watch URL:\n%s", out)
	}
}

// The recordings view plays files, not the tuner: those rows are better spent
// on recordings than on a live URL.
func TestWatchURLsAreLiveViewOnly(t *testing.T) {
	m := withWatchAddresses(newTestModel())
	m = withRecordings(m, Recording{ID: 1, State: "done", Channel: "asahi"})
	if out := m.View(); strings.Contains(out, "/stream.m3u8") {
		t.Fatalf("the recordings view should not carry watch URLs:\n%s", out)
	}
}

// Everything the frame draws is accounted for, so the screen is exactly as
// tall as the terminal: one line too many scrolls the key hints out of view,
// one too few leaves a gap above them.
func TestViewFitsTheTerminalExactly(t *testing.T) {
	names := make([]string, 40) // more channels than any terminal has rows
	for i := range names {
		names[i] = strings.Repeat("ch", i%5+1)
	}

	for _, size := range []struct{ w, h int }{
		{100, 30}, {100, 24}, {80, 20}, {70, 40}, {50, 24}, {40, 18},
	} {
		m := withWatchAddresses(withChannels(newTestModel(), names...))
		m.width, m.height = size.w, size.h
		// A dense guide: eight upcoming events would overflow a short pane.
		events := []Event{{Start: time.Now().Add(-time.Minute), DurationS: 3600,
			Title: "NOW", Synopsis: strings.Repeat("あらすじ", 30)}}
		for i := 1; i <= 8; i++ {
			events = append(events, Event{
				Start:     time.Now().Add(time.Duration(i) * time.Hour),
				DurationS: 3600, Title: "next",
			})
		}
		next, _ := m.Update(scheduleMsg{serviceID: 1000, events: events})
		m = next.(Model)

		if got := countLines(m.View()); got != size.h {
			t.Fatalf("%dx%d: rendered %d lines, want %d:\n%s",
				size.w, size.h, got, size.h, m.View())
		}

		// The recordings view shares the accounting.
		m = withRecordings(m, Recording{ID: 1, State: "recording", Channel: "asahi"})
		if got := countLines(m.View()); got != size.h {
			t.Fatalf("%dx%d recordings: rendered %d lines, want %d:\n%s",
				size.w, size.h, got, size.h, m.View())
		}
	}
}

// A narrow or short terminal gets one line of title instead of the wordmark:
// rows spent on decoration are rows taken from the channel list.
func TestSmallTerminalCollapsesTheBanner(t *testing.T) {
	m := withWatchAddresses(withChannels(newTestModel(), "asahi"))
	m.width, m.height = 50, 24
	out := m.View()
	if strings.Contains(out, wordmark[0]) {
		t.Fatalf("the wordmark should be gone at 50 columns:\n%s", out)
	}
	if !strings.Contains(out, "ferrite") || !strings.Contains(out, tagline) {
		t.Fatalf("the compact header should still identify the program:\n%s", out)
	}

	// Short but wide: the wordmark costs rows, which is what runs out here.
	m.width, m.height = 100, 16
	if out := m.View(); strings.Contains(out, wordmark[0]) {
		t.Fatalf("the wordmark should be gone at 16 rows:\n%s", out)
	}
}

// A terminal too small for the frame still has to render something rather
// than a negative-height panic.
func TestTinyTerminalStillRenders(t *testing.T) {
	m := withWatchAddresses(withChannels(newTestModel(), "asahi", "TBS1"))
	for _, size := range []struct{ w, h int }{{20, 8}, {10, 4}, {1, 1}} {
		m.width, m.height = size.w, size.h
		if out := m.View(); out == "" {
			t.Fatalf("%dx%d rendered nothing", size.w, size.h)
		}
	}
}
