# todo/

Lightweight tracker for cross-session work. One Markdown file per
issue or task. Treat each file as a Jira ticket: pick a stable
filename slug, write the body free-form, close by deleting (or moving
to `todo/done/`) once the work lands.

## Conventions

- Filename: `<id>_<short-kebab-case-slug>.md`, where `<id>` is a
  monotonically-increasing integer (`1_phase-4k-storcon-startup-wedge.md`).
  Use the next free integer for new entries — don't reuse IDs even
  when the underlying file is deleted, so references stay stable.
- Suggested frontmatter at the top of each file (keep it brief):
  ```
  ---
  status: open       # open | in_progress | blocked | done
  severity: high     # high | medium | low
  area: storcon      # tag the subsystem
  opened: 2026-06-02
  ---
  ```
- Sections to include when relevant: **Symptom**, **Root cause /
  hypothesis**, **Repro**, **Fix sketch**, **Notes / links** (commits,
  log lines, PR refs).
- Cross-link to other files with `[see phase-4k-…](phase-4k-…)`.
- One issue per file, even if they're tightly related; correlations go
  in the **Notes** sections.

## Index

Browse the dir; that's the index. If the list grows past ~20, add an
`index.md` here with one-line summaries.
