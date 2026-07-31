---
name: create-issue
description: >-
  Before creating or updating a GitHub issue on yugabyte/amp (new
  issue, status/label/milestone change, closing one out), verify it has
  an area label AND a milestone, that the title/body reflect the final
  landed state — not the planning state, not the "almost-done-with-leftovers"
  state — and that issues whose work has shipped get closed (with the
  landing commit/PR referenced). Fires when opening a new issue, editing
  one, or declaring a tracked piece of work done.
---

# create-issue

This repo's tracker is **GitHub issues** in this repository, driven
via the `gh` CLI. (The old `tasks/*.md` files were migrated to issues
and deleted; `tasks/README.md` is now just a pointer.) The contract is
simple but easy to violate by accident:

- Every issue has an **area label** and a **milestone**.
- The title/body describe what *actually landed*, with commit/PR
  anchors — not the original plan, not what's still pending.
- When the work ships, the issue is **closed**, referencing the
  landing commit or PR.

When those drift, nobody can tell from the issue list what's done or
where it belongs. This skill keeps them in sync.

## When to fire

- You're creating a new issue (`gh issue create`).
- You're editing an issue's labels, milestone, title, or body
  (`gh issue edit`).
- You're marking work done — closing an issue, or saying in
  conversation "OK, that's #34 done" even before you've touched it.
  Close it now, don't defer.

## Step 1 — label + milestone present

The authoritative area/milestone taxonomy and contract live in
[`docs/issue-tracking.md`](../../../docs/issue-tracking.md); the summary
below must stay consistent with it. Every issue MUST carry:

- **An area label**, one (or both) of:
  - `compute` — Postgres, extensions including the `yb_amp` extension, `compute_ctl`,
    pageserver, safekeeper,
    read/write paths, page-service wire protocol.
  - `control plane` — `controller` orchestration,
    `yb_amp_local`, endpoint/branch lifecycle.
  Stock labels (`bug`, `enhancement`, …) are optional extras, not a
  substitute for the area label.
- **A milestone**, one of: `POC` · `Early Access` · `K8 Deploy`.

On create, pass both:

```
gh issue create --title "..." --body "..." \
  --label compute --milestone POC
```

On an existing issue missing either, fix it:

```
gh issue edit <num> --add-label "control plane" --milestone "Early Access"
```

A new area or milestone needs agreement first — don't invent label
names. Check what exists with `gh label list` /
`gh api repos/:owner/:repo/milestones`.

## Step 2 — title + body match reality

This is the step that gets skipped and the one that matters most.

- **Title** is a concise, imperative summary; backticks for
  code/file references. If the work's scope changed, retitle.
- **Body** usually has two layers:
  - **Plan / sketch** — what you intended when the issue opened.
    Dates fast.
  - **Landed** — what actually shipped, with commit hashes / PR refs.

When you close an issue:

- The body (or a closing comment) must say what landed, with the
  landing-commit anchor(s) or `Closes #<num>` PR reference.
- "Leftovers / not blocking" or "Pending follow-ups" notes must be
  revisited:
  - If they landed: note it with the landing commit.
  - If genuinely deferred: split them into a **new issue** and link
    it (`#<num>`), rather than closing with silent loose ends.

An issue body that still leads with "Plan: do X, Y, Z" after the work
landed is a lie by omission. Edit it (`gh issue edit <num> --body ...`)
or leave a closing comment that supersedes it.

## Step 3 — close when it ships

When the work lands, close the issue as part of the same flow that
ships it — ideally via `Closes #<num>` in the PR/commit, or
explicitly:

```
gh issue close <num> --comment "Landed in <commit/PR>. <one-line what shipped>."
```

Don't leave shipped work open "to close later" — the gap is exactly
when the issue list misleads. See the `commit` skill, which
reminds you to reference and close issues at commit time.

## Cross-referencing commits

When a commit is work on an issue, lead the subject with the `[#<num>]`
tag — the full format is `[#<issue>] <area>: <sub-component>: <title>`,
where `<area>` is a subsystem (`compute` / `control_plane`) or a meta
token (`pm` / `docs` / `ci` / `build` / `tools`); see the `commit`
skill's "Subject format". Example:
`[#42] compute: pageserver: strip multi-timeline machinery`. This lets
`git log --oneline` be cross-referenced against the issue list, and
GitHub auto-links the commit on the issue. Use `Closes #<num>` /
`Fixes #<num>` in the PR body or final commit to auto-close on merge.

## When NOT to fire

- Reading an issue as research — read-only, no sync needed.
- Adding a clarifying comment to a still-open issue without changing
  its status — fine, no label/milestone re-check needed.
- Appending a new hypothesis to an open investigation — only fire if
  you're changing status, labels, milestone, or the body now
  contradicts itself.

## Anti-patterns

- **Creating an issue with no label or no milestone.** Both are
  required — an unlabelled, unmilestoned issue is invisible to every
  filtered view the team uses.
- **Closing without a reference.** "Closed" with no commit/PR anchor
  loses the one thing the tracker is good for as a historical record:
  what shipped when.
- **Phantom progress.** A body that says "DONE" up top and "Wave 2 —
  pending" below. Pick one; split the pending part into its own issue.
- **Inventing labels/milestones** on the fly instead of using the
  agreed `compute` / `control plane` and `POC` / `Early Access` /
  `K8 Deploy`.

## Failure modes this prevents

From this repo's file-tracker era (same failure modes apply to
issues):

1. The safekeeper trio (now #10 / #14 / #15) shipped but never got a
   tracker index entry — invisible until a human asked "what's left?".
   The issue equivalent: closing without a label/milestone, so it never
   shows in a filtered view.

2. The Paxos-removal work (now #56) was marked done but its write-up
   still led with "Wave 1 leftovers (not blocking)", listing three
   already-landed items. The record lied until someone read it
   carefully. The issue equivalent: closing without editing the stale
   plan out of the body.

This skill exists so issue closures don't repeat either.
