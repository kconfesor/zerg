---
description: Run everything CI runs, in the order that fails cheapest first
allowed-tools: Bash(gofmt:*), Bash(go:*), Bash(pnpm:*), Bash(./build.sh:*), Read, Grep
---

Run the checks below in this order and report what happened. Stop at the first failure, fix it if
the fix is obvious and small, then start again from the top.

```sh
gofmt -l . | grep -v '^web/'          # must print nothing
go vet ./...
go test ./...
pnpm --dir web lint
pnpm --dir web exec vue-tsc --noEmit  # templates too, not just script blocks
pnpm --dir web test
```

Two things worth knowing before you report a result:

- A test that fails only sometimes is not automatically a flake. `TestFatalErrorStopsSupervision`
  looked like one and was a real race that could make the supervisor respawn into a fatal error.
  Before calling anything flaky, try `-count 20` and `-cpu 1`, which is what made that one
  deterministic.
- Three tests in `internal/api` skip when the cockpit is not built. That is expected in a checkout
  that has never run `./build.sh`, and CI runs them for real in its own job. A skip is not a pass to
  report as coverage.

Report the actual output of anything that failed, not a summary of it.
