-- Inactivity expiry, removed.
--
-- It retired a memory nobody had recalled for ninety days. The mistake is
-- that "nobody asked" is not evidence of anything: the production namespace
-- of a service nobody has deployed since spring is correct, important, and
-- cold, and it died on the same schedule as a note about a merged branch.
--
-- Worse, it was anti-correlated with the problem it was for. Corpus rot is
-- caused by near-duplicates — five phrasings of one preference — and those
-- are recalled constantly, so they never expired. The mechanism removed
-- exactly the memories that were not the problem and kept exactly the ones
-- that were.
--
-- It also made retrieval frequency decide truth, and made a read a write:
-- one bad match extended a wrong memory's life by three months.
--
-- What stays is valid_from and valid_until, which describe the world rather
-- than how often anybody asked.
DROP INDEX IF EXISTS memories_expiring;

ALTER TABLE memories DROP COLUMN expires_at;
ALTER TABLE memories DROP COLUMN last_used_at;
