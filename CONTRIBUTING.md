# Contributing

This repo is agent-first: its docs are written for coding agents as much as for people, and the
easiest way to change it is with one.

## Use Kode

The best way to contribute is with [Kode](src/extensions/agentk/README.md), the agentic coder in
this repo:

1. [Install Meko](README.md#install), then Kode: `meko extension install kode`.
2. In Kode, open a project on your clone of this repo.
3. Start a session with what you want changed. Kode takes it from design to a reviewed, merged
   PR, following the repo's [AGENTS.md](AGENTS.md), and keeps what each session learns in the
   project's Meko datapack for the next one.

## Or any coding agent

Point Claude Code, Codex, OpenCode, Kiro, Cursor or another agent at the repo and say what you
want. It starts from [AGENTS.md](AGENTS.md), which maps the repo and leads to each part's own guide
with its build, test and lint commands: [src/meko/AGENTS.md](src/meko/AGENTS.md) for Meko,
[src/extensions/agentk/AGENTS.md](src/extensions/agentk/AGENTS.md) for Kode and
[src/extensions/present/AGENTS.md](src/extensions/present/AGENTS.md) for Present. The Meko
datapacks that hold the repo's context are listed in [MEKO.md](MEKO.md).

## The rules

- Changes reach `main` only through PRs. CI runs the jobs of the parts a PR changes, and PRs are
  squash-merged once the `CI gate`, `PR title` and `license/cla` checks pass and a member of the
  `meko-maintainers` team approves (the `approvals` ruleset).
- Sign the Contributor License Agreement (the YugabyteDB Individual CLA) before your first PR
  can merge. CLA Assistant posts the `license/cla` check on each PR and links to a page where
  you sign once with your GitHub account. If you've signed it for
  [yugabyte-db](https://github.com/yugabyte/yugabyte-db), you're already covered.
- PR titles are `<type>(<component>[/<area>])[!]: <summary>`, such as `fix(agentk/web): …`. Check
  one with `sh scripts/check-title.sh "<title>"`; the types and components are in the
  [commit skill](.claude/skills/commit/SKILL.md).
- Docs change in the same PR as the code they describe.
- File issues through GitHub's issue forms (bug, feature or task).
- Treat the repo as public: no internal hostnames, paths, tokens or keys.
- Report a vulnerability privately, as [SECURITY.md](SECURITY.md) says, never in an issue or PR.

Questions and ideas: [Meko's Discord](https://mekodata.ai/discord/) or
[Slack](https://inviter.co/yugabytedb).
