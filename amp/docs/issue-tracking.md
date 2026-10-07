# Issue tracking — areas & milestones

**Source of truth** for the area labels and milestones used for AMP work.
Cross-session work is tracked as **GitHub issues on
[`yugabyte/meko`](https://github.com/yugabyte/meko/issues)** — not on
`yugabyte/amp`, which has been merged into this repo and holds no open issues.
Old `yugabyte/amp` issue URLs redirect, but the **numbers changed**: see the
[migration mapping](https://github.com/yugabyte/meko/issues/393#issuecomment-5139709911).

Other docs ([`AGENTS.md`](../AGENTS.md), [`README.md`](../README.md)) point here
instead of restating the lists, so this file is the single place to edit when the
taxonomy changes. The mechanics of filing and updating an issue live in the
`create-amp-issue` skill, which is authoritative on that; this file only owns the
vocabulary.

## The contract

Every AMP issue carries **at least one area label** and **exactly one
milestone**. Both are required — an unlabelled or unmilestoned issue is invisible
to the team's filtered views, and
[`.github/workflows/amp-require-area-label.yml`](../../.github/workflows/amp-require-area-label.yml)
enforces the label half: an issue without one gets `needs-area-label` and a bot
comment until it is fixed.

```sh
gh issue list -R yugabyte/meko --label amp_compute --milestone "AMP: Early Access"

gh issue create -R yugabyte/meko --title "..." --body "..." \
  --label amp_compute --milestone "AMP: Early Access"      # both required
```

A new area or milestone needs agreement first — don't invent names. Inspect the
live set with `gh label list -R yugabyte/meko` and
`gh api repos/yugabyte/meko/milestones`.

## Areas (labels)

Labels are **`amp_`-prefixed** because this repo is shared with meko, so an
unprefixed `compute` would be ambiguous.

| Label | Commit `<area>` token | Covers |
|---|---|---|
| `amp_compute` | `compute` | The amp compute: Postgres, extensions including the `yb_amp` extension, and `compute_ctl`; pageserver / safekeeper (the compute's storage connector to YugabyteDB — YB is the durable storage layer); the page store, WAL durability, read/write paths, page-service wire protocol, integration harness. |
| `amp_controller` | `control_plane` | `controller` orchestration, `yb_amp_local` (local-dev orchestrator, CLI, child-process management), endpoint/branch lifecycle. |
| `amp_voyager` | `voyager` | Voyager migration integration — migration schema, domain state, persistence store, connectivity. |
| `amp_perf_advisor` | `perf_advisor` | Performance advisor. |

A cross-cutting issue may carry several. The repo's stock labels (`bug`,
`enhancement`, …) are orthogonal and optional — they don't substitute for an area
label.

**The label name and the commit-subject token differ**, and that is deliberate:
the label carries the `amp_` prefix for cross-project clarity, while the commit
subject is already scoped by its paths. So `amp_controller` is written
`control_plane` in a subject.

## Milestones

Every issue is filed under one of four, roughly the delivery phases. All carry the
**`AMP: ` prefix** for the same reason the labels do:

| Milestone | Meaning |
|---|---|
| `AMP: K8 Deploy` | Kubernetes deployment / productionization. |
| `AMP: Early Access` | Work required to put it in front of early users. |
| `AMP: GA` | General availability. |
| `AMP: Post GA` | Deferred past GA. |

There is no `POC` milestone — it existed in `yugabyte/amp` and did not survive the
merge.

## Commit subjects

```
[<KEY>-<num>] <area>: <sub-component>: <title>    # Jira ticket, e.g. [AMP-55], [CLOUDGA-37256]
[#<issue>] <area>: <sub-component>: <title>       # GitHub issue in this repo
```

`<area>` is either a **subsystem** — `compute` / `control_plane` / `voyager` /
`perf_advisor`, mirroring the labels above — or a **meta token** for work that
touches no subsystem: `pm`, `docs`, `ci`, `build`, `tools`. The meta tokens are
commit-subject-only; they are not issue labels. The tag is optional, and any Jira
project's key works: use the ticket that tracks the work. `<sub-component>` is optional.

See the `commit` skill's "Subject format" for the full spec. Close issues with
`Closes <full issue URL>` when the work ships — **use a full URL, not a bare
`#<num>`**: AMP's issues were renumbered by the migration, so a bare number can
silently resolve to an unrelated meko item. The same line closes a Jira ticket
(`Closes https://yugabyte.atlassian.net/browse/AMP-55`): on merge,
[`.github/workflows/jira-on-merge.yml`](../../.github/workflows/jira-on-merge.yml)
moves it to Done and comments the PR link.
