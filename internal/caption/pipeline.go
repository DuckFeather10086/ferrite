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
// The anchor — which PTS the first listed segment starts at — is measured once
// per session with ffprobe, since only the segment itself knows.
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
	"os/exec"
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
	// FFprobeBin measures the anchor PTS. Without it the rendition cannot be
	// timed, so captions stay off.
	FFprobeBin string

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

	// Guarded by the owning Pipeline's mu.
	anchorMs  int64
	anchorSeq int64
	haveAnc   bool
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

// reanchorEvery bounds how far a segment window may be derived from declared
// durations before the timeline is measured again.
//
// ffmpeg writes #EXTINF:2.002 while the segments really run about 11 ms shorter,
// so adding durations up drifts — a quarter of a second by segment 20, and past
// a whole segment within the hour. Re-measuring every few segments keeps the
// error inside a video frame or two, and costs one ffprobe per five segments
// however long they are.
const reanchorEvery = 5

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
	if p.FFprobeBin == "" {
		return errors.New("caption: ffprobe is needed to anchor the cue timeline")
	}

	// `cues` prints a JSON object per line as each cue closes, flushed
	// immediately — a caption is only worth something while it is on screen.
	child, err := proc.SpawnOpt(ctx, proc.SpawnOpts{Stdin: true}, p.Bin, "cues")
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
	scanner.Buffer(make([]byte, 0, 8*1024), 64*1024)
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

	// Measure the timeline on the newest listed segment, so the windows either
	// side of it are derived from at most a few durations.
	newest := segments[len(segments)-1]
	if err := p.anchor(ctx, r, newest); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Start times, computed outward from the anchor rather than accumulated
	// from the head of the stream: forward by adding each segment's duration,
	// backward by subtracting the previous one's.
	starts := make([]int64, len(segments))
	anchorAt := -1
	for i, seg := range segments {
		if seg.seq == r.anchorSeq {
			anchorAt = i
			break
		}
	}
	if anchorAt < 0 {
		// The anchor rotated out between measuring and here; the next pass
		// re-measures.
		return nil
	}
	starts[anchorAt] = r.anchorMs
	for i := anchorAt + 1; i < len(segments); i++ {
		starts[i] = starts[i-1] + int64(segments[i-1].duration*1000)
	}
	for i := anchorAt - 1; i >= 0; i-- {
		starts[i] = starts[i+1] - int64(segments[i].duration*1000)
	}

	live := make(map[string]bool, len(segments))
	var playlist strings.Builder
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		targetDuration, segments[0].seq)

	for i, seg := range segments {
		start := starts[i]
		end := start + int64(seg.duration*1000)
		name := fmt.Sprintf("sub%d.vtt", seg.seq)
		if err := p.writeSegment(r, name, start, end); err != nil {
			return err
		}
		live[name] = true
		fmt.Fprintf(&playlist, "#EXTINF:%.3f,\n%s\n", seg.duration, name)
	}

	if err := writeFileAtomic(filepath.Join(r.dir, SubsPlaylist), []byte(playlist.String())); err != nil {
		return err
	}

	// Prune every .vtt the playlist no longer references, mirroring ffmpeg's
	// delete_segments. Read the directory rather than trusting what this
	// process wrote: a session reusing the channel's directory inherits the
	// previous run's segments, whose sequence numbers are unrelated to this
	// run's and would otherwise pile up.
	entries, err := os.ReadDir(r.dir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, "sub") || !strings.HasSuffix(name, ".vtt") {
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

// anchor measures the PTS the given segment starts at, when the last
// measurement is more than reanchorEvery segments away.
func (p *Pipeline) anchor(ctx context.Context, r *Rendition, seg segment) error {
	p.mu.Lock()
	fresh := r.haveAnc && seg.seq-r.anchorSeq < reanchorEvery && seg.seq >= r.anchorSeq
	p.mu.Unlock()
	if fresh {
		return nil
	}

	path := filepath.Join(r.dir, seg.name)
	pts, err := firstVideoPTS(ctx, p.FFprobeBin, path)
	if err != nil {
		return err
	}

	p.mu.Lock()
	first := !r.haveAnc
	r.anchorMs = int64(pts * 1000)
	r.anchorSeq = seg.seq
	r.haveAnc = true
	p.mu.Unlock()
	if first {
		slog.Info("caption: anchored cue timeline",
			"channel", p.Channel, "segment", seg.name, "pts_s", pts)
	} else {
		slog.Debug("caption: re-anchored cue timeline",
			"channel", p.Channel, "segment", seg.name, "pts_s", pts)
	}
	return nil
}

// firstVideoPTS reads the presentation timestamp of a segment's first video
// packet. With ffmpeg's -copyts this is a broadcast PTS, the same clock the
// captions carry.
func firstVideoPTS(ctx context.Context, ffprobeBin, path string) (float64, error) {
	cmd := exec.CommandContext(ctx, ffprobeBin,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "packet=pts_time",
		"-read_intervals", "%+#1",
		"-of", "csv=p=0",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("caption: ffprobe %s: %w", filepath.Base(path), err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" {
			continue
		}
		pts, err := strconv.ParseFloat(line, 64)
		if err != nil {
			continue
		}
		return pts, nil
	}
	return 0, fmt.Errorf("caption: no video PTS in %s", filepath.Base(path))
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
