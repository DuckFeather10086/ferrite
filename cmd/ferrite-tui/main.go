// Command ferrite-tui is a terminal remote control for a ferrite daemon.
//
// It holds no TV state of its own: channels, guide, adapter occupancy and
// recordings all come from the daemon's HTTP API, so this remote, the web UI
// and anything else stay consistent with each other.
//
// The intended setup is this program on the machine you are sitting at and
// the daemon on the tuner box:
//
//	ferrite-tui --host http://tuner.lan:8010
//
// Video goes to a local mpv window rather than into the terminal; the TUI
// spawns and owns that process. Running the TUI over ssh would put the mpv
// window on the *tuner box*, so with no local display it declines to spawn
// and shows the stream URL instead.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	host := flag.String("host", defaultHost(), "ferrite daemon base URL")
	player := flag.String("player", "mpv", "video player binary, or \"none\" to only report stream URLs")
	flag.Parse()

	// A pasted placeholder otherwise fails as an opaque DNS error
	// ("lookup <tunerbox>: no such host") next to an empty channel list,
	// which reads like the daemon lost its channels.
	if strings.ContainsAny(*host, "<>") {
		fmt.Fprintf(os.Stderr,
			"ferrite-tui: --host %s looks like a placeholder — put the real "+
				"daemon address there,\n              or drop the flag "+
				"entirely to use %s\n", *host, defaultHost())
		os.Exit(2)
	}

	p := &Player{Bin: *player}
	if *player == "none" {
		p.Bin = ""
	}

	client := NewClient(*host)
	prog := tea.NewProgram(NewModel(client, p), tea.WithAltScreen())

	if _, err := prog.Run(); err != nil {
		// Make sure a crash doesn't leave an orphaned player behind.
		p.Stop()
		fmt.Fprintln(os.Stderr, "ferrite-tui:", err)
		os.Exit(1)
	}
	p.Stop()
}

// defaultHost prefers FERRITE_HOST so a shell profile can point the remote
// at the tuner box without repeating the flag.
func defaultHost() string {
	if v := os.Getenv("FERRITE_HOST"); v != "" {
		return v
	}
	return "http://localhost:8010"
}
