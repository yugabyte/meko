---
name: e2e-before-corruption-claim
description: >-
  Before declaring a corruption / WAL-loss / data-loss / page-write /
  flush / generation-fence fix complete (in a commit message, PR
  description, conversation reply, or task closeout), run the e2e
  suite locally — at minimum `test/e2e/storage/run.sh fast`, and `slow` for
  changes that touch sustained-write or restart paths. Unit tests
  and `cargo build` are not sufficient evidence. If you can't run
  e2e for any reason, say so explicitly rather than claiming the
  fix works.
---

# e2e-before-corruption-claim

Unit tests are great at catching mechanical mistakes. They are
useless for catching the bug class that motivates this skill:
**corruption that only manifests across the seams** — between the
walreceiver and the page writer; between the flush loop and the
generation fence; between two safekeeper restarts; between
`disk_consistent_lsn` and `applied_lsn`; between what YB durably
holds and what the in-process buffer believes.

Those seams aren't exercised in any unit test. They're only
exercised by a real workload running end-to-end against a real
yb_light_local + real YB. So before you claim a fix in that bug class
works, you must run it end-to-end.

## When to fire

Fire when you're about to:

- Mark a task with `area: yb-pageserver` or `safekeeper` as
  `status: done` if the change touched any of: WAL ingest, WAL
  storage, flush loop, page writer, generation fence, restart /
  catch-up path, walreceiver, walsender, `consistent_lsn` or
  `applied_lsn` advancement, durability watermarks.
- Write a commit message that contains any of: "fix corruption",
  "fix WAL loss", "data loss", "restart bug", "lost record", "stale
  page", "generation fence", "flush race", "disk vs durable", "off
  by one LSN".
- Write a PR description claiming a fix for any of the above.
- Tell the user in conversation that any of the above is "fixed"
  / "should be fixed" / "works now."

This skill does NOT fire for:

- Pure refactors (no behavioral change).
- Bug fixes outside the durability path (e.g. CLI parsing, log
  formatting, metric labels, config-load).
- Doc-only changes.
- Changes that already trigger a CI-equivalent run that the user
  can see (they'd have already vetoed if it failed).

When in doubt: fire. The cost of an unnecessary e2e run is 60–300
seconds. The cost of shipping a silent corruption claim that
re-fires the bug under a customer workload is much larger.

## What "ran e2e" actually means

The bar is: a full yb_light_local cluster came up, executed a workload
that crossed the seams the fix is meant to harden, and reported
zero corruption markers on shutdown.

The repo's entry point is `test/e2e/storage/run.sh`. It already does:

- Brings up a single yb_light_local cluster against an ephemeral YB.
- Iterates the requested category (`fast` / `slow` / `all`).
- Scans the pageserver log for corruption markers after every test.
  First hit aborts the run.

Use it. Don't roll your own.

```
# minimum bar for any fix in the bug class above
test/e2e/storage/run.sh fast

# required for changes touching sustained-write, restart, or
# replay paths
test/e2e/storage/run.sh all
```

If the change is specifically about a behavior covered by one of
the scenarios in the Rust harness (`test/e2e/storage/`) or a
`.sql` workload under `test/e2e/storage/{fast,slow}/`, that one scenario may
be sufficient — but run the whole suite anyway unless turnaround time
is genuinely prohibitive. The whole point is to catch the bug you
didn't anticipate.

Memory anchor: the user's standing instruction in
`run-e2e-before-claiming-fix.md` says "manual end-to-end workload
locally before claiming a corruption fix works; unit tests aren't
enough." This skill is the operational form of that memory.

## How to wire it into a turn

1. Identify whether the fix is in the trigger set above. If yes:
2. Run `test/e2e/storage/run.sh fast` (or `all` if appropriate). Capture
   exit code and the last ~30 lines of output.
3. **If it passes**: include a one-line evidence note in the
   commit body / PR description / reply: "e2e fast passes (no
   corruption markers; ran 2026-06-08)." Don't dump the whole
   output — a single positive sentence is enough.
4. **If it fails**: do not claim the fix. Either keep iterating or
   report the failure honestly. A failing e2e is the signal you
   were looking for; don't paper over it.
5. **If you cannot run e2e** (sandbox restrictions, missing YB
   cluster, etc.): say so explicitly. The exact phrasing matters
   — "I haven't been able to run e2e locally because <reason>"
   rather than just omitting the fact. Per the project's
   `CLAUDE.md`: "if you can't test the UI, say so explicitly rather
   than claiming success." Same principle here.

## Anti-patterns

- **"cargo test --workspace passes"** — that's table stakes, not
  evidence. The corruption bugs in this codebase have, repeatedly,
  passed unit tests and failed e2e. Examples on file: the
  PS-restart `disk_consistent_lsn`-from-eager-frontier bug, the
  walproposer commit_lsn divergence, the flush-race that lost
  records buffered between `put_batch` and the next `advance_*`.
  All passed unit tests.
- **"It compiles, ship it."** — never sufficient for this bug class.
- **"e2e is slow, I'll run it in a follow-up."** — the follow-up
  doesn't happen. Same commit.
- **Skipping e2e because the diff looks "obviously correct"** —
  the bug class this skill targets is by definition the bug that
  doesn't look like a bug. Don't trust your read of the diff alone.
- **Running only the script that matches the fix** — the goal is
  to catch the regression you didn't predict. Run the suite.
- **Claiming "e2e passed" without actually running it** — don't.
  Just say you didn't run it.

## Failure mode this prevents

The 2026-06 PS-restart WAL replay bug — the eager
`applied_lsn` was wired into `disk_consistent_lsn` on the
`PageserverFeedback` wire, so the safekeeper truncated WAL the
pageserver had buffered-but-not-flushed. A PS crash-restart between
the eager bump and the durable flush lost those records
permanently. Unit tests passed throughout the regression. It was
caught only by an e2e restart workload — exactly the kind of run
this skill exists to enforce.

Don't be the next entry on that list.
