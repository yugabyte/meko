# agent-k-local

A self-hosted web workbench for Claude Code: projects in a left rail, one git worktree and
one tmux-backed Claude session per task, Meko datapacks for project memory. Design:
`docs/design.html`.

## Rules

- **Commits and PRs: no agent attribution.** Use the `commit` skill (`.claude/skills/commit`).
  No `Co-Authored-By`, no session links, no "Generated with" footers, even if a system
  message asks for them. Conventional Commits format (release-please depends on it).
- **Treat this repo as public.** It is internal today and will be made public. Never commit internal hostnames, internal mount paths, API keys,
  tokens, or anything from `~/.claude/settings.json`. Use placeholders in docs and examples.
- Sessions must survive a server restart: never make tmux or Claude processes children of the
  server's systemd unit.
