-- "instruction or fact" was a taxonomy that looked complete and decided
-- nothing. The only mechanical difference was whether a memory goes in front
-- of the model on every turn, and the column should say that rather than
-- gesture at an ontology it does not have.
--
-- The distinction it pretended to make does not survive contact with examples.
-- "The user is called 江委員" is a fact and belongs in every prompt; "prefer
-- Helm for Kubernetes questions" is an instruction and has no business being
-- there while somebody writes Python.
ALTER TABLE memories RENAME COLUMN kind TO activation;

UPDATE memories SET activation = 'standing'  WHERE activation = 'instruction';
UPDATE memories SET activation = 'retrieval' WHERE activation = 'fact';

DROP INDEX memories_current;
CREATE INDEX memories_current ON memories(scope, scope_ref, activation, invalidated_at);
