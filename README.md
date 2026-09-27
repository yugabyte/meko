<p align="center"><img src="assets/AgentK-nobg.png" width="120" alt="AgentK logo"></p>

# AgentK Local

The agentic coder for ADLC. AgentK Local lets you manage many projects and [Claude Code](https://claude.com/claude-code) sessions at 
once, with [Meko](https://mekodata.ai) as the shared context layer so designs, transcripts, and shipped work flow into 
datapacks your whole team and their agents can build on.

It's self-hosted on your devserver (headless), your Mac or a Mac mini, and you use it from a browser or
your phone.

![AgentK Local: a session in the Design stage, with the live Claude Code terminal, its design (a Claude Artifact for a world clock widget) in the design panel waiting for approval, and the 5-hour and weekly limits in the rail](docs/screenshots/workbench.png)

<table>
  <tr>
    <td width="50%"><img src="docs/screenshots/design-review.png" alt="Design stage: Claude's plan shown next to the terminal, with Approve design"><br><sub><b>Design review.</b> Claude's plan, rendered next to the terminal, waiting for your approval.</sub></td>
    <td width="50%"><img src="docs/screenshots/new-project.png" alt="New project dialog listing the repos under ~/code"><br><sub><b>New project.</b> Pick a repo from your repos folder; a dedicated Meko datapack is created with it.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/screenshots/project-home.png" alt="Project home with datapacks, activity, token usage and settings"><br><sub><b>Project home.</b> Datapacks, memory search, activity, token usage by day, session and model, and settings.</sub></td>
    <td width="50%"><img src="docs/screenshots/workbench-dark.png" alt="The workbench in the dark theme"><br><sub><b>Dark theme.</b> Follows your system, or pick one.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/screenshots/changes-comments.png" alt="Changes panel with a draft review comment and a new comment on a range of lines"><br><sub><b>Review comments.</b> Comment on lines of the diff, then send them all to Claude at once.</sub></td>
    <td width="50%"><img src="docs/screenshots/mobile.png" alt="A session on a phone: app bar, one-line stage bar, the terminal, its key bar and a compose box"><br><sub><b>On a phone.</b> The terminal with a key bar and a compose box, and the stage gate in reach.</sub></td>
  </tr>
</table>

## Install

You need Linux (x64 or arm64) or macOS 14 or newer on Apple Silicon, and a
[Meko](https://mekodata.ai) account and API key. The repo is internal for now, so the installer is
fetched with `gh`:

```sh
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
```

It offers to install anything that's missing (tmux, git, gh, Claude Code) and to set up Meko for
Claude Code, asking first. It asks for **local** access (this computer only, no login) or
**remote** access (other devices too, like your phone, over HTTPS with a login); remote is the
default over SSH or on a machine with no display. Then it prints the address, or for remote
access a one-time setup link that creates your login. Local access trusts everyone who can log
in to the computer, so pick remote on a shared host.

On a Mac, install while you're logged in to the desktop (in person or with Screen Sharing): Claude
Code keeps its login in your Keychain, so AgentK Local runs while you're logged in. For a Mac mini
that stays on, turn on automatic login and stop it sleeping (System Settings > Energy). With the
macOS firewall on, allow incoming connections for `~/.agent-k-local/app/current/node/bin/node` so
your phone can connect.

## Features

- **Projects** in a left rail: each is an effort to build or improve a feature, product or service,
  worked on in a git repo picked from your repos folder (`~/code` by default).
  A repo can back several projects, each with its own datapack, settings and sessions.
  New sessions start from the base branch after it's fast-forwarded to `origin`.
- **Meko, the shared context layer:** every project has a dedicated datapack plus any number of
  shared ones. Designs, session transcripts and a summary of each merged PR are saved there, so
  the whole team, and its agents, start from the same context.
- **Share a project** by sharing its dedicated datapack with someone. A project datapack shared
  with you shows a banner and a notification, and opens as a new project with the repo and name
  filled in. The Meko server and key are set on the Meko tab in Settings, for your own Meko too.
- **One git worktree and one Claude session per task.** The real Claude Code terminal UI is shown
  in the browser, and sessions are backed by tmux, so they keep running when you disconnect. You
  can still attach from iTerm: `tmux -L agentk attach -t ak-<project>--<session>` (sessions created
  before this naming keep `ak-<project>-<session>`).
- **Changes panel:** click the `+N −N` diff size in a session's footer to see every changed file
  (committed, uncommitted and untracked) with a diff per file. Jump between files with the
  **Files** menu or `[` / `]`, and leave review comments on lines that go to Claude as one prompt.
- **Works from your phone:** a drawer for projects, a key bar (Esc, a sticky Ctrl for
  Ctrl+letter shortcuts, Shift+Tab, arrows, Ctrl+C) and a compose box for the terminal, swipe to scroll back, and the stages and their gates
  in the ⋯ menu.
- **Status at a glance:** see which sessions are working, which need you (a permission prompt or a
  question), and which are idle, with browser notifications.
- **Three session types:** *Exploration* for investigating, POCs and experiments (no design, PR or
  merge), *Plan & Implement* (the default) for the full lifecycle below, and *Quick Code* for small
  tasks that go straight to code.
- **Task lifecycle:** Design → Code → PR → Review (AgentK) → Merge & Close. Complex designs are
  published as native Artifacts with diagrams and shown right next to the terminal; simple ones are
  markdown. You approve the design, opening the PR, and Merge & Close, which merges the PR and
  cleans up the worktree and branches. The server watches the PR and hands reviews (AgentK's and
  people's by default, or every reviewer's, or only people's) and CI failures to Claude.
- **Token usage** per session and per project (by model and by day), read from Claude Code transcripts,
  subagents included. **Usage** in the left panel adds it up across every project, deleted ones
  too, with what shipped: merged PRs and commits pushed straight to the base branch. On Pro and Max plans, the rail also shows your 5-hour and weekly usage limits
  and when they reset.
- **Survives restarts:** after a host reboot, every open session is brought back with
  `claude --resume`, and sessions that were mid-turn are told to continue.
- **One-click updates** from GitHub Releases, without interrupting running sessions, and rollback
  if a new version won't start.

Design: [`docs/design.md`](docs/design.md).

## Commands

| Command | What it does |
|---|---|
| `agent-k-local status` | Services, URL, certificate fingerprint, sessions, and whether the server answers |
| `agent-k-local logs -f` | Follow the server log (`~/.agent-k-local/logs/server.log`) |
| `agent-k-local setup-link` | A new one-time login link; also resets a lost password |
| `agent-k-local uninstall [--purge [--delete-worktrees]]` | Remove the services; `--purge` also deletes `~/.agent-k-local` except session worktrees and designs, which only `--delete-worktrees` removes (after reporting uncommitted changes). Branches are always kept. |
| `tmux -L agentk attach -t ak-<project>--<session>` | Attach to a session from any terminal |

## Develop

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
