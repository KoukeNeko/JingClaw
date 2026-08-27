CREATE TABLE approvals (
    id           TEXT    PRIMARY KEY,
    session_id   TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,

    tool_call_id TEXT    NOT NULL,
    tool_name    TEXT    NOT NULL,
    arguments    TEXT    NOT NULL,

    summary      TEXT    NOT NULL DEFAULT '',
    effects      TEXT    NOT NULL DEFAULT '[]',

    status       TEXT    NOT NULL,
    scope        TEXT    NOT NULL DEFAULT 'once',

    created_at   INTEGER NOT NULL,
    decided_at   INTEGER,
    decided_by   TEXT    NOT NULL DEFAULT ''
) STRICT;

-- One approval per tool call. Answering the same prompt twice, from two
-- clients at once, must not run the tool twice.
CREATE UNIQUE INDEX approvals_by_call ON approvals (run_id, tool_call_id);

-- Startup scans this to find runs paused on a human rather than orphaned by a
-- crash; it stays small however much history accumulates.
CREATE INDEX approvals_pending ON approvals (session_id) WHERE status = 'pending';
