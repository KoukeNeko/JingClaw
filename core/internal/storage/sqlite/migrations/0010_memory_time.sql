-- Two timelines instead of one.
--
-- created_at and invalidated_at are record time: when this agent learned
-- something, and when it stopped believing it. valid_from and valid_until are
-- valid time: when the thing was true in the world.
--
-- They come apart constantly. "The API was v1 until March" is learned in June
-- about a period that ended in March, and a store with one timeline has to
-- pick which of those two dates to lose. Keeping both is what makes "what did
-- it believe when that run happened" a question with an answer.
--
-- valid_from is NOT NULL and defaults to zero, which the reader treats as
-- "since it was learned" — the honest answer when nobody said otherwise.
ALTER TABLE memories ADD COLUMN valid_from INTEGER NOT NULL DEFAULT 0;
ALTER TABLE memories ADD COLUMN valid_until INTEGER;

-- Record hygiene, not truth. A fact nobody has wanted for two months is
-- probably not wrong; it is probably noise, and a retrieval corpus that only
-- grows gets worse at answering as it does.
--
-- Reaching this invalidates rather than deletes, for the same reason a
-- correction does: "what is true now" and "what was true then" are different
-- questions.
ALTER TABLE memories ADD COLUMN expires_at INTEGER;
ALTER TABLE memories ADD COLUMN last_used_at INTEGER;

-- Sweeping for what has expired reads this and nothing else.
CREATE INDEX memories_expiring ON memories (expires_at)
    WHERE expires_at IS NOT NULL AND invalidated_at IS NULL;
