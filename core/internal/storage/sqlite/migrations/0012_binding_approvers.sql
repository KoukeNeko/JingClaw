-- Who may answer an approval from a channel, as opposed to who may ask for
-- work in it.
--
-- Two columns rather than a flag on the existing ones, because they answer
-- different questions and a deployment routinely wants different answers:
-- a room where several people can ask the agent for things, and one or two
-- of them can permit what it asks to do.
--
-- Default empty, which means nobody. A binding that predates this migration
-- therefore gains no new power by being read by a newer daemon.
ALTER TABLE gateway_bindings ADD COLUMN approving_principals TEXT NOT NULL DEFAULT '[]';
ALTER TABLE gateway_bindings ADD COLUMN approving_claims     TEXT NOT NULL DEFAULT '[]';
