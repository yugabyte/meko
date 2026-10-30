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
| `scripts/ci-scope.sh` | Which components' CI jobs a PR runs, from the paths it changes (Markdown only people read runs none). |
| `scripts/release/` | The release scripts the three products share, with `--product agentk\|meko\|presenter`: the next version, the release notes (and whether to release), and pruning old releases; and `fetch-node.sh`, which downloads, checks and unpacks the Node a release bundles, with retries. |
| `.github/` | CI (`ci.yml`: a job per component, in `extensions/agentk`, `extensions/presenter` or `backend`, run only when a PR changes it, and the `CI gate` over them), the PR title check, the three release trains (`agentk-release.yml` for AgentK, tags `agentk-v…`; `meko-release.yml` for Meko, tags `meko-v…`, the repo's latest release; `presenter-release.yml` for Presenter, tags `presenter-v…`), Dependabot, and the issue forms (`ISSUE_TEMPLATE/`: bug, feature and task; blank issues are off). |
| `.claude/skills/commit/` | The commit skill: how to write commits, PR titles and PR descriptions here. |
| `.claude/skills/issue/` | The issue skill: how to write, file and edit GitHub issues here, with the forms' sections, types and labels. |
| `README.md`, `CONTRIBUTING.md`, `LICENSE` | The repo's README (Meko: what it is, installing and building it, its main commands, and AgentK), how to contribute (with AgentK, or any coding agent starting from this file, and the rules), and the license for everything in it. Every command is in `backend/docs/cli.md`. AgentK's README is `extensions/agentk/README.md`. |
| `MEKO.md` | The Meko datapacks that hold context for this repo, with each one's ID and what it covers. How to use them is under [Meko datapacks](#meko-datapacks-mekomd). |

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
  them all. Root Markdown, and Markdown only people read inside a component (READMEs,
  `AGENTS.md`, `REVIEW.md`, `docs/`), run no job, but Markdown a test reads (`backend/docs/`,
  test fixtures) or a release ships (AgentK's `src/tools/skill/SKILL.md`) runs its component's,
  so a new test that reads Markdown, or a release that packs it, gets its path in the script
  too. A new component gets its own job in `ci.yml`, its paths in `scripts/ci-scope.sh`, a
  place in the gate's `needs`, and its name in `COMPONENTS` in `scripts/check-title.sh`. A new
  product also gets a GitHub label of its own name and an entry in the issue forms' Product
  dropdown (`.github/ISSUE_TEMPLATE/`) and in the issue skill.
- **Commands and their output are concise, self-explanatory and simple.** A command does what
  its name says, and there's one way to do a thing: no aliases or shortcuts beside it. Output
  says the result in plain words, one line for a success ("Connected Kiro CLI to Meko"), with
  no file names, URLs or inner workings the user doesn't need. Add a line only when the user
  has something to know or do ("ones already running need a restart"). An error says what's
  wrong and what to run next. Nothing should be readable two ways.
- **Issues** follow the issue skill (`.claude/skills/issue/SKILL.md`): the issue forms'
  sections, a type (`Bug`, `Feature` or `Task`), the `bug` or `enhancement` label, and a product
  label (`meko`, `agentk`, `presenter`) for each product the issue is about.
- **Keep the docs current** in the same commit as the code, and check the change against the
  REVIEW.md of the part you changed (`extensions/agentk/REVIEW.md` for AgentK, `backend/REVIEW.md`
  for the backend).
- **Treat the repo as public** (it's internal today): no internal hostnames, mount paths, tokens
  or keys in code, docs, tests or commit messages.
- **No secrets or personal data (PII)** in anything the repo or its pages on GitHub keep: code,
  comments, docs, tests and their fixtures, screenshots, commit messages, PR titles and
  descriptions, review replies and issues. Secrets are passwords, tokens, API keys, private keys
  and certificates, session cookies, TOTP seeds, and credentials in URLs. Personal data is any
  real person's name, email address, phone number, postal address, user name or handle, account
  or customer ID, or IP address; customer and company names; and anything a user wrote or stored
  (prompts, transcripts, memories, datapack contents). Use made-up values instead (`Alice`,
  `alice@example.com`, `github.com/acme/demo-repo`, a fake token such as `test-token`, `~` for a
  home folder), and scrub logs, command output and screenshots before pasting them in. Refer to
  people by role ("the reporter", "a reviewer"). The only exceptions are a change's author's
  configured git identity, the footer lines the harness asks for, and the Meko lines AgentK's PR
  prompt asks for (`<Meko host>://<datapack id>;<datapack name>`, `meko_design:…` and
  `meko_conversation_id:…`, which AgentK reads back): leave those as given. If you find a
  secret or personal data already in the repo, tell the user: it's removed in a change of its
  own, and a real secret is revoked too, since git history keeps it.
- Shell commands and scripts run on Linux and macOS: stick to POSIX flags that GNU and BSD tools
  share.

## Meko datapacks (`MEKO.md`)

A `MEKO.md` lists the Meko datapacks that hold context for the folder it sits in, with each one's
ID and what it covers; the root's [MEKO.md](MEKO.md) covers the whole repo. Like `REVIEW.md`, it
belongs to its subtree: use every `MEKO.md` from the root down to the files you're working on,
as AgentK's local code review does. Search their datapacks before any work here, even before
reading the code. Datapacks are shared per person: ask a datapack's owner for access.

Save new facts to the datapack in those `MEKO.md` files that owns the topic, or, when none does,
to your project's datapack (AgentK makes one for each project). A datapack that starts holding
context for part of the repo gets a row in that part's `MEKO.md`.

What to save in Meko, and what not to:

- **Don't save** secrets (passwords, tokens, keys, connection strings with credentials), what's in
  flight this session (that's for plans and tasks), anything the code or `git log` already says,
  or the story of a fixed bug: its cause, fix and check live in the commit and the issue. Keep
  only a trap the fix doesn't prevent, in one sentence.
- **Do save** facts the code doesn't hold, such as why a design is the way it is and the
  alternatives it rejected, and the sharp edges a future session would otherwise rediscover the
  hard way.
