-- zerg schema, version 37: knowing which agents the last daemon left running.
--
-- schema_036 made a restart put the swarm back. What it did not do is account
-- for the agents the previous run left behind, and the two together are worse
-- than either alone.
--
-- Each agent runs in a process group of its own so a bash tool call's
-- descendants can be killed as a unit, and the group is signalled from
-- cmd.Cancel, which a SIGKILLed daemon never reaches. Measured before this
-- table existed: after `kill -9` on the daemon, a coder was still running
-- thirty seconds later and still writing files into its worktree. The next
-- daemon then reclaimed that agent's lease and, with resumeOnStart, spawned a
-- replacement into the same worktree with the same conversation resumed. Two
-- harnesses, one directory, both committing.
--
-- A pid alone cannot be signalled safely: the previous daemon may have died
-- days ago and the number now belongs to somebody else's process. identity is
-- what makes the pid checkable -- the process's own start time and command as
-- the operating system reports them, captured when it was spawned. A row whose
-- identity no longer matches is a pid that was reused, and the only correct
-- action is to leave that process alone.
--
-- worktree is here for the message rather than the mechanism: an operator told
-- that a resume was refused wants to know which directory is still occupied.
CREATE TABLE agent_processes (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role       TEXT NOT NULL,
    pid        INTEGER NOT NULL,
    pgid       INTEGER NOT NULL,
    identity   TEXT NOT NULL,
    worktree   TEXT NOT NULL,
    started_at TEXT NOT NULL,
    PRIMARY KEY (project_id, role)
);
