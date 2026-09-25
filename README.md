# agent-k-local

A self-hosted workbench for [Claude Code](https://claude.com/claude-code). It runs headless on
your devserver, and you use it from a browser:

- **Projects** in a left rail, each one a git repo picked from your repos folder (`~/code` by default).
- **One git worktree and one Claude session per task.** The real Claude Code terminal UI is shown
  in the browser, and sessions are backed by tmux so they keep running when you disconnect.
- **Status at a glance:** see which sessions are working, which need you, and which are idle.
- **Meko datapacks** give every project a memory: one dedicated datapack plus any number of
  shared ones.
- **Survives restarts:** after a host reboot, every open session is brought back with
  `claude --resume`.
- **Updates itself** from GitHub Releases without interrupting running sessions.

> Status: design phase. See [`docs/design.html`](docs/design.html).

## Requirements

- Linux (x64 or arm64) with tmux ≥ 2.7, git, and Claude Code
- A [Meko](https://mekodata.ai) account and the `meko-agent-skills` Claude Code plugin

## License

[Apache-2.0](LICENSE)
