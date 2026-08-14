package caption

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Reading the clock out of a segment, which is the one thing about an HLS
// segment that only the segment itself knows.
//
// This used to be `ffprobe -show_entries packet=pts_time -read_intervals %+#1`,
// one subprocess per five segments per rendition. It gave the right answer and
// it was breaking the picture: on this box, spawning it beside a real-time x264
// encode stalls ffmpeg's read of the tune long enough to move a keyframe, so the
// HLS muxer cuts late and then early to catch up. Measured over three
// back-to-back runs on one channel — 22% of segments off their advertised
// length with the probe, 3% with captions off entirely, 1% with captions on and
// the probe fired once. The caption pipeline was corrupting the segmentation it
// then had to measure.
//
// So it is done here instead. It is the same number by a shorter route, and
// cheap enough that every segment can be measured rather than derived.
//
// # Where a timestamp lives in a transport stream
//
// Not in the TS packet header — that is four bytes of routing and nothing else.
// A PTS is in the *PES* header, which is the payload of the one packet in a
// frame that has `payload_unit_start_indicator` set. So most packets carry no
// timestamp at all, and finding the first video PTS means walking packets until
// one starts a PES whose `stream_id` is in the video range.
//
//	TS packet, 188 bytes
//	  [0]      0x47                               sync
//	  [1]      bit7 transport_error_indicator
//	           bit6 payload_unit_start_indicator   this packet starts a PES
//	           bits4-0 ┐
//	  [2]      bits7-0 ┴ PID, 13 bits
//	  [3]      bits5-4 adaptation_field_control    10/11 → an adaptation field
//	                                               00/10 → no payload
//	  payload
//	    [0..2] 00 00 01                           packet_start_code_prefix
//	    [3]    stream_id                          0xE0..0xEF video, 0xC0..0xDF audio
//	    [7]    bits7-6 PTS_DTS_flags              bit7 set → a PTS follows
//	    [8]    PES_header_data_length
//	    [9..13] the PTS
//
// `stream_id` is why there is no PAT/PMT parsing here: the PES header says for
// itself whether it is video, so the program map adds nothing.

const tsPacketSize = 188

// How much of a segment to read looking for the first video PES.
//
// An HLS segment begins with an IDR frame by construction, so the answer is in
// the first few packets; this is slack for the PAT/PMT and any audio ffmpeg
// muxed ahead of it. A whole 720p segment is around 400 KB, so on a tmpfs this
// is at worst the same read ffprobe was doing and usually a fraction of it.
const ptsScanBytes = 128 * 1024

// errNoVideoPTS means the bytes read carried no video PES with a timestamp.
var errNoVideoPTS = errors.New("caption: no video PTS in segment")

// firstVideoPTS is the presentation timestamp of a segment's first video
// packet, in milliseconds.
//
// With ffmpeg's -copyts *and* -muxdelay 0 this is a broadcast PTS — the same
// clock the captions are decoded against, which is the entire reason it is
// worth measuring. Both flags: -copyts stops ffmpeg rebasing the timeline to
// zero, and -muxdelay 0 stops the MPEG-TS muxer adding its own 1.4s to what it
// writes. Nothing here can tell the difference — a shifted segment reads back
// perfectly self-consistently — so the check that this number means what it
// says is a content one, against a recording of the same tune.
func firstVideoPTS(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	buf := make([]byte, ptsScanBytes)
	n, err := io.ReadFull(f, buf)
	// A segment shorter than the scan window is ordinary — that is most of
	// them — and only a read that returned nothing is a failure.
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, err
	}
	ticks, ok := parseFirstVideoPTS(buf[:n])
	if !ok {
		return 0, fmt.Errorf("%w %s", errNoVideoPTS, filepath.Base(path))
	}
	// 90 kHz, so a tick is exactly 1/90 ms and this division is not lossy in
	// any way the caller can see: the value it replaces came back from ffprobe
	// as seconds and was multiplied by 1000 into the same truncation.
	return ticks / 90, nil
}

// parseFirstVideoPTS walks whole TS packets and returns the first video PTS in
// 90 kHz ticks.
func parseFirstVideoPTS(data []byte) (int64, bool) {
	base := syncOffset(data)
	if base < 0 {
		return 0, false
	}
	for off := base; off+tsPacketSize <= len(data); off += tsPacketSize {
		packet := data[off : off+tsPacketSize]
		if packet[0] != 0x47 {
			// Lost alignment — a truncated write, or the read landed mid-file.
			// Re-syncing from here is cheaper than giving up on the segment.
			next := syncOffset(data[off:])
			if next <= 0 {
				return 0, false
			}
			off += next - tsPacketSize
			continue
		}
		// transport_error_indicator: the demux says this packet arrived
		// damaged, so nothing in it can be trusted, timestamps least of all.
		if packet[1]&0x80 != 0 || packet[1]&0x40 == 0 {
			continue
		}
		payload, ok := payloadOf(packet)
		if !ok {
			continue
		}
		if ticks, ok := videoPTS(payload); ok {
			return ticks, true
		}
	}
	return 0, false
}

// syncOffset finds the start of the first whole packet.
//
// 0x47 occurs freely inside payloads, so one on its own proves nothing: a real
// packet boundary has another one exactly 188 bytes later, and another after
// that. Confirmed against as many following boundaries as the bytes in hand
// allow, rather than a fixed three — a segment being written can offer fewer,
// and refusing to sync on it would mean refusing to measure it at all. A stream
// that does not sync within one packet length is not a transport stream.
func syncOffset(data []byte) int {
	for i := 0; i < tsPacketSize && i+tsPacketSize <= len(data); i++ {
		if data[i] != 0x47 {
			continue
		}
		confirmed := true
		for k := 1; k <= 2; k++ {
			at := i + k*tsPacketSize
			if at >= len(data) {
				break
			}
			if data[at] != 0x47 {
				confirmed = false
				break
			}
		}
		if confirmed {
			return i
		}
	}
	return -1
}

// payloadOf returns a packet's payload, skipping the adaptation field.
func payloadOf(packet []byte) ([]byte, bool) {
	switch (packet[3] >> 4) & 0x03 {
	case 0x01: // payload only
		return packet[4:], true
	case 0x03: // adaptation field, then payload
		length := int(packet[4])
		start := 5 + length
		if start >= tsPacketSize {
			return nil, false
		}
		return packet[start:], true
	default: // 0x00 reserved, 0x02 adaptation field only — no payload either way
		return nil, false
	}
}

// videoPTS reads the timestamp out of a PES header, if this is the start of a
// video PES that carries one.
func videoPTS(payload []byte) (int64, bool) {
	if len(payload) < 14 {
		return 0, false
	}
	if payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
		return 0, false
	}
	// 0xE0..0xEF is video; audio (0xC0..0xDF) carries timestamps too and is not
	// what the video segments are timed by.
	if payload[3] < 0xE0 || payload[3] > 0xEF {
		return 0, false
	}
	// PTS_DTS_flags is two bits: 0b10 a PTS, 0b11 a PTS and a DTS, 0b00
	// neither. Either way the PTS is the first five bytes of the header data,
	// so the high bit is the whole question.
	if payload[7]&0x80 == 0 || payload[8] < 5 {
		return 0, false
	}
	b := payload[9:14]
	// 33 bits, with a marker bit stuffed after every seven or eight of them —
	// which is why this cannot be read as an integer and has to be reassembled.
	pts := int64(b[0]>>1&0x07)<<30 |
		int64(b[1])<<22 |
		int64(b[2]>>1&0x7F)<<15 |
		int64(b[3])<<7 |
		int64(b[4]>>1&0x7F)
	return pts, true
}
