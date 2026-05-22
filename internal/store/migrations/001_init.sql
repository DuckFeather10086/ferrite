-- EPG events ingested from `dvbr epg --schedule --json`.
CREATE TABLE IF NOT EXISTS epg_events (
    service_id  INTEGER NOT NULL,
    event_id    INTEGER NOT NULL,
    start_utc   INTEGER NOT NULL,  -- unix seconds
    duration_s  INTEGER NOT NULL,
    title       TEXT NOT NULL,
    synopsis    TEXT,
    raw         TEXT,              -- original JSON payload, debug
    ingested_at INTEGER NOT NULL,
    PRIMARY KEY (service_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_epg_events_time
    ON epg_events(service_id, start_utc);

-- User- or auto-created recording schedules. The scheduler watches
-- this table and fires at start_utc - lead_s.
CREATE TABLE IF NOT EXISTS schedules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    channel     TEXT NOT NULL,
    service_id  INTEGER,           -- optional, denormalized for clarity
    event_id    INTEGER,           -- nullable for ad-hoc schedules
    start_utc   INTEGER NOT NULL,
    end_utc     INTEGER NOT NULL,
    lead_s      INTEGER NOT NULL DEFAULT 30,
    trail_s     INTEGER NOT NULL DEFAULT 60,
    state       TEXT NOT NULL DEFAULT 'pending',  -- pending|running|done|failed|canceled
    created_at  INTEGER NOT NULL
);

-- Completed (or in-progress) recording artifacts.
CREATE TABLE IF NOT EXISTS recordings (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    schedule_id INTEGER,           -- nullable for ad-hoc rec
    channel     TEXT NOT NULL,
    title       TEXT,
    start_utc   INTEGER NOT NULL,
    end_utc     INTEGER,
    path        TEXT NOT NULL,
    size_bytes  INTEGER,
    state       TEXT NOT NULL DEFAULT 'recording',  -- recording|done|failed
    error       TEXT,
    FOREIGN KEY (schedule_id) REFERENCES schedules(id)
);
CREATE INDEX IF NOT EXISTS idx_recordings_start
    ON recordings(start_utc);
