-- Things the agent asked a person, and what came back.
--
-- Its own table rather than a column on approvals. An approval asks whether
-- something may happen and is answered yes or no; this asks what the person
-- wants and is answered with their words. One table would mean every read of
-- either checking which kind it had got.
CREATE TABLE questions (
    id           TEXT    PRIMARY KEY,
    session_id   TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,

    tool_call_id TEXT    NOT NULL,

    prompt       TEXT    NOT NULL,
    kind         TEXT    NOT NULL,
    options      TEXT    NOT NULL DEFAULT '[]',

    status       TEXT    NOT NULL,
    answer       TEXT    NOT NULL DEFAULT '',
    answered_by  TEXT    NOT NULL DEFAULT '',

    created_at   INTEGER NOT NULL,
    answered_at  INTEGER
) STRICT;

-- One question per tool call. Answering the same prompt twice, from two
-- clients at once, must not resume the run twice.
CREATE UNIQUE INDEX questions_by_call ON questions (run_id, tool_call_id);

-- Startup scans this to find runs paused on a person rather than orphaned by
-- a crash; it stays small however much history accumulates.
CREATE INDEX questions_pending ON questions (session_id) WHERE status = 'pending';
