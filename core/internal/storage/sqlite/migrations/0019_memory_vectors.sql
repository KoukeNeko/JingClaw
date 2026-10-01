-- What an embedding model made of each memory, for finding one by meaning.
--
-- Keyed by memory and model together, because changing the model makes
-- every vector from the old one meaningless next to the new one; the old
-- rows are simply never read again. A memory that is forgotten takes its
-- vectors with it.
CREATE TABLE memory_vectors (
    memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    vector BLOB NOT NULL,
    PRIMARY KEY (memory_id, model)
) STRICT;
