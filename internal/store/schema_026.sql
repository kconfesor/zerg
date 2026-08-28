-- Where a project's work can be run or sent.
--
-- A target is a name and a command, because configuration here is rows and
-- zerg knows nothing about Docker, Vercel or Fly: it runs what it is told, in
-- the repository, and reports what happened. Anything provider-specific lives
-- in the command, where the operator can read it.
--
-- kind carries both values from the start. A local target is a preview the
-- daemon runs on this machine and proxies; a remote one sends the work
-- somewhere else and needs credentials, which is issue #9's second half. The
-- CHECK names both now because a shipped CHECK cannot be widened later without
-- rebuilding the table.
CREATE TABLE deploy_targets (
    id         TEXT NOT NULL PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('local', 'remote')),

    -- Run through a shell, in the worktree, with PORT set for a local target.
    -- Foreground is the contract: the daemon owns the process group and stops
    -- it by killing that group, which is what makes `docker compose up` stop
    -- its containers.
    command    TEXT NOT NULL,

    -- Where to run it, relative to the checkout. Empty is the root, which is
    -- what a compose file at the top of a repository wants.
    cwd        TEXT NOT NULL DEFAULT '',

    -- How to put it away, when killing the process group is not enough.
    --
    -- `docker compose up` interrupted stops its containers and leaves them
    -- exited, one set per preview, for ever. Nothing generic can know that:
    -- the command knows what it started, so the target says how to undo it
    -- (`docker compose down`). Empty means killing the group is the whole
    -- story, which is true of every dev server.
    stop_command TEXT NOT NULL DEFAULT '',

    -- How long to wait for the port to answer before calling it failed.
    -- A container image that has to be pulled is minutes; a vite preview is
    -- seconds, and neither should be the other's timeout.
    ready_secs INTEGER NOT NULL DEFAULT 120,

    created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX deploy_targets_name ON deploy_targets (project_id, name);

-- Who owns a running service, which decides what stops it.
--
-- An agent's dev server is a child of the swarm and dies with it. A preview
-- the daemon started is not: the reason to run one is to click around after
-- the pipeline finished, so the swarm going down must leave it alone. Without
-- this column the swarm's shutdown marked every service stopped, including the
-- one still serving.
ALTER TABLE artifacts ADD COLUMN owner TEXT NOT NULL DEFAULT 'agent';
