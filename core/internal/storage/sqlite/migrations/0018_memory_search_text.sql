-- The memory index, rebuilt over storage.SearchText.
--
-- The default tokenizer reads a run of Chinese characters as one word, so a
-- memory written in Chinese was found by its whole sentence and by nothing
-- shorter: 使用者偏好用繁體中文回覆 did not come back for 繁體中文. The duplicate
-- check searches the same index, so it missed those too.
--
-- The index now holds the rewrite rather than the text. The rewrite is Go, so
-- the store writes each entry beside the memory in the same transaction, and
-- fills in any memory the index lacks when the database is opened — which is
-- every memory here, the first time. Only the deletion stays a trigger: it
-- needs no rewrite, and it is what keeps a deletion from leaving the index
-- claiming the agent still believes something.
DROP TRIGGER memories_fts_insert;
DROP TRIGGER memories_fts_delete;
DROP TRIGGER memories_fts_update;
DROP TABLE memories_fts;

CREATE VIRTUAL TABLE memories_fts USING fts5(
    text,
    content = '',
    contentless_delete = 1
);

CREATE TRIGGER memories_fts_delete AFTER DELETE ON memories BEGIN
    DELETE FROM memories_fts WHERE rowid = old.rowid;
END;
