-- Whether a run is a turn of the conversation or work done inside one.
--
-- A worker's events are in the log like everything else — "why did it decide
-- that" has to stay answerable — but they are not part of the conversation.
-- Keeping a search's hundred tool results out of what the model reads again
-- is the whole reason for delegating the search.
--
-- Empty is a conversation, so every run written before this is one.
ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT '';

-- Which run asked for it. Recorded rather than inferred: after a crash the
-- question is which run was waiting on which, and the alternative is guessing
-- from a parent's unfinished tool call.
ALTER TABLE runs ADD COLUMN parent_run_id TEXT NOT NULL DEFAULT '';

-- Finding a run's workers is how a client shows what it delegated.
CREATE INDEX runs_parent ON runs (parent_run_id) WHERE parent_run_id != '';
