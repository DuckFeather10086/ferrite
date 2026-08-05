-- What became of a finished recording after the tuner let go of it.
--
-- A recording is MPEG-2 video in a transport stream: no browser can decode it,
-- and its captions are on a PID nothing but arib-caption reads. The post-pass
-- turns each finished one into an .mp4 a <video> element can open, with .ass
-- and .vtt sidecars beside it.
--
-- Only the state is here. The derived files are named after the recording's own
-- path, so they inherit the storage-root check that column already gets rather
-- than adding three more filesystem paths from a database to guard.
ALTER TABLE recordings ADD COLUMN post_state TEXT;
ALTER TABLE recordings ADD COLUMN post_error TEXT;

-- Everything recorded before this existed is left alone. The column defaults to
-- NULL, and NULL is what the queue looks for, so without this line the first
-- start after the upgrade would quietly begin transcoding every recording on
-- the disk — hours of it, at once, on a box whose job is to keep recording.
-- Anything wanted can be asked for by hand.
UPDATE recordings SET post_state = 'skipped' WHERE post_state IS NULL;
