-- Which model answers in this session. Empty uses the configured one, which is
-- what almost every session does.
--
-- Per session rather than per run: a conversation whose model changed halfway
-- through is one whose earlier turns were written by a different writer, and
-- "why did it get worse" would have no visible answer.
ALTER TABLE sessions ADD COLUMN model TEXT NOT NULL DEFAULT '';
