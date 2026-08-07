package caption

import (
	"os"
	"path/filepath"
	"testing"
)

// The five PTS bytes of a real segment's first video packet, and the answer
// ffprobe gives for them.
//
// Taken off this box rather than constructed, because the point of the test is
// that the byte layout is right and a fixture built by the same understanding
// that built the parser would agree with it either way. `ffprobe
// -show_entries packet=pts_time` on the segment these came from says
// 35563.826900 s, which is 3200744421 ticks of 90 kHz and 35563826 ms.
var (
	realVideoPTSBytes = []byte{0x25, 0xfb, 0x1d, 0xf7, 0xcb}
	realVideoPTSMs    = int64(35_563_826)

	// From the *audio* PES that precedes it in the same segment — 486 ms
	// earlier, which is what a parser that took the first timestamp it found
	// would return instead.
	realAudioPTSBytes = []byte{0x25, 0xfb, 0x1b, 0xa1, 0x93}
	realAudioPTSMs    = int64(35_563_340)
)

// pesPacket builds a PES header carrying one PTS, as ffmpeg's mpegts muxer
// writes it: no DTS, five bytes of header data.
func pesPacket(streamID byte, pts []byte) []byte {
	payload := []byte{
		0x00, 0x00, 0x01, streamID,
		0x00, 0x00, // PES_packet_length, unused here
		0x80, // '10' marker, no scrambling
		0x80, // PTS_DTS_flags = 10: a PTS and no DTS
		byte(len(pts)),
	}
	return append(payload, pts...)
}

// tsPacket wraps a payload in a 188-byte transport packet, padding the tail the
// way a muxer does.
func tsPacket(pid uint16, start bool, adaptation int, payload []byte) []byte {
	packet := make([]byte, tsPacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = 0x47
	packet[1] = byte(pid >> 8)
	if start {
		packet[1] |= 0x40
	}
	packet[2] = byte(pid)
	at := 4
	if adaptation > 0 {
		packet[3] = 0x30 // adaptation field and payload
		packet[4] = byte(adaptation - 1)
		for i := 5; i < 4+adaptation; i++ {
			packet[i] = 0x00
		}
		at = 4 + adaptation
	} else {
		packet[3] = 0x10 // payload only
	}
	copy(packet[at:], payload)
	return packet
}

func writeTS(t *testing.T, packets ...[]byte) string {
	t.Helper()
	var data []byte
	for _, p := range packets {
		data = append(data, p...)
	}
	path := filepath.Join(t.TempDir(), "stream0.ts")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// The one thing this has to get right that is easy to get wrong: audio carries
// timestamps too, and in a real segment ffmpeg muxes an audio PES ahead of the
// video one. The segment window is the *video* clock — it is what the player's
// own timeline is built from — so a parser that stops at the first PTS it finds
// is out by however far the two streams are apart, which on this box is 486 ms.
func TestFirstVideoPTSIgnoresAudio(t *testing.T) {
	path := writeTS(t,
		// PAT and PMT, as every segment starts: PSI, not PES, and skipped.
		tsPacket(0x0000, true, 0, []byte{0x00, 0x00, 0xb0, 0x0d}),
		tsPacket(0x1000, true, 0, []byte{0x00, 0x02, 0xb0, 0x17}),
		tsPacket(0x0101, true, 0, pesPacket(0xc0, realAudioPTSBytes)),
		tsPacket(0x0100, true, 0, pesPacket(0xe0, realVideoPTSBytes)),
	)
	ms, err := firstVideoPTS(path)
	if err != nil {
		t.Fatal(err)
	}
	if ms == realAudioPTSMs {
		t.Fatalf("took the audio timestamp (%d ms); the window is the video clock", ms)
	}
	if ms != realVideoPTSMs {
		t.Errorf("want %d ms, got %d", realVideoPTSMs, ms)
	}
}

// A video packet with an adaptation field — which is every segment's first one,
// since that is where the PCR rides — has its payload pushed along by the
// field's own length.
func TestFirstVideoPTSSkipsTheAdaptationField(t *testing.T) {
	path := writeTS(t, tsPacket(0x0100, true, 8, pesPacket(0xe0, realVideoPTSBytes)))
	ms, err := firstVideoPTS(path)
	if err != nil {
		t.Fatal(err)
	}
	if ms != realVideoPTSMs {
		t.Errorf("want %d ms, got %d", realVideoPTSMs, ms)
	}
}

// Only the packet that *starts* a PES carries the header. The rest of the frame
// is continuation, and reading a PES header out of one is reading picture data
// as a timestamp.
func TestFirstVideoPTSIgnoresContinuationPackets(t *testing.T) {
	path := writeTS(t,
		tsPacket(0x0100, false, 0, pesPacket(0xe0, realAudioPTSBytes)),
		tsPacket(0x0100, true, 0, pesPacket(0xe0, realVideoPTSBytes)),
	)
	ms, err := firstVideoPTS(path)
	if err != nil {
		t.Fatal(err)
	}
	if ms != realVideoPTSMs {
		t.Errorf("want %d ms, got %d", realVideoPTSMs, ms)
	}
}

// A segment with no video in it at all — a playlist entry ffmpeg is still
// writing — is reported rather than guessed at, so the caller can leave the
// window alone and try again on the next tick.
func TestFirstVideoPTSReportsWhenThereIsNone(t *testing.T) {
	path := writeTS(t,
		tsPacket(0x0101, true, 0, pesPacket(0xc0, realAudioPTSBytes)),
		tsPacket(0x0101, true, 0, pesPacket(0xc0, realAudioPTSBytes)),
		tsPacket(0x0101, true, 0, pesPacket(0xc0, realAudioPTSBytes)),
	)
	if _, err := firstVideoPTS(path); err == nil {
		t.Error("want an error for a segment carrying no video PTS")
	}
}

// 0x47 is an ordinary byte inside a payload, so finding one proves nothing on
// its own — a packet boundary is one with more 0x47s exactly 188 bytes apart.
func TestSyncOffsetNeedsMoreThanOneSyncByte(t *testing.T) {
	real := tsPacket(0x0100, true, 0, pesPacket(0xe0, realVideoPTSBytes))
	var data []byte
	// A stray 0x47 ahead of the stream, in the position a naive scan takes.
	data = append(data, 0x47, 0x11, 0x22)
	for i := 0; i < 3; i++ {
		data = append(data, real...)
	}
	if got := syncOffset(data); got != 3 {
		t.Errorf("want the real packet start at 3, got %d", got)
	}
}
