-- Measured audio/video PTS skew per channel.
--
-- ISDB-T muxes interleave audio ahead of the first decodable video frame,
-- so live HLS needs the audio delayed by that difference. Measuring it
-- costs an ffprobe pass over the first seconds of the stream (~5s), which
-- is the single biggest chunk of channel-change latency.
--
-- The skew is a property of the broadcaster's mux, not of the moment, so
-- it is measured once and reused. offset_s is the *raw* measurement;
-- audio_offset_bias from the daemon config is applied at use time so
-- changing it doesn't require re-probing.
CREATE TABLE IF NOT EXISTS av_offsets (
    channel      TEXT PRIMARY KEY,   -- canonical config.Channel.Name
    offset_s     REAL NOT NULL,      -- video_pts - audio_pts, seconds
    measured_utc INTEGER NOT NULL
);
