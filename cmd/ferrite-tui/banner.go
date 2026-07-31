package main

// The header is the one part of the screen that is not data. It says what
// this program is — the remote and the daemon are two binaries and a
// terminal gives no other clue which one you are looking at — and beside it,
// where the stream can be opened from another device.

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// wordmark is "ferrite" in half-block runes.
//
// Every line is block-drawing runes and ASCII spaces and nothing else, so
// all three scale together if a terminal draws the blocks double-width (they
// are East Asian Ambiguous, and a CJK-width terminal does). Mixing in a
// glyph from another width class — a bullet, a box corner — is what shears
// this kind of art. A full-height figlet font would look bolder and cost
// twice the rows, which on a 24-row terminal comes straight out of the
// channel list.
var wordmark = []string{
	"█▀▀ █▀▀ █▀█ █▀█ █ ▀█▀ █▀▀",
	"█▀  █▀▀ █▀▄ █▀▄ █  █  █▀▀",
	"▀   ▀▀▀ ▀ ▀ ▀ ▀ ▀  ▀  ▀▀▀",
}

// tagline answers "what is this thing" for someone who found the terminal
// already open.
const tagline = "ISDB-T live TV · guide · recordings"

// Below either bound the banner collapses to a single line: rows spent on
// decoration are rows taken from the channel list, and a narrow terminal
// would wrap the wordmark into rubble.
const (
	bannerMinWidth  = 62
	bannerMinHeight = 20
)

// renderHeader draws the wordmark with the tagline and which daemon this
// remote is driving beside it.
func (m Model) renderHeader() string {
	if m.width < bannerMinWidth || m.height < bannerMinHeight {
		return styleTitle.Render("ferrite") + styleDim.Render(" · "+tagline)
	}
	left := styleTitle.Render(strings.Join(wordmark, "\n"))
	right := strings.Join([]string{
		tagline,
		styleDim.Render(m.daemonLine()),
		"",
	}, "\n")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right)
}

// headerHeight is how many rows renderHeader draws.
func (m Model) headerHeight() int {
	if m.width < bannerMinWidth || m.height < bannerMinHeight {
		return 1
	}
	return len(wordmark)
}

// daemonLine names the daemon this remote is driving and how long it has
// been up. With the TUI on a laptop and the daemon in a corner of the room,
// "is the thing even running" is the first question an empty screen raises.
func (m Model) daemonLine() string {
	parts := []string{shortHost(m.client.BaseURL)}
	if m.status.Version != "" {
		parts = append(parts, m.status.Version)
	}
	if m.status.Uptime != "" {
		parts = append(parts, "up "+m.status.Uptime)
	}
	return strings.Join(parts, " · ")
}

func shortHost(base string) string {
	base = strings.TrimPrefix(base, "http://")
	base = strings.TrimPrefix(base, "https://")
	return strings.TrimSuffix(base, "/")
}

// renderWatchURLs lists every address the live stream can be opened at.
//
// This block is why a headless setup is usable at all: the TUI runs over ssh
// on a box with no display, so the useful answer to "watch this" is a URL to
// paste into VLC on the iPad or a browser on the laptop. The daemon supplies
// the addresses — it is the only side that can see its own interfaces — and
// every one of them points at the same single playlist.
func (m Model) renderWatchURLs() string {
	live := m.status.LiveChannel() != ""
	addrs := m.watchAddresses()
	lines := make([]string, 0, len(addrs))
	for i, a := range addrs {
		lead := "      "
		if i == 0 {
			lead = styleTitle.Render("watch ")
		}
		line := cell(a.Kind, 10) + a.StreamURL(m.status.Stream)
		if !live {
			// Nothing is tuned, so these 404 until something is. Still
			// worth showing — this is where the address is read off the
			// screen — but not dressed up as already playing.
			line = styleDim.Render(line)
		}
		lines = append(lines, lead+line)
	}
	return strings.Join(lines, "\n")
}

// watchAddresses is what the daemon reported, falling back to the address
// this remote was pointed at — which is at least one that is known to work.
func (m Model) watchAddresses() []Address {
	if len(m.status.Addresses) > 0 {
		return m.status.Addresses
	}
	return []Address{{Kind: "daemon", Base: m.client.BaseURL}}
}
