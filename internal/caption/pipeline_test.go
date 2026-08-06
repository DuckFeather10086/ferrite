package caption

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A caption that outlives a segment is written into each one it covers, cut to
// that segment's window — because a player treats two copies of it with
// different ends as two cues and draws the line twice.
func TestWriteSegmentCutsACaptionAtTheSegmentBoundary(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))
	// On screen from 1.0s, its end not broadcast yet.
	p.cues = []Cue{{StartMs: 1_000, EndMs: 31_000, Open: true, Text: "ながいひとつ"}}

	for _, seg := range []struct {
		name       string
		start, end int64
		wantStart  string
		wantEnd    string
	}{
		// The caption starts inside the first segment and runs past it.
		{"sub0.vtt", 0, 2_000, "00:00:01.000", "00:00:02.000"},
		// The next one carries the same words from its own start: contiguous
		// with the piece before it, and overlapping nothing.
		{"sub1.vtt", 2_000, 4_000, "00:00:02.000", "00:00:04.000"},
	} {
		if err := p.writeSegment(r, seg.name, seg.start, seg.end); err != nil {
			t.Fatalf("writeSegment %s: %v", seg.name, err)
		}
		body := read(t, filepath.Join(dir, seg.name))
		want := seg.wantStart + " --> " + seg.wantEnd
		if !strings.Contains(body, want) {
			t.Errorf("%s: want a cue %q, got:\n%s", seg.name, want, body)
		}
		if n := strings.Count(body, " --> "); n != 1 {
			t.Errorf("%s: want one cue, got %d:\n%s", seg.name, n, body)
		}
	}
}

// The end a caption really had wins over the provisional one as soon as it is
// known, and a caption that ended before the segment does not reach into it.
func TestWriteSegmentHonoursAClosedEnd(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))
	p.cues = []Cue{
		{StartMs: 1_000, EndMs: 2_500, Text: "みじかい"},
		{StartMs: 2_500, EndMs: 6_000, Text: "つぎ"},
	}
	if err := p.writeSegment(r, "sub1.vtt", 2_000, 4_000); err != nil {
		t.Fatal(err)
	}
	body := read(t, filepath.Join(dir, "sub1.vtt"))
	if !strings.Contains(body, "00:00:02.000 --> 00:00:02.500") {
		t.Errorf("the first caption should end where it ended:\n%s", body)
	}
	if !strings.Contains(body, "00:00:02.500 --> 00:00:04.000") {
		t.Errorf("the second should fill the rest of the segment:\n%s", body)
	}

	// Nothing of the first caption belongs in the segment after it.
	if err := p.writeSegment(r, "sub2.vtt", 4_000, 6_000); err != nil {
		t.Fatal(err)
	}
	if body := read(t, filepath.Join(dir, "sub2.vtt")); strings.Contains(body, "みじかい") {
		t.Errorf("a finished caption reached into a later segment:\n%s", body)
	}
}

// Where the cue sits: placed by us, because a browser's own default leaves room
// for its control bar and puts the caption in the lower third of the picture.
func TestCuePlacement(t *testing.T) {
	bottom := formatCue(Cue{StartMs: 0, EndMs: 1_000, Text: "した"})
	if !strings.Contains(bottom, "line:94%\n") {
		// Nothing after the percentage: a line alignment there makes Chromium
		// discard the setting and fall back to its own placement.
		t.Errorf("an ordinary caption should be placed near the bottom: %q", bottom)
	}
	top := formatCue(Cue{StartMs: 0, EndMs: 1_000, Top: true, Text: "うえ"})
	if !strings.Contains(top, "line:1\n") {
		t.Errorf("a caption the broadcast moved up should stay up: %q", top)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
