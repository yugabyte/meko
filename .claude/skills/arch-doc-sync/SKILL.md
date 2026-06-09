---
name: arch-doc-sync
description: >-
  Before staging files for a git commit (or finalizing one) in this repo,
  check whether the diff touches any subsystem covered by an
  `architecture/*.md` doc. If it does, read the relevant doc, decide
  whether the change makes any statement in the doc wrong (sections,
  tables, code-layout lists, CLI/config blocks, mermaid diagrams,
  commit-hash anchors), and update the doc in the **same** commit. Use
  for `git commit`, `git add`-then-commit, `/commit-commands:commit`,
  `/commit-commands:commit-push-pr`, or any flow that ends in a commit.
---

# arch-doc-sync

This repo treats `architecture/*.md` as the canonical description of how
the system is wired today. If a commit changes how the system is wired
and the relevant arch doc still says the old thing, the doc is wrong —
and **wrong docs are worse than absent docs**, because they teach future
readers (and future agents) a false model.

So: every commit that touches a subsystem covered by an arch doc must
keep that doc accurate, in the **same commit**. Not "follow-up", not
"TODO" — same commit. If the change doesn't affect anything the doc
claims, that's fine; you still have to look.

## When to fire

Run this check before you finalize a commit message and call `git
commit`. That means:
- Direct `git commit` invocations.
- The `/commit-commands:commit` and `/commit-commands:commit-push-pr`
  flows.
- Any agent step that ends with "now commit this."

If you've already `git add`-ed and you're about to write the message,
that's the moment.

## Step 1 — figure out what's staged

```
git diff --cached --name-only
```

If the only staged files are themselves under `architecture/`,
`tasks/`, `docs/`, or top-level READMEs, skip — nothing else to keep
in sync.

## Step 2 — map staged files to arch docs

Use this table. A staged path matching the left column means you must
read the doc on the right and decide if it needs updating.

| Code area (staged path matches) | Arch doc(s) to recheck |
|---|---|
| `safekeeper/**`, `libs/safekeeper_api/**`, `libs/walproposer/**` | `architecture/safekeeper.md`, `architecture/safekeeper-lsns.md` |
| `pageserver/**` (excluding `pageserver/pagebench/**`) | `architecture/pageserver.md`, `architecture/pageserver-lsns.md`, `architecture/yb-pageserver.md` |
| `storage_controller/**` | `architecture/storage-controller.md` |
| `libs/remote_storage/**` | `architecture/remote-storage.md` |
| `pgxn/**`, `compute_tools/**`, `compute/**` | `architecture/compute.md` |
| Schema files: `storage_controller/migrations/**/*.sql`, anything creating/altering tables in `yb_storage` or `storage_controller` databases | `architecture/yugabyte.md` + the per-subsystem doc that owns the table |
| Anything that changes a binary's CLI surface (clap `Args` structs in `*/src/bin/*.rs`) | The "Run / configure" block in the matching subsystem doc |
| Top-level topology (new binary, new wire link, removed component) | `architecture/high-level.md` + `architecture/README.md` |

A change can hit more than one doc. Read all of them.

## Step 3 — decide if the doc is now wrong

Skim each candidate doc with the diff open. The doc needs an update if
the change invalidates **any** of these:

- A section heading or "Changes in yb-light" entry (e.g. you deleted
  the module that §N describes, or fully removed a thing the doc
  presents as still-present).
- A code-layout list (`safekeeper/src/foo.rs` — purpose). Module
  deleted/added/renamed → list is wrong.
- A CLI / config example (Run / configure block, TOML snippet, flag
  list). A flag added/removed/renamed makes the example a lie.
- A schema block (`CREATE TABLE ...`, column list, primary key) when
  you altered the live table.
- A mermaid diagram (an arrow, node, or label that no longer
  corresponds to the code).
- A persistence cheat sheet / field table / LSN inventory.
- A claim about a control-flow path ("X calls Y which writes to Z") if
  you changed any node in that path.
- An anchored commit hash, when your commit supersedes that commit's
  behavior (rewrite the section; don't just append).

If you genuinely can't find any statement the doc makes that's now
wrong, fine — record that in your head and move on. But default
toward "needs a touch" — a section header rename, a one-line
clarification, an obsolete commit anchor swapped for the new one.
These are cheap and they're the things that rot.

## Step 4 — update the doc, in the same commit

Edit the doc(s). Do not split into a separate "docs follow-up" commit
— if the code lands and the doc still says the old thing, even for one
commit, search/agent context will pick up the wrong model.

Then re-stage and commit everything together:

```
git add <code files> <architecture/*.md you edited>
git commit -m "..."
```

Naming: the commit subject describes the code change. The arch-doc
update is part of the same change, not a separate concern, so don't
mention it in the subject. (You can call it out in the body if the doc
edit is large or load-bearing.)

## Step 5 — when you genuinely don't need to update

Cases where the right answer is "no doc edit" and you should not force one:

- **Pure bug fix inside an internal function** that the doc describes
  only at the subsystem level. The doc says "X handles Y"; the bug was
  in how X computed Z. The doc remains true.
- **Test-only changes** (`*/tests/**`, `*_test.rs`, fixture data,
  integration scripts under `test_runner/`, `test/e2e/`).
- **Comment / typo / whitespace** in code files.
- **Dependency bumps** (`Cargo.toml`, `Cargo.lock`) unless the bump
  changes documented behavior.
- **Build/CI plumbing** (`.github/**`, `Dockerfile`, `scripts/**`) that
  doesn't change the deployed architecture.

When you skip, you should know **which** of these applies — not just
"feels minor." If you can't categorize the skip, default to checking.

## Anti-patterns

- **"I'll update the doc in a follow-up."** No. The follow-up never
  happens, and the doc lies for the gap. Same commit.
- **Cargo-culting the doc.** Don't add a fresh "Changes in yb-light" §N
  entry for every commit; many commits just edit an existing section.
  Section count is not progress.
- **Updating only the commit-hash anchor.** If you swap `c4c7c1350`
  for `840e56aae` but the prose still describes the old design,
  you've made the doc more wrong, not less.
- **Splitting an arch-doc rewrite into a separate "docs" PR** when the
  underlying code change is on this branch. Doc and code travel
  together.

## Failure mode this prevents

Real example from this repo: §1 of `architecture/safekeeper.md`
described the control file as a 388B bincode blob stored at
`yb_storage.blobs` under `safekeeper-control/<tenant>/<timeline>`,
keyed by a `ControlFileStorage::{File, Yb}` dispatch enum, selected by
`--control-file-backend file|remote-storage`. By the time anyone read
it, none of that was true: task 19 (`840e56aae`) had moved the control
file to typed columns in `safekeeper.timeline_state`, deleted the enum,
deleted `control_file_upgrade.rs`, and removed both CLI flags. The
commit that did the move did not touch the architecture doc — so for
weeks the canonical description of the safekeeper was a fabrication.
This skill exists so that doesn't happen again.
