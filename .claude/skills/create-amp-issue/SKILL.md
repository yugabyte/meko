---
name: create-amp-issue
description: >-
  Before creating or updating a GitHub issue for **AMP** work in this repo (new
  issue, label/milestone change, closing one out), verify it has an AMP area
  label AND an AMP milestone, that the title/body reflect the final landed
  state — not the planning state, not the "almost-done-with-leftovers" state —
  and that issues whose work has shipped get closed, with the landing commit/PR
  referenced. Fires when opening a new AMP issue, editing one, or declaring a
  tracked piece of AMP work done.
---

# create-amp-issue

**Scope: AMP work.** AMP is tracked as **GitHub issues in this repository**,
driven via the `gh` CLI. This skill governs those issues — it makes no claim
about how meko's own work is tracked.

The contract is simple but easy to violate by accident:

- Every issue has an **AMP area label** and an **AMP milestone**.
- The title/body describe what *actually landed*, with commit/PR anchors — not
  the original plan, not what's still pending.
- When the work ships, the issue is **closed**, referencing the landing commit
  or PR.

When those drift, nobody can tell from the issue list what's done or where it
belongs. This skill keeps them in sync.

## When to fire

- You're creating a new AMP issue (`gh issue create`).
- You're editing an AMP issue's labels, milestone, title, or body
  (`gh issue edit`).
- You're marking AMP work done — closing an issue, or saying in conversation
  "OK, that's #34 done" even before you've touched it. Close it now, don't
  defer.

## Step 1 — area label + milestone present

Every AMP issue MUST carry:

- **An AMP area label.** These four are the only allowed AMP area labels:
  - `amp_compute` — Postgres, extensions including the `yb_amp` extension,
    `compute_ctl`, pageserver, safekeeper, read/write paths, page-service wire
    protocol.
  - `amp_controller` — controller orchestration, `yb_amp_local`,
    endpoint/branch lifecycle.
  - `amp_voyager` — Voyager migration integration.
  - `amp_perf_advisor` — performance advisor.

  Stock labels (`bug`, `enhancement`, …) are optional extras, not a substitute
  for the area label. Don't reach for a meko label on an AMP issue, or vice
  versa.

- **An AMP milestone.** All are `AMP: `-prefixed, which is what separates them
  from meko's. Check what actually exists rather than trusting a list here:

  ```bash
  gh api repos/:owner/:repo/milestones --jq '.[] | .title'
  ```

On create, pass both:

```bash
gh issue create --title "..." --body "..." \
  --label amp_compute --milestone "AMP: K8 Deploy"
```

On an existing issue missing either, fix it:

```bash
gh issue edit <num> --add-label amp_controller --milestone "AMP: Early Access"
```

A new area label or milestone needs agreement first — don't invent names. Check
what exists with `gh label list`.

The area label is enforced post-hoc by
`.github/workflows/amp-require-area-label.yml`: it applies `needs-area-label` plus
a comment to any issue lacking one of the four, and clears the flag automatically
once a real area label is added. Getting it right at create time avoids the round
trip.

## Step 2 — title + body match reality

This is the step that gets skipped and the one that matters most.

- **Title** is a concise, imperative summary; backticks for code/file
  references. If the work's scope changed, retitle.
- **Body** usually has two layers:
  - **Plan / sketch** — what you intended when the issue opened. Dates fast.
  - **Landed** — what actually shipped, with commit hashes / PR refs.

When you close an issue:

- The body (or a closing comment) must say what landed, with the landing-commit
  anchor(s) or `Closes #<num>` PR reference.
- "Leftovers / not blocking" or "Pending follow-ups" notes must be revisited:
  - If they landed: note it with the landing commit.
  - If genuinely deferred: split them into a **new issue** and link it
    (`#<num>`), rather than closing with silent loose ends.

An issue body that still leads with "Plan: do X, Y, Z" after the work landed is
a lie by omission. Edit it (`gh issue edit <num> --body ...`) or leave a closing
comment that supersedes it.

## Step 3 — close when it ships

When the work lands, close the issue as part of the same flow that ships it —
ideally via `Closes #<num>` in the PR/commit, or explicitly:

```bash
gh issue close <num> --comment "Landed in <commit/PR>. <one-line what shipped>."
```

Don't leave shipped work open "to close later" — the gap is exactly when the
issue list misleads.

## Cross-referencing commits

An AMP commit references its issue with a leading `[#<num>]` tag; the AMP
subject format is `[#<issue>] <area>: <sub-component>: <title>`, where `<area>`
is a subsystem (`compute` / `control_plane`) or a meta token (`pm` / `docs` /
`ci` / `build` / `tools`). Example:

```
[#42] compute: pageserver: strip multi-timeline machinery
```

This lets `git log --oneline` be cross-referenced against the issue list, and
GitHub auto-links the commit on the issue. Use `Closes #<num>` / `Fixes #<num>`
in the PR body or final commit to auto-close on merge.

Note the two tag forms this repo accepts — `[MEKO-<num>]` for meko work,
`[#<issue>]` for AMP work; see the `commit` skill. Don't invent a number: if there's no issue, either file one or omit the
tag entirely.

## When NOT to fire

- Working on **meko** rather than AMP. This skill is AMP-scoped; it has no
  opinion on meko's tracker.
- Reading an issue as research — read-only, no sync needed.
- Adding a clarifying comment to a still-open issue without changing its
  status — fine, no label/milestone re-check needed.
- Appending a new hypothesis to an open investigation — only fire if you're
  changing status, labels, milestone, or the body now contradicts itself.

## Anti-patterns

- **Creating an issue with no area label or no milestone.** Both are required —
  an unlabelled, unmilestoned issue is invisible to every filtered view the team
  uses.
- **Using a meko label on an AMP issue.** The `amp_*` labels are what make AMP
  work filterable now that both projects share one tracker.
- **Closing without a reference.** "Closed" with no commit/PR anchor loses the
  one thing the tracker is good for as a historical record: what shipped when.
- **Phantom progress.** A body that says "DONE" up top and "Wave 2 — pending"
  below. Pick one; split the pending part into its own issue.
- **Inventing labels/milestones** on the fly instead of using the four `amp_*`
  areas and the milestones that actually exist.
