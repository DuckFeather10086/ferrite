# Handoff

## 2026-06-18 — first real-hardware E2E run of the b25 pipeline

End-to-end verified against the actual tuner through the **full isdbd
HTTP path** (not just the raw `dvbr | b25 | ffmpeg` shell pipeline):

```
GET /api/live/TOKYO MX1.m3u8
  → dvbr tune (adapter0 lock) → b25 descramble → ffmpeg HLS
  → segment served by API
```

The API-fetched segment ffprobes as **mpeg2video 1440x1080 + AAC 48kHz**,
and `ffmpeg -i seg.ts -an -f null -` decodes a clean **58-frame GOP
(4 I / 16 P / 38 B) with zero errors** → descrambling confirmed. (Note:
checking the scrambling-control bits of the *ffmpeg-output* segment proves
nothing — ffmpeg always emits clean TS. The decode succeeding is the
proof: a scrambled MPEG2 stream can't yield a decodable GOP.)

Known-good channels on this box (Tokyo/Kanagawa): `TOKYO MX1`, `TBS1`,
`asahi`, `NHKEFl1El5~`, `tvk1`. The J:COM cable muxes (473142857 /
485142857) have no antenna signal and will block until tune timeout.

### Config change in this commit

`configs/isdbd.toml`:

```
- dvbr_bin = "../dvbr/target/release/dvbr"
+ dvbr_bin = "../target/release/dvbr"
```

The `dvbr` release binary builds into the **parent virtual workspace**
`target/release/dvbr`, not `dvbr/target/release/dvbr`. (`b25_bin` at
`../libaribb25-rs/target/release/b25` was already correct.) Run isdbd
from the `isdbd/` dir so the relative bin/channels/storage paths resolve.

### To reproduce the verification

1. Build: `go build -o /tmp/isdbd ./cmd/isdbd` (Go isn't on the non-login
   PATH — `export PATH=/usr/local/go/bin:$PATH` first).
2. **Disable EPG for the run** (see open issue below) — point `-config`
   at a copy of `configs/isdbd.toml` with `epg_channels = []`.
3. From `isdbd/`: `/tmp/isdbd -config <that>.toml`.
4. `curl /api/live/TOKYO%20MX1.m3u8`, wait ~10-15s for the first
   segment, fetch a `.ts` under `/api/live/`, then
   `ffmpeg -i seg.ts -an -f null -` should decode without errors.

## OPEN ISSUE — EPG startup scan starves live HLS (single adapter)

**Symptom:** first `GET /api/live/{ch}.m3u8` after boot returns 404 and
"live looks broken."

**Root cause:** `epg.Refresher.Run` does an immediate first pass on
startup (then on `epg_cron`). It spawns `dvbr epg --schedule ...` which
takes the flock on `/tmp/dvbr-adapter0.lock` + opens the DVB device —
**outside `tuner.Pool`**, so the Pool's in-memory mutex doesn't serialize
it. A concurrent live request's own `dvbr tune` then fails with
`DVB adapter 0 is already in use`, exits, feeds empty input to
b25 → ffmpeg ("Invalid data found"), no segment is written, and the
playlist endpoint 404s (ServeFile on a missing m3u8). EIT `--schedule`
scans run for minutes, so the whole window is blocked.

**Fix options (not done):**
- Route `dvbr epg` through `tuner.Pool` so EPG / live / recordings share
  one adapter arbiter; or
- Make EPG opportunistic — skip/defer the startup pass and yield the
  adapter to live + recording demand. A single adapter can't do EPG and
  live simultaneously.

**Workaround until fixed:** `epg_channels = []` disables EPG (gate is
`len(cfg.EPGChannels) > 0 && cfg.DvbrBin != ""` in `cmd/isdbd/main.go`).
