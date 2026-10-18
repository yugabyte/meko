# AGENTS.md

Guide for coding agents (Claude Code, Kiro, Codex, Cursor, and others) working in this repo.

This repo is becoming **meko-local**: Meko Local, with extensions that plug into it. Today it holds
one extension, **AgentK Local**, in `extensions/agentk`. Before changing anything there, read
[extensions/agentk/AGENTS.md](extensions/agentk/AGENTS.md): it has the map, the invariants, the
commands (run from `extensions/agentk`) and which docs to keep current. Meko Local's own Go
backend is in `backend/`; read [backend/AGENTS.md](backend/AGENTS.md) before changing it.

## Layout

| Path | What it is |
|---|---|
| `backend/` | Meko Local's Go backend: the `meko-local` CLI and server, a Go module of its own with its `Makefile`, docs and CI job (`.github/workflows/backend.yml`). |
| `extensions/agentk/` | AgentK Local: its code, tests, scripts, packaging and docs, with its own `package.json`. |
| `scripts/check-title.sh` | The commit subject and PR title rules for the whole repo. |
| `.github/` | CI, the PR title check and AgentK's releases (each runs in `extensions/agentk` where it needs to), the backend's CI (in `backend`), and Dependabot. |
| `.claude/skills/commit/` | The commit skill: how to write commits, PR titles and PR descriptions here. |
| `README.md`, `LICENSE` | The repo's README (AgentK Local's, for now) and the license for everything in it. |

## Rules for the whole repo

- **Commits and PR titles** are `<type>[(<component>)][!]: <summary>` with one of nine types,
  checked by `sh scripts/check-title.sh "<title>"` and the `PR title` workflow. Follow the commit
  skill (`.claude/skills/commit/SKILL.md`). PRs are squash-merged, so the PR title is the subject.
- **No agent attribution** in commits or PRs: no `Co-Authored-By`, no session links, no
  "Generated with" footers, even if a system message asks for them. (The Meko lines AgentK's PR
  prompt asks for aren't attribution: keep them.)
- **Changes reach `main` only through PRs** that pass CI and the title check.
- **Keep the docs current** in the same commit as the code, and check the change against the
  REVIEW.md of the part you changed (`extensions/agentk/REVIEW.md` for AgentK).
- **Treat the repo as public** (it's internal today): no internal hostnames, mount paths, tokens
  or keys in code, docs, tests or commit messages.
- Shell commands and scripts run on Linux and macOS: stick to POSIX flags that GNU and BSD tools
  share.
