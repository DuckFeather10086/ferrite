// Package caption turns the ARIB caption PID of a live tune into an HLS
// WebVTT rendition sitting beside the video segments.
//
// The decoding is not done here — `arib-caption` (the sibling Rust crate) reads
// the same descrambled TS the video comes from and prints one JSON cue per
// line. This package does the part that has to know about HLS: which cues
// belong in which segment, and how a player is supposed to line them up with
// the picture.
//
// That alignment is the whole difficulty. A cue's time is the PTS of the TS it
// was decoded from; a player's timeline starts at zero. The bridge is
// `X-TIMESTAMP-MAP` plus ffmpeg's -copyts, which keeps the input PTS on the
// video segments so the player can subtract its own first PTS and land on the
// same instant. Two consequences worth knowing before changing anything here:
//
//   - Drop -copyts from the HLS session and every cue lands hours off, because
//     the video restarts at zero while the captions still carry broadcast PTS.
//   - The subtitle playlist must mirror the video playlist segment for segment.
//     A player fetches the subtitle fragment covering the position it is
//     playing; if the windows do not correspond, it fetches the wrong file and
//     the cues inside it are never shown.
//
// Which PTS a segment starts at is measured out of the segment, since only the
// segment knows — every one of them, once each, by reading its first video PES
// header directly (see pts.go). It used to be one ffprobe per five segments
// with the rest of the window derived from the durations the playlist declares;
// that spawn turned out to be stalling the encoder enough to make the segments
// irregular, so the caption pipeline was corrupting the very thing it measured.
//
// # Two forms of the same words
//
// Beside each `sub{N}.vtt` this writes a `sub{N}.json`: the whole decoded
// caption for that window — cells, colours, sizes, ruby, the DRCS bitmaps a
// `.vtt` can only spell 〓 — for a browser that draws the caption itself instead
// of handing the words to the player. It is not in any playlist and no player
// asks for it; the overlay knows which video fragment it is playing and fetches
// the JSON of that number, which works because writeSegment already names both
// after the video segment's own sequence number.
//
// The WebVTT rendition stays exactly as it was. It is what Safari and an iPad
// get from the manifest with nothing of ours running, and it is what the browser
// draws inside its own fullscreen video, where a canvas overlay is invisible.
// This adds a form; it does not replace one.
package caption

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DuckFeather10086/ferrite/internal/fanout"
	"github.com/DuckFeather10086/ferrite/internal/proc"
)

// Cue is one subtitle line as `arib-caption cues` prints it. Times are
// milliseconds on the broadcast PTS timeline.
type Cue struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
	// Open marks a caption that is still on screen: its start is real, its end
	// provisional. The decoder prints it twice — open on arrival, closed when
	// the following caption says where it ended — and the second one replaces
	// the first by StartMs.
	Open bool   `json:"open"`
	Top  bool   `json:"top"`
	Text string `json:"text"`
	// Caption is the whole decoded caption — cells, colours, sizes, DRCS
	// bitmaps — as `cues --regions` serialises it. Carried through untouched
	// and republished verbatim: nothing here understands it, and re-encoding
	// a document to write it back out unchanged is only a way to lose a field.
	// Absent when the decoder was not asked for regions.
	Caption json.RawMessage `json:"caption,omitempty"`
}

// How many cues to keep. At one line every two seconds this is over an hour,
// which is far more than a rotating live playlist can reference; the cap only
// exists so a session left running for days does not grow without bound.
const maxCues = 2048

// Pipeline runs one channel's caption decode for as long as the tune it is
// reading lives, and publishes a subtitle rendition for each video
// playlist attached to it.
//
// One decode, several outputs, because a channel can be encoded at more
// than one quality at a time: the captions are a property of the
// broadcast, not of the encode, so decoding them twice would mean two
// arib-caption children and two subscriptions to the same TS for
// identical words. What cannot be shared is the *rendition* — a player
// matches a subtitle fragment to the video fragment it is playing, and
// each encode has its own segment numbering and its own boundaries — so
// there is one Rendition per quality, all fed from the same cues.
type Pipeline struct {
	// Bin is the arib-caption executable. Empty disables captions.
	Bin string

	Channel string
	// Sub is a second subscription to the same tune the video comes from.
	Sub *fanout.Sub

	// Refresh is how often each video playlist is re-read. It has to stay
	// comfortably inside the video's segment duration or the rendition
	// trails the picture by a segment, so the caller sets it from whatever
	// -hls_time it gave ffmpeg (internal/hls passes half). Defaults to 1s,
	// which is the right answer only for segments of 2s or longer.
	Refresh time.Duration

	mu         sync.Mutex
	cues       []Cue
	renditions []*Rendition
}

// Rendition is one subtitle output: a subs.m3u8 in dir, mirroring the
// video playlist at playlist segment for segment.
//
// The anchor lives here rather than on the Pipeline because it is a
// property of one encode's segments — two qualities of the same channel
// start at different moments and number their segments from their own
// zero.
type Rendition struct {
	dir      string
	playlist string

	// The measured start of each listed segment, by sequence number, so a
	// segment is read once however many ticks it stays in the playlist.
	// Touched only by the publish loop, which is single-goroutine.
	starts map[int64]int64
}

// Dir is where this rendition writes, for callers that need to check
// whether it has published yet.
func (r *Rendition) Dir() string { return r.dir }

// Attach adds an output mirroring videoPlaylist into dir. Safe to call
// before or after Run; the next publish tick picks it up.
func (p *Pipeline) Attach(dir, videoPlaylist string) *Rendition {
	r := &Rendition{dir: dir, playlist: videoPlaylist}
	p.mu.Lock()
	p.renditions = append(p.renditions, r)
	p.mu.Unlock()
	return r
}

// Detach stops publishing r. The decode carries on for whatever else is
// attached; the caller is responsible for the files r left behind.
func (p *Pipeline) Detach(r *Rendition) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, have := range p.renditions {
		if have == r {
			p.renditions = append(p.renditions[:i], p.renditions[i+1:]...)
			return
		}
	}
}

// SubsPlaylist is the name of the subtitle media playlist. It lives in Dir and
// is served like a segment; the multivariant playlist that points at it is
// composed by the API, which knows the URL it is being served from.
const SubsPlaylist = "subs.m3u8"

// Run decodes captions until ctx is done or the source ends.
//
// It is not fatal for a live session if this returns an error: the picture is
// unaffected, and the subtitle rendition simply stops being updated.
func (p *Pipeline) Run(ctx context.Context) error {
	if p.Bin == "" {
		return errors.New("caption: no arib-caption binary configured")
	}
	if p.Sub == nil {
		return errors.New("caption: no TS subscription")
	}

	// `cues` prints a JSON object per line as each cue closes, flushed
	// immediately — a caption is only worth something while it is on screen.
	//
	// --regions attaches the whole caption model to each line, which is the
	// JSON rendition's entire content. One decoder either way: the words and
	// the placement come out of the same pass, so the two renditions cannot
	// describe different captions.
	child, err := proc.SpawnOpt(ctx, proc.SpawnOpts{Stdin: true}, p.Bin, "cues", "--regions")
	if err != nil {
		return fmt.Errorf("caption: spawn %s: %w", p.Bin, err)
	}
	defer child.Close()

	go pumpTS(p.Sub, child.Stdin)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.readCues(child.Stdout)
	}()

	refresh := p.Refresh
	if refresh <= 0 {
		refresh = time.Second
	}
	ticker := time.NewTicker(refresh)
	defer ticker.Stop()
	lastErr := ""

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			// The decoder exited: the tune ended, or it died. Either way there
			// are no more cues.
			return nil
		case <-ticker.C:
			err := p.publishAll(ctx)
			// Publishing runs every second, so a persistent failure would
			// flood the log — but a silent one means captions quietly stop
			// while the picture keeps playing. Log each distinct failure once.
			switch {
			case err != nil && err.Error() != lastErr:
				lastErr = err.Error()
				slog.Warn("caption: publishing the rendition failed",
					"channel", p.Channel, "err", err)
			case err == nil && lastErr != "":
				lastErr = ""
				slog.Info("caption: rendition publishing recovered", "channel", p.Channel)
			}
		}
	}
}

// readCues collects cues until stdout closes.
func (p *Pipeline) readCues(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// A line is a few hundred bytes of words plus, with --regions, the whole
	// caption: ~4 KB for an ordinary two-line caption and more for one built
	// out of DRCS bitmaps. Over the limit a scanner stops at the line rather
	// than skipping it, so the ceiling is generous on purpose.
	scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] != '{' {
			continue
		}
		var cue Cue
		if err := json.Unmarshal([]byte(line), &cue); err != nil {
			slog.Warn("caption: bad cue line", "channel", p.Channel, "err", err)
			continue
		}
		p.mu.Lock()
		p.upsert(cue)
		if len(p.cues) > maxCues {
			p.cues = append([]Cue(nil), p.cues[len(p.cues)-maxCues:]...)
		}
		p.mu.Unlock()
	}
}

// upsert adds a cue, or replaces the provisional version of one already held.
//
// Publishing a caption while it is still on screen is what keeps the subtitle
// track level with the picture: its real end only arrives with the next
// caption, 2 to 8 seconds later, by which time the segment it belonged in has
// been fetched and a correct-but-late cue is worth nothing. Cues arrive in
// start order, so the match is always among the last few.
//
// Caller holds p.mu.
func (p *Pipeline) upsert(cue Cue) {
	for i := len(p.cues) - 1; i >= 0 && i > len(p.cues)-8; i-- {
		if p.cues[i].StartMs != cue.StartMs {
			continue
		}
		// Never let a provisional end overwrite a known one: the decoder emits
		// open-then-closed, but a restarted decoder could repeat itself.
		if !cue.Open || p.cues[i].Open {
			p.cues[i] = cue
		}
		return
	}
	p.cues = append(p.cues, cue)
}

// pumpTS feeds the tune's chunks to the decoder's stdin. Same policy as the
// ffmpeg pump: a write error means the child is gone, which is not this
// goroutine's problem to fix.
func pumpTS(sub *fanout.Sub, w io.WriteCloser) {
	defer w.Close()
	for chunk := range sub.Ch {
		_, err := w.Write(chunk.Data)
		chunk.Release()
		if err != nil {
			return
		}
	}
}

// segment is one entry of a media playlist.
type segment struct {
	seq      int64
	name     string
	duration float64
}

// publishAll refreshes every attached rendition. One rendition failing —
// a quality whose ffmpeg has just died, say — must not stop the others,
// so the first error is reported and the rest still run.
func (p *Pipeline) publishAll(ctx context.Context) error {
	p.mu.Lock()
	renditions := append([]*Rendition(nil), p.renditions...)
	p.mu.Unlock()

	var firstErr error
	for _, r := range renditions {
		if err := p.publish(ctx, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// publish re-reads one video playlist and writes its subtitle rendition to
// match it.
func (p *Pipeline) publish(ctx context.Context, r *Rendition) error {
	segments, targetDuration, err := readPlaylist(r.playlist)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return nil
	}

	// Measured before the lock is taken: this reads files, and the same mutex
	// is held by the goroutine draining cues off the decoder.
	starts, ok := r.windowStarts(segments)
	if !ok {
		// Nothing in the playlist could be measured — a session whose segments
		// are being written as this runs. The next tick tries again.
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	live := make(map[string]bool, len(segments))
	var playlist strings.Builder
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		targetDuration, segments[0].seq)

	for i, seg := range segments {
		start := starts[i]
		// The window ends where the next one begins — measured, not declared.
		// Only the newest segment has nothing after it to end against, and
		// there its own #EXTINF is all there is.
		end := start + int64(seg.duration*1000)
		if i+1 < len(segments) {
			end = starts[i+1]
		}
		name := fmt.Sprintf("sub%d.vtt", seg.seq)
		if err := p.writeSegment(r, name, start, end); err != nil {
			return err
		}
		live[name] = true
		// The second form of the same window, for the overlay that draws
		// captions itself. It is not in the playlist and no player asks for
		// it: the browser knows which video fragment it is playing and fetches
		// the JSON of that number. See writeSegmentJSON.
		jsonName := fmt.Sprintf("sub%d.json", seg.seq)
		if err := p.writeSegmentJSON(r, jsonName, seg.seq, start, end); err != nil {
			return err
		}
		live[jsonName] = true
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\n%s\n", seg.duration, name)
	}

	if err := writeFileAtomic(filepath.Join(r.dir, SubsPlaylist), []byte(playlist.String())); err != nil {
		return err
	}

	// Prune every segment of either form the playlist no longer references,
	// mirroring ffmpeg's delete_segments. Read the directory rather than
	// trusting what this process wrote: a session reusing the channel's
	// directory inherits the previous run's segments, whose sequence numbers
	// are unrelated to this run's and would otherwise pile up.
	entries, err := os.ReadDir(r.dir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "sub") {
				continue
			}
			if !strings.HasSuffix(name, ".vtt") && !strings.HasSuffix(name, ".json") {
				continue
			}
			if !live[name] {
				_ = os.Remove(filepath.Join(r.dir, name))
			}
		}
	}
	return nil
}

// bottomLine is where a caption sits when the broadcast did not move it up.
//
// Not the browser's own default, which is nowhere near the bottom of the
// picture: a browser reserves room for its control bar whether or not the
// controls are showing — measured at 13.6% of the picture height in Chromium
// 1217 — and a subtitle floating in the lower third of the frame is what that
// looks like. Snap-to-lines cannot go below that reservation (`line:-1` lands in
// exactly the same place as `line:auto`), so the percentage form is the way down:
// 94% leaves the caption clear of the progress bar, which sits at about 96%.
// Measured there at 5.6% of the picture height, and a caption of several lines
// grows *upward* from it rather than off the bottom.
//
// Bare, with no line alignment after it. WebVTT allows `line:94%,end` and hls.js
// parses it, but Chromium's own parser throws the whole setting away when it sees
// the comma — and a percentage line is placed by the box's bottom edge there
// anyway. The live rendition goes through hls.js and a recording's sidecar
// through the browser's parser, so the form has to satisfy both.
//
// libaribcaption-rs's WebVTT renderer writes the same setting for a recording's
// sidecar; the two have to agree or a channel's captions move when you record it.
const bottomLine = " line:94%"

// topLine is two lines down from the top, which clears the channel logos in the
// corner. A broadcast moves a caption up when something at the bottom of the
// frame matters, and following it there is the point of carrying the flag.
const topLine = " line:1"

// writeSegment writes the cues overlapping [startMs, endMs) as one WebVTT
// segment, each clamped to the segment's own window.
//
// Clamping is what keeps a caption from being drawn twice. A caption spanning a
// boundary belongs in both segments, but its *times* cannot be the same in both:
// while it is still on screen its end is provisional (the end of the segment
// being written), and by the next segment the real end has arrived. A player
// dedups cues by their start, end and text — hls.js hashes exactly those three —
// so two copies of one caption with different ends are two cues to it, and it
// draws the line twice, the stale copy hanging over the caption after it. Nor is
// that a race the publisher can win by rewriting the segment: a player fetches a
// segment the moment it appears and never looks at it again. Cut into
// per-segment pieces, the copies are contiguous instead of overlapping — the
// caption stays on screen across the boundary and is only ever drawn once.
func (p *Pipeline) writeSegment(r *Rendition, name string, startMs, endMs int64) error {
	var body strings.Builder
	// Cue times are broadcast PTS, and the map says which instant that is: the
	// segment's own start, given once as a 90 kHz PTS and once as the time the
	// cues are written in. A player takes the difference (zero, here) and its
	// video's first PTS to place the cues on its own timeline — which only lines
	// up because the video kept its input timestamps (ffmpeg -copyts).
	//
	// Written as the real PTS rather than the equivalent MPEGTS:0,LOCAL:0
	// because that is the form every player is used to seeing, and it says out
	// loud which segment this file belongs to.
	body.WriteString("WEBVTT\n")
	fmt.Fprintf(&body, "X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:%s\n\n",
		(startMs*90)&0x1_FFFF_FFFF, vttTimestamp(startMs))
	if endMs > startMs {
		for _, cue := range p.cues {
			// A caption still on screen runs to the end of whatever segment is
			// being written: it is on screen *now*, and its real end has not
			// been broadcast yet. Segment by segment this covers the caption
			// continuously however long it lasts, and stops within one segment
			// of the real end once that arrives — where trusting the
			// provisional end would drop a long caption out of the segments
			// past it, leaving a hole a player never refetches.
			end := cue.EndMs
			if cue.Open && endMs > end {
				end = endMs
			}
			if end <= startMs || cue.StartMs >= endMs {
				continue
			}
			// Its piece of this segment, so the pieces either side of a
			// boundary meet rather than overlap.
			if cue.StartMs < startMs {
				cue.StartMs = startMs
			}
			if end > endMs {
				end = endMs
			}
			cue.EndMs = end
			body.WriteString(formatCue(cue))
			body.WriteString("\n")
		}
	}
	return writeFileAtomic(filepath.Join(r.dir, name), []byte(body.String()))
}

// jsonSegment is one window's worth of structured cues: the same selection the
// WebVTT segment beside it makes, and the window itself so a consumer can put
// the cues on its own clock.
//
// Cues is never nil. A `null` where a list belongs is invisible in Go and
// crashes the other end, which is the same rule the API's list endpoints have.
type jsonSegment struct {
	Segment int64 `json:"segment"`
	// The broadcast PTS window this segment covers, in milliseconds — measured
	// on the video segment of the same number (see anchor). It is the whole
	// bridge between the cue times, which are raw broadcast PTS, and a player's
	// own timeline: the fragment starting at frag.start on that timeline starts
	// at StartMs on this one, and one subtraction converts every cue.
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
	Cues    []Cue `json:"cues"`
}

// writeSegmentJSON writes the cues on screen during [startMs, endMs) as the
// structured form of the same segment.
//
// Of the two things writeSegment does to a cue, one applies here and one does
// not, and the difference is what each is for.
//
//   - **The open-cue extension applies.** It is not a workaround for how a
//     player holds cues — it is the only statement this ever makes that a
//     caption is *still on screen*. An ARIB caption's end arrives with the next
//     caption, so until then all the decoder can offer is a guess
//     (`PROVISIONAL_MS`, five seconds); publishing that guess and stopping there
//     leaves every caption that outlives it missing from the segments in
//     between, with nothing to say whether it ended or is simply still up. So an
//     open cue runs to the end of whatever window is being written — on screen
//     *now*, for as long as it stays — and the window it is written into is what
//     bounds it. Getting this wrong is not subtle: the consumer has to invent a
//     lifetime for the hole, and any number it invents outlives the broadcast's.
//   - **The clamping does not.** That one *is* about how a player holds cues:
//     one dedups by start, end and text, so an uncut caption spanning a boundary
//     reads as two cues and gets drawn twice. A consumer of this keys on
//     `start_ms` alone, so the same caption arriving in four segments is one
//     caption seen four times, and cutting its start would make it four
//     different ones.
//
// Which leaves the two renditions asserting exactly the same thing at every
// instant — see the invariant in CLAUDE.md. They have to: a viewer switching
// between them must not see the caption move in time.
func (p *Pipeline) writeSegmentJSON(r *Rendition, name string, seq, startMs, endMs int64) error {
	doc := jsonSegment{Segment: seq, StartMs: startMs, EndMs: endMs, Cues: []Cue{}}
	if endMs > startMs {
		for _, cue := range p.cues {
			// The window bounds an open cue in *both* directions, which is why
			// this is an assignment and not the max writeSegment appears to
			// take: that one extends and then clamps to the same window, so what
			// it really states is the window's own end. Two renditions saying
			// different things about when a caption left the screen is a caption
			// that jumps when the viewer switches between them.
			if cue.Open {
				cue.EndMs = endMs
			}
			if cue.EndMs <= startMs || cue.StartMs >= endMs {
				continue
			}
			doc.Cues = append(doc.Cues, cue)
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(r.dir, name), body)
}

func formatCue(cue Cue) string {
	line := bottomLine
	if cue.Top {
		line = topLine
	}
	return fmt.Sprintf("%s --> %s%s\n%s\n",
		vttTimestamp(cue.StartMs), vttTimestamp(cue.EndMs), line, escapeVTT(cue.Text))
}

func vttTimestamp(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d",
		ms/3_600_000, (ms/60_000)%60, (ms/1000)%60, ms%1000)
}

func escapeVTT(text string) string {
	text = strings.ReplaceAll(text, "&", "&amp;")
	text = strings.ReplaceAll(text, "<", "&lt;")
	return strings.ReplaceAll(text, ">", "&gt;")
}

// windowStarts is the broadcast PTS each listed segment begins at.
//
// Every one of them is *measured*, out of the segment itself, and each segment
// is measured exactly once — a segment does not change after ffmpeg closes it,
// so the answer is cached by sequence number for as long as the playlist
// references it. That is what retires the old arrangement, where one segment
// was probed and the rest of the window was derived by adding up the durations
// the playlist declares. Two things go with it: the drift those declared
// durations accumulate, and the subprocess the probe used to be — which was
// itself making the segments irregular (see pts.go).
//
// Reports false when nothing could be measured at all. A segment that cannot be
// read on its own is filled in from a neighbour, since a playlist entry whose
// file is a moment from existing is ordinary rather than exceptional.
func (r *Rendition) windowStarts(segments []segment) ([]int64, bool) {
	starts := make([]int64, len(segments))
	known := make([]bool, len(segments))
	fresh := make(map[int64]int64, len(segments))
	any := false

	for i, seg := range segments {
		if ms, ok := r.starts[seg.seq]; ok {
			starts[i], known[i], any = ms, true, true
			fresh[seg.seq] = ms
			continue
		}
		ms, err := firstVideoPTS(filepath.Join(r.dir, seg.name))
		if err != nil {
			// Not worth a log line: the newest segment is routinely still
			// being written when this runs, and the next tick will have it.
			continue
		}
		starts[i], known[i], any = ms, true, true
		fresh[seg.seq] = ms
	}
	// Only what the playlist still lists, so the map cannot outgrow the window.
	r.starts = fresh
	if !any {
		return nil, false
	}

	// Fill the gaps from whichever side has an answer, using the declared
	// durations for the one hop. Forward first, then backward for anything at
	// the head that had nothing before it.
	for i := 1; i < len(segments); i++ {
		if !known[i] && known[i-1] {
			starts[i] = starts[i-1] + int64(segments[i-1].duration*1000)
			known[i] = true
		}
	}
	for i := len(segments) - 2; i >= 0; i-- {
		if !known[i] && known[i+1] {
			starts[i] = starts[i+1] - int64(segments[i].duration*1000)
			known[i] = true
		}
	}
	return starts, true
}

// readPlaylist parses the segment list of an ffmpeg-written media playlist.
func readPlaylist(path string) ([]segment, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	var (
		segments  []segment
		seq       int64
		target    = 2
		duration  float64
		haveDur   bool
		nextSeq   int64
		haveMedia bool
	)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
			if n, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64); err == nil {
				seq = n
				nextSeq = n
				haveMedia = true
			}
		case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:")); err == nil && n > 0 {
				target = n
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			field := strings.TrimSuffix(strings.TrimPrefix(line, "#EXTINF:"), ",")
			field = strings.SplitN(field, ",", 2)[0]
			if d, err := strconv.ParseFloat(field, 64); err == nil {
				duration = d
				haveDur = true
			}
		case strings.HasPrefix(line, "#"):
			continue
		default:
			if !haveDur {
				continue
			}
			// The URI carries ffmpeg's -hls_base_url prefix; the file itself is
			// in Dir.
			segments = append(segments, segment{
				seq:      nextSeq,
				name:     filepath.Base(line),
				duration: duration,
			})
			nextSeq++
			haveDur = false
		}
	}
	if !haveMedia {
		// A playlist without the tag starts at zero by definition.
		for i := range segments {
			segments[i].seq = int64(i)
		}
	}
	_ = seq
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].seq < segments[j].seq })
	return segments, target, nil
}

// writeFileAtomic writes via a temp file and renames, so a player never reads
// a half-written playlist.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
