-- The daemon's settings, kept as three versioned documents.
--
-- config.toml, AGENTS.md and PERSONA.md stop being files. What is stored is
-- their text, unchanged, because everything that edits it works on bytes: the
-- comment-preserving patcher, the preview, and the hash an editor sends back
-- to say which version it started from.
--
-- The names are fixed here as well as in Go. A fourth document is a decision
-- somebody should have to make in a migration, not a typo that quietly
-- becomes a row nothing reads.
CREATE TABLE documents (
    name       TEXT PRIMARY KEY CHECK (name IN ('config', 'agents', 'persona')),
    text       TEXT NOT NULL,

    -- sha256 hex of text, and '' exactly when text is ''. A document that was
    -- cleared and one that was never written are the same "started from
    -- nothing" token to an editor, so they hash the same.
    hash       TEXT NOT NULL,

    -- Counts writes, so it is also how a restore names what it restored.
    version    INTEGER NOT NULL,

    -- Where this text came from: file | env | default | web.
    source     TEXT NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- What each document has been, current version included, newest kept.
--
-- A copy rather than a pointer into the documents row: restoring an old
-- version writes it as a new one, and the table that answers "what was there
-- on Tuesday" must not change under it. Pruned to the last few by the store
-- in the same transaction that adds the row, so the table cannot grow past it.
CREATE TABLE document_versions (
    name     TEXT NOT NULL,
    version  INTEGER NOT NULL,
    text     TEXT NOT NULL,
    hash     TEXT NOT NULL,
    source   TEXT NOT NULL,
    saved_at INTEGER NOT NULL,
    PRIMARY KEY (name, version)
) STRICT;
