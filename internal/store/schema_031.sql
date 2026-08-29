-- Conversations, so a project can hold more than one.
--
-- Chat was one per project, identified by the role its events carried. That
-- makes a second question about a second subject either an interruption of the
-- first or a reason to end it: the only way to start fresh was to delete what
-- was there. People keep several conversations going for the same reason they
-- keep several tabs open, and each wants its own thread and its own files.
CREATE TABLE chats (
  id          TEXT PRIMARY KEY,
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  -- Title is what the tab says. Written by the person, or taken from their
  -- first message when they have not said.
  title       TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  -- LastUsedAt orders the tabs, so the one being worked in stays to hand.
  last_used_at TEXT NOT NULL
);

CREATE INDEX chats_by_project ON chats(project_id, last_used_at DESC);

-- Which conversation an event belongs to.
--
-- A column rather than a role of its own ("chat:01M…"): the role is what the
-- cockpit filters, colours and replays on, and encoding an id in it would make
-- every one of those a string match. Null for everything that is not a
-- conversation, which is almost every event ever recorded.
ALTER TABLE events ADD COLUMN chat_id TEXT;

CREATE INDEX events_by_chat ON events(chat_id, id) WHERE chat_id IS NOT NULL;

-- And which conversation a file was attached to, so deleting one tab takes its
-- own attachments and leaves the others alone.
ALTER TABLE artifacts ADD COLUMN chat_id TEXT;

-- Everything already said belongs to the conversation that was there.
--
-- One row per project that has any chat history, keeping the transcript rather
-- than starting people at an empty tab with their questions deleted underneath
-- them. The timestamps come from the events themselves, so the tab is as old as
-- the conversation in it.
INSERT INTO chats (id, project_id, title, created_at, last_used_at)
SELECT
  -- Derived from the project rather than generated: a migration has no id
  -- generator, and a derived id is the same one the two updates below use.
  'chat-' || e.project_id,
  e.project_id,
  -- Named after the first thing the person said in it, the way a new tab is.
  -- Left empty it would read "New chat" while holding a fortnight of history.
  COALESCE((
    SELECT substr(replace(o.text, char(10), ' '), 1, 60)
      FROM events o
     WHERE o.project_id = e.project_id AND o.role = 'operator'
       AND o.kind = 'message' AND COALESCE(o.text, '') <> ''
     ORDER BY o.id LIMIT 1
  ), ''),
  MIN(e.ts),
  MAX(e.ts)
FROM events e
WHERE e.role IN ('chat', 'operator')
GROUP BY e.project_id;

UPDATE events
   SET chat_id = 'chat-' || project_id
 WHERE role IN ('chat', 'operator');

UPDATE artifacts
   SET chat_id = 'chat-' || project_id
 WHERE role = 'operator' AND task_id IS NULL;
