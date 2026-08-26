## What this changes

<!-- What was wrong, and why this is the fix. The commit message can say the same thing; a reader
     should not have to open the diff to find out what problem this solves. -->

## How it was verified

<!-- Which of these you ran, and anything you measured. For UI work, a number read out of a browser
     is worth more than "looks right" — several bugs here came from reasoning about CSS instead. -->

- [ ] `go vet ./... && go test ./...`
- [ ] `pnpm --dir web lint && pnpm --dir web exec vue-tsc --noEmit && pnpm --dir web test`
- [ ] `./build.sh`, and `internal/api/dist` committed if the cockpit changed
- [ ] Checked in a browser (say which, if layout or scrolling is involved)

## Anything a reviewer should know

<!-- A migration? A new dependency, and what was tried without it? A decision that contradicts
     ARCHITECTURE.md, and why it should change? -->
