-- Standing instructions, and the firings they have already accounted for.
--
-- A schedule is not a run and not a session: it is the thing that makes them.
-- What it holds is enough to work out, from here alone, which firings are
-- owed — because a timer inside a process is not the truth. A laptop shut at
-- midnight has to decide on waking what it missed, and a process that knows
-- only "the next tick is in sixty seconds" cannot.
CREATE TABLE schedules (
    id            TEXT PRIMARY KEY,

    -- Counts changes to when or what this does. Part of a firing's identity,
    -- so editing a schedule cannot make a resolved firing look unresolved.
    revision      INTEGER NOT NULL DEFAULT 1,

    expression    TEXT NOT NULL,

    -- By name, not as an offset. "Every day at nine" means nine where
    -- somebody is, and an offset is wrong twice a year.
    zone          TEXT NOT NULL DEFAULT '',

    prompt        TEXT NOT NULL,
    session_id    TEXT NOT NULL REFERENCES sessions(id),

    -- Who set this up. Attribution, and only that: creating a schedule is
    -- delegation, and running one is not impersonation, so this is never who
    -- acts when it fires.
    created_by    TEXT NOT NULL DEFAULT '',

    -- Where the answer goes, said outright rather than inferred from where
    -- the schedule was made. "Made in this channel once" and "post here every
    -- day for a year" are different statements.
    deliver       TEXT NOT NULL DEFAULT '',

    missed_policy TEXT NOT NULL DEFAULT '',
    paused        INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

-- One occasion a schedule was due, and what became of it.
--
-- Keyed by when it was due rather than when it ran, so that reconciling is
-- idempotent: a daemon restarting or waking works out what is owed and cannot
-- create a second run for an occasion already accounted for.
--
-- The revision is in the key because a schedule that changed is a different
-- instruction. Firings resolved under the old one stay resolved.
CREATE TABLE schedule_firings (
    schedule_id   TEXT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    revision      INTEGER NOT NULL,

    -- The time it was due, which is a fact about the schedule.
    due_at        INTEGER NOT NULL,

    -- When something noticed, which is a fact about the machine. Hours apart
    -- on a laptop that slept, and confusing the two is how a log stops being
    -- able to say what happened.
    observed_at   INTEGER NOT NULL,

    -- How many were coalesced into this one, so a late answer can say so.
    missed        INTEGER NOT NULL DEFAULT 0,

    -- Empty when the firing was resolved without running anything, which is
    -- what a skipping schedule does with work that is already stale.
    run_id        TEXT NOT NULL DEFAULT '',

    PRIMARY KEY (schedule_id, revision, due_at)
);

-- Finding a schedule's last firing is the question asked on every reconcile.
CREATE INDEX schedule_firings_recent ON schedule_firings (schedule_id, due_at DESC);
