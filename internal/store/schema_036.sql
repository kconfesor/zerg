-- zerg schema, version 36: surviving a restart of the daemon.
--
-- Everything the *queue* knows already survives one. Leases are reclaimed at
-- boot, interrupted approvals are settled against the repository, and the board
-- is a table. Two things did not, and both of them are the parts a person
-- notices.
--
-- The first is that nobody recorded the operator ever wanting the project to be
-- running. A swarm existed only as a map in memory, so the daemon came back
-- with every project stopped and the queue full, waiting for somebody to press
-- Start again. `sessions.ended_at` cannot answer this: shutdown fills it in, so
-- a clean stop and a deliberate one are the same row afterwards.
--
-- start_requested_at is that intent, and it is the operator's rather than the
-- daemon's: set when they press Start, cleared when they press Stop, and left
-- alone by anything that is merely the process ending. A daemon that comes back
-- to a non-NULL value is looking at a project somebody still wants running.
--
-- A timestamp rather than a flag, for the reason schema_014 gives: it answers
-- "since when", which is the question asked about a project that has been
-- resuming itself for a week.
ALTER TABLE projects ADD COLUMN start_requested_at TEXT;

-- The second is the agent's own memory.
--
-- Both supported harnesses keep a conversation on disk and will resume it
-- (claude --resume, pi --session-id), and zerg has advertised that it can --
-- adapter.Caps.ResumeSession has been true for both since they were written --
-- while passing neither flag anywhere. Every spawn therefore started a cold
-- session: the queue handed the work back, and the agent that received it had
-- no memory of having already read the repository, made a decision, or half
-- written the change it was being asked for again.
--
-- This is not only about the daemon. A cerebrate restarts its agent on any
-- crash, with backoff, and that respawn was cold too, which is the far more
-- common case.
--
-- session_id is what the harness *said* it was running, latched from its own
-- stream, never what zerg passed it. claude answers --resume on a session that
-- is somehow still live by forking to a new id and saying so; trusting the id
-- we sent would then resume a conversation nobody is writing to.
--
-- fingerprint is what the session was started under: harness, model, thinking
-- level and the composed system prompt. A role restarts when its configuration
-- changes precisely so the new configuration applies (ARCHITECTURE §11.3), and
-- resuming across that edit would replay a conversation shaped by the old
-- instructions while claiming the new ones are in force. A fingerprint that no
-- longer matches is a session that must not be resumed.
CREATE TABLE role_sessions (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    role        TEXT NOT NULL,
    harness     TEXT NOT NULL,
    session_id  TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (project_id, role)
);
