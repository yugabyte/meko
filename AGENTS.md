# AGENTS.md

Guide for coding agents (Claude Code, OpenCode, Kiro, Codex, Cursor, and others) working in this repo.

This repo is becoming **meko**: Meko, with extensions that plug into it. Today it holds
two extensions: **AgentK**, in `extensions/agentk`, and **Presenter**, in `extensions/presenter`.
Before changing anything in one, read its AGENTS.md ([AgentK's](extensions/agentk/AGENTS.md),
[Presenter's](extensions/presenter/AGENTS.md)): it has the map, the invariants, the commands (run
from the extension's folder) and which docs to keep current. Meko's own Go backend is in
`backend/`; read [backend/AGENTS.md](backend/AGENTS.md) before changing it.

## Layout

| Path | What it is |
|---|---|
| `backend/` | Meko's Go backend: the `meko` CLI and server, a Go module of its own with its `Makefile`, docs and CI job (`backend` in `.github/workflows/ci.yml`), and its Node memory engine in `backend/memory_engine`. |
| `extensions/agentk/` | AgentK: its code, tests, scripts, packaging and docs, with its own `package.json`. |
| `extensions/presenter/` | Presenter: interactive, branching presentations built from datapacks. Its server, its pages (served by Meko at `/ext/presenter/`), tests and docs, with its own `package.json`. Releases on its own train, whose tarballs Meko installs and updates. |
| `scripts/check-title.sh` | The commit subject and PR title rules for the whole repo. |
| `scripts/ci-scope.sh` | Which components' CI jobs a PR runs, from the paths it changes. |
| `scripts/release/` | The release scripts the three products share, with `--product agentk\|meko\|presenter`: the next version, the release notes (and whether to release), and pruning old releases; and `fetch-node.sh`, which downloads, checks and unpacks the Node a release bundles, with retries. |
| `.github/` | CI (`ci.yml`: a job per component, in `extensions/agentk`, `extensions/presenter` or `backend`, run only when a PR changes it, and the `CI gate` over them), the PR title check, the three release trains (`agentk-release.yml` for AgentK, tags `agentk-v…`; `meko-release.yml` for Meko, tags `meko-v…`, the repo's latest release; `presenter-release.yml` for Presenter, tags `presenter-v…`), Dependabot, and the issue forms (`ISSUE_TEMPLATE/`: bug, feature and task; blank issues are off). |
| `.claude/skills/commit/` | The commit skill: how to write commits, PR titles and PR descriptions here. |
| `.claude/skills/issue/` | The issue skill: how to write, file and edit GitHub issues here, with the forms' sections, types and labels. |
| `README.md`, `LICENSE` | The repo's README (Meko: what it is, building it, its commands, and its extensions) and the license for everything in it. AgentK's README is `extensions/agentk/README.md`. |

## Rules for the whole repo

- **Commits and PR titles** are `<type>(<component>[/<area>])[!]: <summary>` with one of nine
  types and one of the repo's components: `agentk` (`extensions/agentk`), `meko` (Meko, in
  `backend/`), `presenter` (`extensions/presenter`) or `repo` (the shared root), e.g.
  `fix(agentk/web): …`. A change that ships in more than one names each, comma-separated, and
  releases each product it names: `feat(agentk,meko): …`. They're
  checked by `sh scripts/check-title.sh "<title>"` and the `PR title` workflow. Follow the commit
  skill (`.claude/skills/commit/SKILL.md`). PRs are squash-merged, so the PR title is the subject.
- **Changes reach `main` only through PRs** that pass the `CI gate` and `PR title` checks. A PR
  runs only the jobs of the components it changes (`scripts/ci-scope.sh`); a push to `main` runs
  them all. A new component gets its own job in `ci.yml`, its paths in `scripts/ci-scope.sh`, a
  place in the gate's `needs`, and its name in `COMPONENTS` in `scripts/check-title.sh`. A new
  product also gets a GitHub label of its own name and an entry in the issue forms' Product
  dropdown (`.github/ISSUE_TEMPLATE/`) and in the issue skill.
- **Issues** follow the issue skill (`.claude/skills/issue/SKILL.md`): the issue forms'
  sections, a type (`Bug`, `Feature` or `Task`), the `bug` or `enhancement` label, and a product
  label (`meko`, `agentk`, `presenter`) for each product the issue is about.
- **Keep the docs current** in the same commit as the code, and check the change against the
  REVIEW.md of the part you changed (`extensions/agentk/REVIEW.md` for AgentK, `backend/REVIEW.md`
  for the backend).
- **Treat the repo as public** (it's internal today): no internal hostnames, mount paths, tokens
  or keys in code, docs, tests or commit messages.
- Shell commands and scripts run on Linux and macOS: stick to POSIX flags that GNU and BSD tools
  share.
