-- Memory that crosses sessions.
--
-- A separate table rather than a projection of the event log, because a fact
-- learned in one session has to be found while replaying a different one, and
-- a projection has no answer to "a projection of which log".
--
-- Provenance is not nullable. A memory whose origin has been lost is a claim
-- nobody can check, and losing it is exactly how untrusted text becomes a
-- trusted fact: by passing through a summary that dropped where it came from.
CREATE TABLE memories (
    id              TEXT PRIMARY KEY,

    scope           TEXT NOT NULL,   -- workspace | principal
    scope_ref       TEXT NOT NULL,
    kind            TEXT NOT NULL,   -- instruction | fact
    text            TEXT NOT NULL,

    -- The least trusted thing that contributed, not the trust of whoever
    -- approved it. It only ever travels downwards.
    trust           TEXT NOT NULL,

    origin_kind     TEXT NOT NULL,   -- local_client | gateway
    origin_client   TEXT NOT NULL DEFAULT '',
    origin_platform TEXT NOT NULL DEFAULT '',
    origin_principal TEXT NOT NULL DEFAULT '',

    source_session  TEXT NOT NULL,
    source_seq      INTEGER NOT NULL,

    -- Every memory has one. Nothing is written without a person.
    approved_by     TEXT NOT NULL,

    created_at      INTEGER NOT NULL,

    -- A fact that stopped being true is invalidated, not deleted: "what is
    -- true now" and "what was true then" are different questions.
    invalidated_at  INTEGER,
    superseded_by   TEXT REFERENCES memories(id)
);

CREATE INDEX memories_current ON memories(scope, scope_ref, kind, invalidated_at);
CREATE INDEX memories_source ON memories(source_session);

-- Full text search over the same rows rather than a copy of them, so a
-- deletion cannot leave the index claiming the agent still believes something.
CREATE VIRTUAL TABLE memories_fts USING fts5(
    text,
    content = 'memories',
    content_rowid = 'rowid'
);

CREATE TRIGGER memories_fts_insert AFTER INSERT ON memories BEGIN
    INSERT INTO memories_fts(rowid, text) VALUES (new.rowid, new.text);
END;

CREATE TRIGGER memories_fts_delete AFTER DELETE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
END;

CREATE TRIGGER memories_fts_update AFTER UPDATE ON memories BEGIN
    INSERT INTO memories_fts(memories_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
    INSERT INTO memories_fts(rowid, text) VALUES (new.rowid, new.text);
END;
