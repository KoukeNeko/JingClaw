-- A position in the whole log, beside the position within one session.
--
-- seq answers "the how-manyth thing in this conversation", which is what a
-- client watching one conversation resumes from. It cannot answer "how far
-- through the log have I read", because two sessions both at seq 50 make that
-- question meaningless -- and that is the question a console watching every
-- session at once has to ask when it reconnects.
--
-- Timestamps cannot answer it either. A wall clock goes backwards on an NTP
-- correction, and an event appended later can carry an earlier time, so a
-- client resuming from a timestamp silently skips whatever arrives behind it.
-- What a cursor needs is append position, and in a single-writer log that
-- already exists -- it just had no name.
--
-- Allocated the same way seq is: MAX + 1 inside the writing transaction.
-- Nullable rather than NOT NULL because rows written before this migration
-- are backfilled below and a default would have to lie about their order.
ALTER TABLE events ADD COLUMN global_seq INTEGER;

-- Existing rows in the order they were written. Within a session, seq is that
-- order exactly; across sessions, occurred_at is the only record of it there
-- has ever been, so it decides between them. Ties break on session so the
-- result is the same however many times this runs.
UPDATE events
SET global_seq = (
	SELECT COUNT(*)
	FROM events AS earlier
	WHERE earlier.occurred_at < events.occurred_at
	   OR (earlier.occurred_at = events.occurred_at
	       AND (earlier.session_id < events.session_id
	            OR (earlier.session_id = events.session_id
	                AND earlier.seq <= events.seq)))
);

-- A duplicate would be two events claiming one position, and a client
-- resuming from it would be handed one of them and never the other.
CREATE UNIQUE INDEX events_global_seq ON events (global_seq);

-- How far the log has been discarded.
--
-- Pruning leaves gaps in global_seq, which is fine: a cursor has to be
-- monotonic, not contiguous. What is not fine is a client resuming from
-- before a gap and being told nothing, since "there is nothing after your
-- cursor" and "what was after your cursor is gone" are opposite answers.
--
-- One row, enforced by the primary key rather than by everyone remembering.
CREATE TABLE log_watermark (
	id             INTEGER PRIMARY KEY CHECK (id = 1),
	pruned_through INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO log_watermark (id, pruned_through) VALUES (1, 0);
