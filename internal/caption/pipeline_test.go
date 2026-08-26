package caption

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

// A caption spanning a boundary keeps its own start in every window it appears
// in, where the WebVTT form has to cut it. The start is the key the consumer
// replaces on, so cutting it would turn one caption into one per segment.
func TestWriteSegmentJSONKeepsTheCueWhole(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))
	p.cues = []Cue{
		{StartMs: 1_000, EndMs: 31_000, Open: true, Text: "ながいひとつ",
			Caption: json.RawMessage(`{"plane_width":960,"plane_height":540,"regions":[]}`)},
		// Ended before this segment opens: not in it at all.
		{StartMs: 100, EndMs: 900, Text: "おわった"},
	}
	if err := p.writeSegmentJSON(r, "sub1.json", 1, 2_000, 4_000); err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Segment int64 `json:"segment"`
		StartMs int64 `json:"start_ms"`
		EndMs   int64 `json:"end_ms"`
		Cues    []struct {
			StartMs int64           `json:"start_ms"`
			EndMs   int64           `json:"end_ms"`
			Open    bool            `json:"open"`
			Text    string          `json:"text"`
			Caption json.RawMessage `json:"caption"`
		} `json:"cues"`
	}
	if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, "sub1.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	// The window is the whole bridge to the player's clock, so it has to be in
	// the file: the cue times are raw broadcast PTS and mean nothing without it.
	if doc.Segment != 1 || doc.StartMs != 2_000 || doc.EndMs != 4_000 {
		t.Errorf("wrong window: %+v", doc)
	}
	if len(doc.Cues) != 1 {
		t.Fatalf("want the one overlapping cue, got %d: %+v", len(doc.Cues), doc.Cues)
	}
	cue := doc.Cues[0]
	// Its own start, from before this window; its end is the window's, since it
	// is still on screen and nothing has said where it stops.
	if cue.StartMs != 1_000 || cue.EndMs != 4_000 || !cue.Open {
		t.Errorf("want the caption's own start held to the end of the window, got %+v", cue)
	}
	// Passed through byte for byte: nothing here understands the model, and
	// re-encoding it is only a way to lose a field.
	if string(cue.Caption) != `{"plane_width":960,"plane_height":540,"regions":[]}` {
		t.Errorf("the caption payload was rewritten: %s", cue.Caption)
	}
}

// A caption that outlives the decoder's guess at its length is still on screen,
// and every window it is on screen in has to say so.
//
// This is the one thing the structured form must keep doing that the WebVTT form
// does. The end of an open cue is a guess — five seconds, PROVISIONAL_MS, and
// the real one only arrives with the next caption. Publish the guess and stop
// there and the segments past it say nothing at all, which a consumer cannot
// tell from "it ended": the caption either vanishes at five seconds or is given
// some invented lifetime that outlives the broadcast's. Shipped both ways round
// before this test existed.
func TestWriteSegmentJSONKeepsAnOpenCueOnScreen(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))
	// On screen from 1.0s with the decoder's provisional five seconds.
	p.cues = []Cue{{StartMs: 1_000, EndMs: 6_000, Open: true, Text: "ながいひとつ"}}

	for _, seg := range []struct {
		name       string
		start, end int64
		wantEnd    int64 // 0 for "not in this window"
	}{
		// Inside the guess: the window is what bounds it, not the guess.
		{"sub1.json", 1_000, 2_000, 2_000},
		{"sub2.json", 4_000, 5_000, 5_000},
		// Past the guess, and still open: the caption has not ended, so the
		// window still carries it and says so as far as its own end.
		{"sub6.json", 6_000, 7_000, 7_000},
		{"sub9.json", 9_000, 10_000, 10_000},
	} {
		if err := p.writeSegmentJSON(r, seg.name, 0, seg.start, seg.end); err != nil {
			t.Fatalf("%s: %v", seg.name, err)
		}
		var doc struct {
			Cues []struct {
				StartMs int64 `json:"start_ms"`
				EndMs   int64 `json:"end_ms"`
			} `json:"cues"`
		}
		if err := json.Unmarshal([]byte(read(t, filepath.Join(dir, seg.name))), &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Cues) != 1 {
			t.Fatalf("%s: want the caption to still be on screen, got %d cues",
				seg.name, len(doc.Cues))
		}
		// The start is never cut — it is the key the consumer replaces on, and
		// four cut copies would be four different captions.
		if doc.Cues[0].StartMs != 1_000 {
			t.Errorf("%s: the start was cut to %d", seg.name, doc.Cues[0].StartMs)
		}
		if doc.Cues[0].EndMs != seg.wantEnd {
			t.Errorf("%s: want the caption held to the end of the window (%d), got %d",
				seg.name, seg.wantEnd, doc.Cues[0].EndMs)
		}
	}

	// And once the broadcast says where it ended, that wins and the windows
	// after it are empty — which is what takes the caption off screen.
	p.cues = []Cue{{StartMs: 1_000, EndMs: 6_500, Text: "ながいひとつ"}}
	if err := p.writeSegmentJSON(r, "sub9.json", 0, 9_000, 10_000); err != nil {
		t.Fatal(err)
	}
	if body := read(t, filepath.Join(dir, "sub9.json")); !strings.Contains(body, `"cues":[]`) {
		t.Errorf("a closed caption reached into a later window: %s", body)
	}
}

// A window with nothing in it still answers with a list. `null` is invisible in
// Go and crashes the consumer, which is the same rule the API's lists have.
func TestWriteSegmentJSONIsAlwaysAList(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))
	if err := p.writeSegmentJSON(r, "sub0.json", 0, 0, 2_000); err != nil {
		t.Fatal(err)
	}
	if body := read(t, filepath.Join(dir, "sub0.json")); !strings.Contains(body, `"cues":[]`) {
		t.Errorf("want an empty list, got: %s", body)
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

// ptsBytes encodes a 90 kHz timestamp the way a PES header carries it — the
// inverse of what pts.go parses, checked against the real fixture in
// TestPTSBytesRoundTrip.
func ptsBytes(ticks int64) []byte {
	return []byte{
		0x20 | byte((ticks>>29)&0x0e) | 0x01,
		byte(ticks >> 22),
		byte((ticks>>14)&0xfe) | 0x01,
		byte(ticks >> 7),
		byte((ticks<<1)&0xfe) | 0x01,
	}
}

func TestPTSBytesRoundTrip(t *testing.T) {
	got := ptsBytes(realVideoPTSMs * 90)
	if ticks, ok := parseFirstVideoPTS(tsPacket(0x0100, true, 0, pesPacket(0xe0, got))); !ok || ticks/90 != realVideoPTSMs {
		t.Fatalf("ptsBytes round trip: %d ok=%v, want %d", ticks/90, ok, realVideoPTSMs)
	}
}

// videoSegment writes a segment whose first video PES carries startMs, which is
// what windowStarts measures out of it.
func videoSegment(t *testing.T, dir string, seq int64, startMs int64) {
	t.Helper()
	data := append(
		tsPacket(0x0000, true, 0, []byte{0x00, 0x00, 0xb0, 0x0d}),
		tsPacket(0x0100, true, 0, pesPacket(0xe0, ptsBytes(startMs*90)))...,
	)
	name := filepath.Join(dir, "stream"+strconv.FormatInt(seq, 10)+".ts")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// videoWindow writes the segments of a rolling playlist and the playlist that
// lists them, as ffmpeg would.
func videoWindow(t *testing.T, dir string, first int64, count int) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:%d\n", first)
	for i := 0; i < count; i++ {
		seq := first + int64(i)
		videoSegment(t, dir, seq, 100_000+seq*2_000)
		fmt.Fprintf(&b, "#EXTINF:2.000,\nchannel/1080p/stream%d.ts\n", seq)
	}
	path := filepath.Join(dir, "stream.m3u8")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A subtitle segment has to outlive the video segment of the same number, or a
// player that has fallen behind the live edge gets a 404 on the captions for a
// picture it can still fetch.
//
// ffmpeg keeps one segment past the window (hls_delete_threshold), and this
// publishes a tick behind it — so pruning exactly the window, which is what this
// used to do, left the subtitle rendition the narrower of the two and 404'd the
// oldest entry of the playlist a player was holding. Measured on a live session
// before the fix: video segments outlived the playlist by 1.95s, subtitle
// segments by 0.00s.
func TestPublishKeepsASubtitleSegmentPastTheWindow(t *testing.T) {
	dir := t.TempDir()
	p := &Pipeline{}
	r := p.Attach(dir, filepath.Join(dir, "stream.m3u8"))

	// A leftover from an earlier run on this directory, numbered where this
	// run will never reach. Nothing keeps it.
	if err := os.WriteFile(filepath.Join(dir, "sub9999.vtt"), []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	publish := func(first int64) {
		t.Helper()
		videoWindow(t, dir, first, 6)
		if err := p.publish(r); err != nil {
			t.Fatalf("publish at %d: %v", first, err)
		}
	}

	publish(10)
	for _, name := range []string{"sub10.vtt", "sub10.json", "sub15.vtt", "sub15.json"} {
		if !exists(name) {
			t.Errorf("%s: want the window's own segments written", name)
		}
	}
	if exists("sub9999.vtt") {
		t.Error("sub9999.vtt: a previous run's leftover must not survive a publish")
	}

	// 10 has just rotated out of the window. A player's copy of subs.m3u8 still
	// lists it, so it has to still be there.
	publish(11)
	if !exists("sub10.vtt") || !exists("sub10.json") {
		t.Error("sub10: dropped on the tick it left the window — this is the 404")
	}

	// Two segments back is the grace band's edge; past it there is no player
	// left that could ask.
	publish(13)
	if exists("sub10.vtt") || exists("sub10.json") {
		t.Error("sub10: kept beyond the grace band, so the directory would grow")
	}
	if !exists("sub11.vtt") || !exists("sub12.vtt") {
		t.Error("sub11/sub12: inside the grace band and still needed")
	}

	// The invariant, stated the way it matters: every video segment ffmpeg has
	// left on disk (the window plus its own one-segment threshold) has captions
	// beside it.
	for seq := int64(12); seq <= 18; seq++ {
		name := fmt.Sprintf("sub%d.vtt", seq)
		if !exists(name) {
			t.Errorf("%s: a video segment on disk with no captions beside it", name)
		}
	}
}
