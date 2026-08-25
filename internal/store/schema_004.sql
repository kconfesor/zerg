-- zerg schema, version 4: rework.
--
-- Nothing in the router forbids a backward handoff, and that is deliberate:
-- rework is normal, and a strict one-way pipeline would make a reviewer unable
-- to return work it just found problems in.
--
-- But backward edges mean cycles, and every lap costs real tokens. A counter
-- makes a loop visible instead of expensive-and-silent.
ALTER TABLE tasks ADD COLUMN rework_count INTEGER NOT NULL DEFAULT 0;
