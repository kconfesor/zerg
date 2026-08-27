---
description: Build the daemon and run it with the cockpit hot-reloading
allowed-tools: Bash(go:*), Bash(pkill:*), Bash(curl:*), Read
---

```sh
go build -o zerg ./cmd/zerg && ./zerg up
```

That is the whole loop. About two seconds for the daemon, and the cockpit hot-reloads from source as
files are saved, because `zerg up` starts its dev server itself when nothing is compiled in.

Do not run `./build.sh` for this. It compiles the cockpit into the binary, which is what you want for
a daemon you will run and not for one you are changing: it costs eleven seconds and the assets it
produces are thrown away on the next edit.

If the pages say the cockpit is not built, the dev server did not start. The daemon's log says why,
and it is almost always node or pnpm missing from PATH.

To check it is serving rather than assuming:

```sh
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7717/          # 200
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:7717/api/projects
```
