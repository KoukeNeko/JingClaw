-- Timestamps are Unix nanoseconds. SQLite has no native time type, and
-- storing integers keeps ordering exact and comparisons cheap.

CREATE TABLE sessions (
    id         TEXT    PRIMARY KEY,
    title      TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE TABLE runs (
    id               TEXT    PRIMARY KEY,
    session_id       TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    status           TEXT    NOT NULL,

    -- Stored as JSON rather than columns because a gateway principal has a
    -- shape this milestone cannot yet pin down, and guessing at columns now
    -- would mean migrating them later.
    origin           TEXT    NOT NULL,
    delivery_targets TEXT    NOT NULL DEFAULT '[]',

    created_at       INTEGER NOT NULL,
    finished_at      INTEGER
) STRICT;

CREATE INDEX runs_by_session ON runs (session_id, created_at);

-- Partial index over live runs only. Startup scans this to find runs orphaned
-- by a crash, and it stays small no matter how much history accumulates.
CREATE INDEX runs_unfinished ON runs (status)
    WHERE status NOT IN ('completed', 'cancelled', 'failed');

CREATE TABLE events (
    session_id  TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,

    -- Dense and 1-based within a session. The composite primary key is what
    -- makes a duplicate sequence number a constraint violation rather than
    -- silent history corruption.
    seq         INTEGER NOT NULL,

    id          TEXT    NOT NULL,
    run_id      TEXT    NOT NULL DEFAULT '',
    occurred_at INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    payload     TEXT    NOT NULL,

    PRIMARY KEY (session_id, seq)
) STRICT;

CREATE UNIQUE INDEX events_by_id ON events (id);
