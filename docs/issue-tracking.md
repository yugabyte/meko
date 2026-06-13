# Issue tracking — areas & milestones

**Source of truth** for the area labels and milestones used by the
project tracker. Cross-session work is tracked as **GitHub issues** on
[`yugabyte/yb-light`](https://github.com/yugabyte/yb-light/issues) — the
old file-based `tasks/` tracker has been retired and migrated to issues.

Other docs (top-level `README.md`, `AGENTS.md`, `tasks/README.md`, and
the `commit-sync` / `task-tracker-sync` skills) point here instead of
restating the lists, so this file is the single place to edit when the
taxonomy changes.

## The contract

Every issue carries **one (or both) area label(s)** and **exactly one
milestone**. Both are required — an unlabelled or unmilestoned issue is
invisible to the team's filtered views.

```sh
gh issue list  --label storage --milestone POC          # filter
gh issue create --title "..." --body "..." \
  --label storage --milestone POC                        # both required
```

A new area or milestone needs agreement first — don't invent names.
Inspect the live set with `gh label list` and
`gh api repos/:owner/:repo/milestones`.

## Areas (labels)

| Label | Commit `<area>` token | Covers |
|---|---|---|
| `storage` | `storage` | pageserver / safekeeper / storage_controller, the YB-backed page store, WAL durability, read/write paths, page-service wire protocol, e2e harness. |
| `control plane` | `control_plane` | `storage_controller` orchestration, `yb_light_local` (local-dev orchestrator, CLI, child-process management), endpoint/branch lifecycle. |

A cross-cutting issue may carry both labels. The repo's stock labels
(`bug`, `enhancement`, …) are orthogonal and optional — they don't
substitute for an area label.

The commit-subject `<area>` token mirrors the label name, but
`control plane` is written `control_plane` (underscored) in a commit
subject.

## Milestones

Every issue is filed under one of three milestones, roughly the
delivery phases:

| Milestone | Meaning |
|---|---|
| `POC` | Proof-of-concept work; where the migrated tracker tasks landed. |
| `Early Access` | Work required to put it in front of early users. |
| `K8 Deploy` | Kubernetes deployment / productionization. |

## Commit subjects

```
[#<issue>] <area>: <sub-component>: <title>
```

`<area>` is either a **subsystem** — `storage` / `control_plane`,
mirroring the issue labels above (`control plane` written
`control_plane` in a subject) — or a **meta token** for work that
touches neither subsystem: `pm` (project management: tracker, issues,
planning), `docs`, `ci`, `build`, `tools`. The meta tokens are
commit-subject-only; they are not issue labels. `[#<issue>]` and
`<sub-component>` are optional.

See the `commit-sync` skill's "Subject format" for the full spec and
examples. Close issues with `Closes #<num>` / `Fixes #<num>` when the
work ships.
