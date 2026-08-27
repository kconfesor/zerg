---
description: Add a schema migration without destroying anyone's history
argument-hint: [what the migration changes]
allowed-tools: Bash(ls:*), Bash(sqlite3:*), Bash(go:*), Read, Write, Edit, Grep
---

Add a migration for: $ARGUMENTS

Read `internal/store/schema_014.sql` first. It is the worked example for the trap below.

1. Write `internal/store/schema_0NN.sql`, one higher than the highest that exists.
2. Add the `//go:embed` line and the entry in the `migrations` slice in `internal/store/store.go`.
   The slice order is the migration order and `user_version` counts it.
3. Never edit a migration that has shipped. A database at `user_version N` has already run the old
   text, so an edit changes what new databases get and nothing else, and the two diverge silently.

**Rebuilding a table is not a refactor**, for the reason AGENTS.md gives: with foreign keys on,
`DROP TABLE tasks` cascades a delete through every transcript in the database. Add a column instead,
as `schema_014.sql` does.

Then prove it on a copy of a real database rather than only on a fresh one:

```sh
cp ~/.zerg/zerg.db /tmp/migration-check.db
sqlite3 /tmp/migration-check.db 'pragma user_version; select count(*) from messages, events;'
go run ./cmd/zerg up --db /tmp/migration-check.db --addr 127.0.0.1:7999 --no-tls   # then stop it
sqlite3 /tmp/migration-check.db 'pragma user_version; select count(*) from messages, events;'
```

The version must go up by one and the counts must not move. Report both readings.
