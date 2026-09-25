<p align="center"><img src="assets/AgentK.png" width="120" alt="AgentK logo"></p>

# agent-k-local

A self-hosted workbench for [Claude Code](https://claude.com/claude-code). It runs headless on
your devserver, and you use it from a browser:

- **Projects** in a left rail, each one a git repo picked from your repos folder (`~/code` by default).
- **One git worktree and one Claude session per task.** The real Claude Code terminal UI is shown
  in the browser, and sessions are backed by tmux, so they keep running when you disconnect. You
  can still attach from iTerm: `tmux -L agentk attach -t ak-<project>-<session>`.
- **Status at a glance:** see which sessions are working, which need you (a permission prompt or a
  question), and which are idle, with browser notifications.
- **Task lifecycle:** Design → Code → PR → Review (AgentK) → Merge → Cleanup. You approve the
  design, opening the PR and merging. The server waits for reviews and CI and hands AgentK
  findings to Claude.
- **Meko datapacks** give every project a memory: one dedicated datapack plus any number of
  shared ones.
- **Survives restarts:** after a host reboot, every open session is brought back with
  `claude --resume`, and sessions that were mid-turn are told to continue.
- **Updates itself** from GitHub Releases without interrupting running sessions.

Design: [`docs/design.html`](docs/design.html).

## Requirements

- Linux (x64 or arm64) with tmux ≥ 2.7, git, curl, the GitHub CLI (`gh`) and Claude Code
- A [Meko](https://mekodata.ai) account and the `meko-agent-skills` Claude Code plugin

## Install

The repo is internal for now, so the installer is fetched with `gh`:

```sh
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
```

It installs to `~/.agent-k-local`, sets up two systemd user units (the web server, and the tmux
server that owns your sessions), and prints a one-time setup link. Lost your password? Run
`agent-k-local setup-link` on the host.

## Develop

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE)
