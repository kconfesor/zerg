---
name: add-feature
description: Add a feature to zerg end to end, across the Go daemon and the Vue cockpit. Use when implementing anything that needs both a daemon change and a UI change, when adding an API endpoint, or when a change touches the store, nydus, overmind or the board.
---

# Adding a feature to zerg

**Read [AGENTS.md](../../../AGENTS.md) first.** It holds the rules every agent here works to, and
the ones that have been paid for: append-only migrations, the cascade that a table rebuild triggers,
which failures are 400s, and what a green test run does not prove. Nothing below repeats them.

This skill is the shape of a change that crosses both halves.

## The order

**1. Decide where the truth lives.** State is SQLite; configuration is rows rather than files. If
the feature needs a column, follow `/migration` before writing any SQL.

**2. Make the daemon do it.** AGENTS.md has the package map. Handlers stay thin: decode, call,
choose a status code. Anything an operator can fix comes back as a 400 that names it.

**3. Put it on screen.** `web/src/lib/api.ts` mirrors the daemon's JSON and is the only source of
types. Components are `<script setup lang="ts">` with shadcn-vue pieces from `components/ui`. State
several views need lives in `App.vue` and comes down as props, which is why the readiness report and
its pending flag are there rather than in `Settings.vue`.

**4. Prove it changed something.** Assert against the database, git, or the rendered page, and for
layout read a number back out of a browser rather than reasoning about the CSS.

## Before you say it is done

Run `/verify`, then answer three questions in the pull request: what you observed rather than
inferred, what you left out and why, and whether anything in `docs/ARCHITECTURE.md` is now false.
