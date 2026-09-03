-- zerg schema, version 38: a decision records what took it, not only who.
--
-- decided_by says "supervisor", which is a role name. A role's model is edited
-- in the library at any time, so reading today's configuration says nothing
-- about what approved a card last week, and "an opus approved this" against
-- "something cheap approved this" is exactly the question an operator has when
-- they disagree with a decision and want to know how much weight it carries.
--
-- Recorded on the row at the moment the decision is written, from what the
-- deciding process was actually running. Joining an approval to whichever
-- usage_turns row sits nearest its decided_at was the alternative and is the
-- §6.1 mistake again: a value naming an outcome derived from a proxy for it.
-- A guess dressed as a record is worse than an empty column, because nothing
-- afterwards can tell the two apart.
--
-- Empty on every row written before this, and on every operator decision: a
-- person is not a model. Empty is the honest answer where nothing was
-- recorded, and it stays empty rather than being backfilled from configuration
-- that has since moved on.
--
-- Columns, not a rebuilt table. approvals and clarifications both cascade from
-- tasks; schema_014.sql is the record of what a rebuild would destroy.
ALTER TABLE approvals ADD COLUMN decided_model TEXT NOT NULL DEFAULT '';
ALTER TABLE approvals ADD COLUMN decided_harness TEXT NOT NULL DEFAULT '';
ALTER TABLE clarifications ADD COLUMN answered_model TEXT NOT NULL DEFAULT '';
ALTER TABLE clarifications ADD COLUMN answered_harness TEXT NOT NULL DEFAULT '';
