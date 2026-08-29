-- What the agent said it was going to do, per session.
--
-- A table as well as an event, following the same shape as approvals: the
-- record is what the runtime reads back to apply the next change, and the
-- event is how everything else finds out. Rebuilding the plan by scanning the
-- log for the last plan event would mean reading a week of history to answer
-- a question with one row.
--
-- The whole plan as JSON rather than a row per item. It is read and written
-- whole every time, nothing queries inside it, and a table of items would
-- need an ordering column whose only purpose is to put them back in the order
-- they were written.
CREATE TABLE session_plans (
    session_id TEXT    PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    items      TEXT    NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL
) STRICT;
