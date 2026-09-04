-- zerg schema, version 37: a card that is supervised still lands with a person.
--
-- Issue #38. An architect sidecar will decide mid-pipeline gates and questions
-- on a card that asked for it. That process does not exist yet. What has to
-- exist first is the rule that makes the feature safe on Default: coder then
-- reviewer, both ungated, so a finished card merges unattended. Delegating
-- without this column would auto-answer questions and still land, which is
-- the opposite of "the final click stays human".
--
-- supervised is a property of the card, not of the finishing role's gate. The
-- gate field would move if that role were skipped or turned off, which is the
-- same failure as taking terminality from line order.
--
-- decided_by / answered_by record who actually clicked, so a later trail can
-- tell a person from the sidecar. Empty while pending. "operator" is the
-- cockpit; a role name is an agent, once those verbs exist.
--
-- Columns, not a rebuilt table: tasks, approvals and clarifications all
-- cascade, and schema_014.sql is the record of what DROP would destroy.
ALTER TABLE tasks ADD COLUMN supervised INTEGER NOT NULL DEFAULT 0;
ALTER TABLE approvals ADD COLUMN decided_by TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN evidence_sha TEXT NOT NULL DEFAULT '';
ALTER TABLE clarifications ADD COLUMN answered_by TEXT NOT NULL DEFAULT '';
ALTER TABLE clarifications ADD COLUMN evidence_sha TEXT NOT NULL DEFAULT '';
