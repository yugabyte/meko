# Issue tracking

AMP work is tracked in **Jira, project
[`AMP`](https://yugabyte.atlassian.net/browse/AMP)**. Every ticket is filed **under
an epic**. **Don't open GitHub issues for AMP work.** The ones that existed were
moved to Jira (label `from-github`) and closed as duplicates of their tickets.

To pick an epic, query the live list rather than a copy written down anywhere:

```
project = AMP AND issuetype = Epic AND statusCategory != Done
```

How to file, update and close a ticket is in the `create-amp-issue` skill, which
is authoritative on that.

## Commit subjects

```
[AMP-<num>] <area>: <sub-component>: <title>
```

`<area>` is either a **subsystem** or a **meta token** for work that touches no
subsystem:

| Token | Covers |
|---|---|
| `compute` | Postgres, extensions including `yb_amp`, `compute_ctl`, pageserver, safekeeper, the page store, WAL durability, the read/write paths, the integration harness. |
| `control_plane` | `controller` orchestration, `yb_amp_local`, endpoint and branch lifecycle. |
| `voyager` | Voyager migration integration. |
| `perf_advisor` | Performance advisor. |
| `pm`, `docs`, `ci`, `build`, `tools` | Meta tokens, for commits that touch no subsystem. |

The tag is required (the `PR title` check enforces it), several go back to back
(`[AMP-55][CLOUDGA-37256]`), and any Jira project's key works: use the ticket that
tracks the work. `<sub-component>` is optional. See the `commit` skill's "Subject
format" for the full spec.

Close a ticket with `Closes https://yugabyte.atlassian.net/browse/AMP-55` in the
PR body. On merge,
[`.github/workflows/jira-on-merge.yml`](../../.github/workflows/jira-on-merge.yml)
moves it to Done and comments the PR link.

A `[#N]` tag on an older commit refers to a GitHub issue. Commits from before the
monorepo merge use **amp's old numbering**: see the
[migration mapping](https://github.com/yugabyte/meko/issues/393#issuecomment-5139709911)
and the note in the root [`AGENTS.md`](../../AGENTS.md).
