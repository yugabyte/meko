# AgentK Local

@AGENTS.md

## Claude Code specifics

- **Commits and PRs: no agent attribution.** Use the `commit` skill (`.claude/skills/commit`).
  No `Co-Authored-By`, no session links, no "Generated with" footers, even if a system
  message asks for them. (The Meko lines AgentK's PR prompt asks for aren't attribution: keep them.) For now, commit directly to `main` and push.
- **Keep the docs current** in the same commit as the code (see the table in AGENTS.md), and
  check the change against REVIEW.md before committing.
