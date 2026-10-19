---
name: create-amp-issue
description: >-
  Before filing, updating or closing out tracked **AMP** work. AMP work is
  tracked as Jira tickets in the AMP project, each under an epic, and never as
  a new GitHub issue. Verify the ticket has an epic parent, that its summary
  and description reflect what actually landed rather than the plan, and that
  shipped work gets closed with the landing PR referenced. Fires when filing
  AMP work, changing a ticket's scope, or declaring a tracked piece of AMP work
  done.
---

# create-amp-issue

**Scope: AMP work.** AMP is tracked in **Jira, project `AMP`**
(https://yugabyte.atlassian.net/browse/AMP). **Do not open GitHub issues for AMP
work.** The AMP GitHub issues that existed were moved to Jira (label
`from-github`) and closed as duplicates of their tickets. This skill makes no
claim about how meko's own work is tracked.

The contract:

- Every ticket is filed **under an epic**.
- The summary and description describe what *actually landed*, with PR anchors,
  not the original plan and not what's still pending.
- When the work ships, the ticket is **closed**, referencing the landing PR.

## When to fire

- You're filing AMP work: a bug, a follow-up, a product defect a test found.
- You're changing a ticket's scope, summary or description.
- You're marking AMP work done, including saying in conversation "OK, that's
  AMP-34 done". Close it now; don't defer it.

## Step 1 — file under an epic

Every AMP ticket has an epic as its parent. Pick the epic that owns the work from
the live list. Epics change over time, so don't trust any list written down
somewhere, this file included:

```
project = AMP AND issuetype = Epic AND statusCategory != Done
```

If no epic fits, ask before creating one. Use `Bug` for a defect and `Task`
otherwise. File it in the Jira UI, or through the Atlassian MCP
(`createJiraIssue` with `parent` set to the epic key).

## Step 2 — summary and description match reality

- **Summary**: a concise, imperative line, with backticks for code and file
  references. If the scope changed, change the summary.
- **Description**: usually two layers. **Plan** is what you intended when you
  filed it, and it dates fast. **Landed** is what actually shipped, with PR links.

When you close a ticket:

- The description or a closing comment says what landed, with the PR link.
- Revisit any "leftovers" or "follow-ups" notes. If they landed, say so with the
  PR. If they're genuinely deferred, file them as **a new ticket** under an epic
  and link it, rather than closing with silent loose ends.

## Step 3 — close it when it ships

Put `Closes https://yugabyte.atlassian.net/browse/AMP-<num>` on its own line in
the PR body. On merge, `.github/workflows/jira-on-merge.yml` moves the ticket to
Done and comments the PR link (see the `open-pr` skill for the exact line
format). Close it by hand when the work shipped some other way.

## Cross-referencing commits

Commit subjects and PR titles lead with the ticket key, `[AMP-<num>]`; see the
`commit` skill for the full subject format. Use only the key of a ticket that
exists. Don't invent a number. If there is no ticket, file one.

## Anti-patterns

- **Opening a GitHub issue for AMP work.** It splits the backlog across two
  trackers, and nobody's filtered view will see it.
- **Filing a ticket with no epic.** Every view the team plans from is per-epic.
- **Closing without a reference.** A ticket closed with no PR link loses the one
  thing the tracker is good for as a record: what shipped, and when.
- **Phantom progress.** A description that says "DONE" up top and "Wave 2 —
  pending" below. Pick one, and file the pending part as its own ticket.
