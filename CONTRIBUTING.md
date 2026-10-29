# Contributing

This repo is agent-first: its docs are written for coding agents as much as for people, and the
easiest way to change it is with one.

## Use AgentK

The best way to contribute is with [AgentK](extensions/agentk/README.md), the agentic coder in this
repo:

1. [Install Meko](README.md#install), then AgentK: `meko extension install agentk`.
2. In AgentK, open a project on your clone of this repo.
3. Start a session with what you want changed. AgentK takes it from design to a reviewed, merged
   PR, following the repo's [AGENTS.md](AGENTS.md), and keeps what each session learns in the
   project's Meko datapack for the next one.

## Or any coding agent

Point Claude Code, Codex, OpenCode, Kiro, Cursor or another agent at the repo and say what you
want. It starts from [AGENTS.md](AGENTS.md), which maps the repo and leads to each part's own guide
with its build, test and lint commands: [backend/AGENTS.md](backend/AGENTS.md) for Meko,
[extensions/agentk/AGENTS.md](extensions/agentk/AGENTS.md) for AgentK and
[extensions/presenter/AGENTS.md](extensions/presenter/AGENTS.md) for Presenter.

## The rules

- Changes reach `main` only through PRs. CI runs the jobs of the parts a PR changes, and PRs are
  squash-merged once the `CI gate` and `PR title` checks pass.
- PR titles are `<type>(<component>[/<area>])[!]: <summary>`, such as `fix(agentk/web): …`. Check
  one with `sh scripts/check-title.sh "<title>"`; the types and components are in the
  [commit skill](.claude/skills/commit/SKILL.md).
- Docs change in the same PR as the code they describe.
- File issues through GitHub's issue forms (bug, feature or task).
- Treat the repo as public: no internal hostnames, paths, tokens or keys.

Questions and ideas: [Meko's Discord](https://mekodata.ai/discord/) or
[Slack](https://inviter.co/yugabytedb).
