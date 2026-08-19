package hls

import "strconv"

// Quality is one encoding tier a viewer can ask for.
//
// Tiers are **on demand and single**: a viewer picks one, the daemon runs
// exactly one encode for it, and everyone watching that channel at that
// tier shares it. What this deliberately is not is ABR — no master
// playlist offering several bitrates for the player to switch between.
// Standing up every tier for every viewer would mean two or three
// simultaneous H.264 encodes of the same picture on a box whose encoder
// is an N100's iGPU, to serve a switch nobody on a LAN needs. The cost of
// the choice is a reload when it changes, which is a button, once.
//
// All tiers of a channel are fed from one tune and one lease: adding a
// tier costs an ffmpeg, not an adapter.
type Quality struct {
	// Name is the URL and directory component, and the key in the config
	// table: "1080p", "720p". Kept short and path-safe.
	Name string
	// Label is what a person sees. Defaults to Name.
	Label string
	// Bandwidth is the BANDWIDTH the master playlist advertises, in bits
	// per second. It is a declaration to the player, not a limit on the
	// encoder — but a wrong one makes a player misjudge whether it can
	// keep up. Zero falls back to defaultBandwidth.
	Bandwidth int
	// OutputArgs are the ffmpeg arguments after -i: the filter chain and
	// the encoder, exactly as internal/postprocess takes them and for the
	// same reason — naming only a codec produces an ffmpeg that fails at
	// runtime. Empty means DefaultOutputArgs.
	OutputArgs []string
}

// defaultBandwidth is what the manifest advertises for a tier that does
// not say: the default encode's ceiling plus its audio.
const defaultBandwidth = 6_500_000

// DefaultQualityName is the tier a request that does not name one gets,
// and the only tier on a daemon whose config declares none.
const DefaultQualityName = "source"

// DefaultOutputArgs is the live encode this daemon has always done:
// deinterlace, normalize to square pixels, H.264 at 6 Mbit/s.
//
// ISDB-T video is MPEG-2 1080i — browsers have no MPEG-2 decoder, so
// `-c:v copy` produces a stream hls.js loads but can never render
// (audio-less black frame, videoWidth 0). superfast+zerolatency runs
// ~2.5× realtime on an Intel N100.
//
// ISDB-T HD is coded 1440x1080 with *non-square* pixels (SAR 4:3 → DAR
// 16:9). Passing that through relies on the player honouring the SAR in
// the H.264 VUI, and hls.js/MSE does not do so reliably — the picture
// comes out horizontally squished. Normalize to square pixels instead,
// which yields the standard 1920x1080 for HD and leaves an already-square
// 1920x1080 or a 4:3 SD subchannel geometrically correct: scale width by
// SAR (rounded to even), keep height, SAR 1:1. Deinterlace first —
// scaling interlaced fields would smear them.
//
// -g is not here. It is computed from the segment length (see
// segmentSeconds) and appended after these, because every segment has to
// start on an IDR frame; a tier that sets its own -g breaks that.
var DefaultOutputArgs = []string{
	"-vf", "yadif=0,scale=trunc(iw*sar/2)*2:ih,setsar=1",
	"-c:v", "libx264", "-preset", "superfast", "-tune", "zerolatency",
	"-b:v", "6M", "-maxrate", "7M", "-bufsize", "12M",
	"-pix_fmt", "yuv420p",
	"-c:a", "aac", "-b:a", "192k",
}

// DefaultQuality is the single tier a daemon runs when its config names
// none — which is every daemon that has not opted into tiers, and is
// exactly the encode that was hardcoded before they existed.
func DefaultQuality() Quality {
	return Quality{
		Name:      DefaultQualityName,
		Label:     "Source",
		Bandwidth: defaultBandwidth,
	}
}

func (q Quality) label() string {
	if q.Label != "" {
		return q.Label
	}
	return q.Name
}

func (q Quality) bandwidth() int {
	if q.Bandwidth > 0 {
		return q.Bandwidth
	}
	return defaultBandwidth
}

func (q Quality) outputArgs() []string {
	if len(q.OutputArgs) > 0 {
		return q.OutputArgs
	}
	return DefaultOutputArgs
}

// QualityInfo is a tier as a client sees it on /api/status.
type QualityInfo struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Bandwidth int    `json:"bandwidth"`
}

// QualityList reports the tiers this daemon offers, in config order. The
// first is the default.
func (m *Manager) QualityList() []QualityInfo { return qualityInfos(m.Qualities) }

// qualities is Qualities with the default filled in.
func (m *Manager) qualities() []Quality { return qualitiesOr(m.Qualities) }

// ResolveQuality maps a requested tier name to a configured one. An empty
// or unknown name gets the default rather than an error: a stale
// bookmark, or a client that has never heard of tiers, should get
// television rather than a 404.
func (m *Manager) ResolveQuality(name string) Quality {
	return resolveQuality(m.Qualities, name)
}

// The three below take the tier table as an argument rather than a
// receiver, because there are two things that run these encodes now: live,
// and a recording being re-encoded on demand (see vod.go). One table, one
// set of rules for reading it — a tier that resolves differently depending
// on which of them asked is a tier the URL cannot name.

// qualitiesOr fills in the built-in tier for a daemon whose config declares
// none.
func qualitiesOr(qs []Quality) []Quality {
	if len(qs) == 0 {
		return []Quality{DefaultQuality()}
	}
	return qs
}

func resolveQuality(qs []Quality, name string) Quality {
	qs = qualitiesOr(qs)
	for _, q := range qs {
		if q.Name == name {
			return q
		}
	}
	return qs[0]
}

func qualityInfos(qs []Quality) []QualityInfo {
	qs = qualitiesOr(qs)
	out := make([]QualityInfo, 0, len(qs))
	for _, q := range qs {
		out = append(out, QualityInfo{Name: q.Name, Label: q.label(), Bandwidth: q.bandwidth()})
	}
	return out
}

// gopArgs is the keyframe interval every tier gets, appended after its
// own output args so a tier cannot set a GOP that does not divide the
// segment length. See segmentSeconds.
func gopArgs() []string {
	return []string{"-g", strconv.Itoa(segmentSeconds * outputFPS)}
}
