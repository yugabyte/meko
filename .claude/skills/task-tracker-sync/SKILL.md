---
name: task-tracker-sync
description: >-
  Before staging any change to a file under `tasks/` (new task file,
  status flip, frontmatter edit, body rewrite), verify the file's
  frontmatter is well-formed, that `tasks/README.md`'s index row
  matches what the frontmatter says, and that the body reflects the
  final landed state — not the planning state, not the
  "almost-done-with-leftovers" state. Also fires when creating a new
  task file or marking one done. Same commit, no follow-ups.
---

# task-tracker-sync

`tasks/` is this repo's lightweight Jira. The contract is simple but
easy to violate by accident:

- Every task file has frontmatter that says what state it's in.
- `tasks/README.md` has an index table that mirrors the frontmatter.
- The task body describes what *actually landed*, with commit
  anchors — not the original plan, not what's still pending.

When those three drift apart, future-you (and the next agent that
opens the repo) can't tell what's done. This skill keeps them in sync.

## When to fire

- You're editing the frontmatter of a `tasks/*.md` file (any field).
- You're creating a new `tasks/<id>_*.md` file.
- You're marking a task done (`status: pending|in_progress` → `done`).
- You're closing the loop on a task in conversation ("OK, that's
  task 9 done") even if you haven't touched the file yet — touch it
  now, don't defer.

## Step 1 — frontmatter sanity

Open the affected file. Confirm:

```yaml
---
status: pending|in_progress|blocked|done|deferred
severity: highest|high|medium|low
area: yb-pageserver|safekeeper|storage_ctl|page_service / compute|yb_light_local|...
opened: YYYY-MM-DD
closed: YYYY-MM-DD     # only when status=done or deferred
---
```

- `area:` is one of the values listed in `tasks/README.md` "Areas",
  or a `/`-joined pair for multi-area tasks. New areas need a README
  update.
- `closed:` is present iff `status` is `done` or `deferred`. If you
  set `status: done` you MUST set `closed:` to today's date in the
  current-day format (e.g. 2026-06-08). Don't leave it stamped to
  whenever the work actually finished if you don't know the exact
  date — today is fine.
- Dates are absolute. Convert any "Thursday" / "last week" in user
  messages to the actual ISO date before persisting.

## Step 2 — README index row

Open `tasks/README.md`. Find the row for this task's `<id>`.

- If no row exists: add one. Columns are `ID | Title | Status | Severity | Area`.
- Status emoji: `✅ done` · `🚧 in_progress` · `⏳ pending` · `🚫 blocked` · `💤 deferred`.
- Severity / area cells must match the frontmatter.
- Title is concise; use backticks for code/file references.

Rows must be in `<id>` order (the natural sort: `4.a` before `4.a.1`
before `4.b`).

If you renamed a task or changed its severity, the README row must
follow.

## Step 3 — body matches reality

This is the step that gets skipped and the one that matters most.

A task body usually has two layers:

- **Plan / sketch** — what you intended to do when the file was
  opened. This dates fast.
- **Landed** — what actually shipped, with commit hashes.

When you mark a task done:

- The top of the body must clearly say "done" (a `## Status: DONE`
  header, or equivalent) with the landing-commit anchor(s).
- "Wave 1 leftovers / not blocking" or "Pending follow-ups" sections
  must be revisited:
  - If those leftovers landed: replace with a one-line note citing
    the landing commit.
  - If they were genuinely deferred: keep them, with a pointer to
    the follow-up task (if any) or a note saying why deferred.
- Old planning prose can stay below a `---` separator as historical
  context. Don't delete it unless it's actively misleading; just
  don't lead with it.

A task body that still leads with "Plan: do X, Y, Z" after the work
landed is a lie by omission. Fix it.

## Step 4 — same commit

The frontmatter flip, the README index row, and the body refresh all
travel in one commit. Don't ship `status: done` in one commit and
the README update in another — the gap is when search/agent context
sees inconsistent state.

Naming: if the task is the primary subject of the commit, the
subject line can name it ("close task 9"). If the task closeout is a
side-effect of bigger work, fold it into that commit's message body.

## When NOT to fire

- Editing a task's body to clarify wording or fix typos without
  changing status — README row doesn't need re-checking, only
  frontmatter↔body consistency.
- Reading a task file as research — read-only, no sync needed.
- Adding a new section to a still-pending task's body (e.g. updating
  a hypothesis as you learn more) — only fire if frontmatter changes
  or the body starts contradicting the README row.

## Anti-patterns

- **"I'll update the README in a moment"** — no. The README index
  is the load-bearing surface; it's how anyone sees the task list at
  a glance. Same commit.
- **Marking done without setting `closed:`** — the closed-date is
  how you know which work shipped when, which is the only thing the
  index is good for as a historical record.
- **Renumbering**: don't reuse `<id>`s when a file is deleted. The
  README convention is explicit about this.
- **Phantom progress** — appending "Wave 2 — pending" to a body that
  also says "## Status: DONE". Pick one.

## Failure modes this prevents

Real examples from this repo:

1. Tasks 19, 20, 21 (all safekeeper, all `status: done` since
   2026-06-05) shipped without their README index entries — invisible
   in the table until someone explicitly went looking. Caught only
   because a human asked "what's left?" and noticed the gap.

2. Task 9 (`9_safekeeper-paxos-removal.md`) had `status: done` in
   frontmatter but a body that led with "Wave 1 leftovers (not
   blocking; cleanup-only)" listing three items as TODO. All three
   had actually landed weeks earlier. The body lied for as long as
   it took someone to read it carefully and compare to git log.

This skill exists so future task closures don't repeat either.
