# Issue tracking — areas & milestones

**Source of truth** for the area labels and milestones used by the
project tracker. Cross-session work is tracked as **GitHub issues** on
[`yugabyte/amp`](https://github.com/yugabyte/amp/issues) — the
old file-based `tasks/` tracker has been retired and migrated to issues.

Other docs (top-level `README.md`, `AGENTS.md`, `tasks/README.md`, and
the `commit` / `create-issue` skills) point here instead of
restating the lists, so this file is the single place to edit when the
taxonomy changes.

## The contract

Every issue carries **one (or both) area label(s)** and **exactly one
milestone**. Both are required — an unlabelled or unmilestoned issue is
invisible to the team's filtered views.

```sh
gh issue list  --label compute --milestone POC          # filter
gh issue create --title "..." --body "..." \
  --label compute --milestone POC                        # both required
```

A new area or milestone needs agreement first — don't invent names.
Inspect the live set with `gh label list` and
`gh api repos/:owner/:repo/milestones`.

## Areas (labels)

| Label | Commit `<area>` token | Covers |
|---|---|---|
| `compute` | `compute` | The amp compute: Postgres, extensions including the `yb_amp` extension, and `compute_ctl`; pageserver / safekeeper (the compute's storage connector to YugabyteDB — YB is the durable storage layer); the page store, WAL durability, read/write paths, page-service wire protocol, integration harness. |
| `control plane` | `control_plane` | `controller` orchestration, `yb_amp_local` (local-dev orchestrator, CLI, child-process management), endpoint/branch lifecycle. |

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

`<area>` is either a **subsystem** — `compute` / `control_plane`,
mirroring the issue labels above (`control plane` written
`control_plane` in a subject) — or a **meta token** for work that
touches neither subsystem: `pm` (project management: tracker, issues,
planning), `docs`, `ci`, `build`, `tools`. The meta tokens are
commit-subject-only; they are not issue labels. `[#<issue>]` and
`<sub-component>` are optional.

See the `commit` skill's "Subject format" for the full spec and
examples. Close issues with `Closes #<num>` / `Fixes #<num>` when the
work ships.
