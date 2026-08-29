-- The call as somebody deciding should see it: a diff for an edit, the command
-- line for an execution.
--
-- Alongside the arguments rather than instead of them. The arguments are what
-- will actually run, and a decision made against a rendering that disagreed
-- with them would be a decision about something else.
--
-- Empty for every approval already recorded, and for tools that have nothing
-- clearer to show than their arguments.
ALTER TABLE approvals ADD COLUMN preview TEXT NOT NULL DEFAULT '';
