<p align="center"><img src="assets/AgentK-nobg.png" width="120" alt="AgentK logo"></p>

<p align="center">
  <a href="https://mekodata.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="../../backend/assets/badges/meko-context-dark.svg">
      <img src="../../backend/assets/badges/meko-context.svg" width="112" height="30" alt="Meko context">
    </picture>
  </a>
</p>

# AgentK

The agentic coder for ADLC. AgentK lets you manage many projects and [Claude Code](https://claude.com/claude-code),
[OpenCode](https://opencode.ai) or [Kiro CLI](https://kiro.dev/cli/) sessions at once, with [Meko](https://mekodata.ai) as the shared context layer so designs, transcripts, and what each session learned flow into 
datapacks your whole team and their agents can build on.

It's self-hosted on your devserver (headless), your Mac or a Mac mini, and you use it from a browser or
your phone.

![AgentK: a session in the Design stage, with the live Claude Code terminal, its design (a Claude Artifact for a world clock widget) in the design panel waiting for approval, and the 5-hour and weekly limits in the rail](docs/screenshots/workbench.png)

<table>
  <tr>
    <td width="50%"><img src="docs/screenshots/design-review.png" alt="Design stage: Claude's plan shown next to the terminal, with Approve design"><br><sub><b>Design review.</b> Claude's plan, rendered next to the terminal, waiting for your approval.</sub></td>
    <td width="50%"><img src="docs/screenshots/new-project.png" alt="Open project dialog listing the repos under ~/code"><br><sub><b>Open project.</b> Pick a repo from your repos folder; a dedicated Meko datapack is created with it.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/screenshots/project-home.png" alt="Project home with datapacks, activity, token usage and settings"><br><sub><b>Project home.</b> Datapacks, memory search, activity, token usage by day, session and model, and settings.</sub></td>
    <td width="50%"><img src="docs/screenshots/workbench-dark.png" alt="The workbench in the dark theme"><br><sub><b>Dark theme.</b> Follows your system, or pick one.</sub></td>
  </tr>
  <tr>
    <td width="50%"><img src="docs/screenshots/changes-comments.png" alt="Changes panel with a draft review comment and a new comment on a range of lines"><br><sub><b>Review comments.</b> Comment on lines of the diff, then send them all to Claude at once.</sub></td>
    <td width="50%"><img src="docs/screenshots/mobile.png" alt="A session on a phone: app bar, one-line stage bar, the terminal, its key bar and a compose box"><br><sub><b>On a phone.</b> The terminal with a key bar and a compose box, and the stage gate in reach.</sub></td>
  </tr>
  <tr>
    <td colspan="2"><img src="docs/screenshots/kiro-session.png" alt="A session running on Kiro CLI in the Design stage: Kiro's terminal with the design it wrote and its open questions, the markdown design in the design panel waiting for approval, and the footer showing the credits Kiro billed"><br><sub><b>Sessions on Kiro.</b> The same stages, gates and design review with Kiro CLI as the agent; the footer shows the credits it billed.</sub></td>
  </tr>
</table>

## Features

- **Many tasks at once.** Each task gets its own git worktree, branch and agent session (Claude
  Code, OpenCode or Kiro CLI), with the real terminal in your browser. Sessions keep running when
  you close the page.
- **From idea to merged PR.** Every task runs on Explore → Design → Code → Review & PR → Merge &
  Close. You approve the design, opening the PR and the merge, or let **Run to merge** do it all.
- **Code review on your computer.** AgentK reviews each change before the PR opens, and whenever you
  ask. You check the findings, and AgentK fixes the ones you keep.
- **Review someone else's PR** with AgentK summing it up, the author's design beside it, and
  answers to your questions. It posts to GitHub only when you say.
- **Shared memory with Meko.** Each project has a Meko datapack. Designs and what each session
  learned are saved there, so your team and its agents start from the same context.
- **Ask AgentK** (`⌘K`) in plain words: "start a design task to add dark mode", or paste a GitHub
  issue or PR link to start on it.
- **Works from your phone**, and installs as an app.
- **Token usage and cost** per session and project, and your Claude plan limits.

[All features](docs/features.md)

## Install

You need Linux (x64 or arm64) or macOS 14 or newer on Apple Silicon, and
[Claude Code](https://claude.com/claude-code), [OpenCode](https://opencode.ai) or
[Kiro CLI](https://kiro.dev/cli/). Install [Meko](https://mekodata.ai), then AgentK:

```sh
curl -fsSL https://mekodata.ai/install.sh | sh
meko extension install agentk
```

Setup offers to install anything missing and prints the address to open. A new install opens on
the AgentK tour. Updates install by themselves.

To share projects with your team, link Meko to Meko Cloud: Settings › Meko Cloud in Meko's web UI,
or `meko cloud_link`.

AgentK has no login. With Meko's remote access (the default over SSH), anyone who can reach port
7777 can open it and its terminals.

[Install details](docs/install.md): who can open it, updates, where data lives, and Mac notes.

## Commands

| Command | What it does |
|---|---|
| `me_koder ui` | Open AgentK in your browser |
| `me_koder status` | Show whether AgentK is running, and its address |
| `me_koder attach [session]` | Show a session's terminal in yours (`Ctrl+\` detaches) |
| `me_koder logs -f` | Follow the server log |

[All commands](docs/install.md#commands)

## Docs

- [Features](docs/features.md) and [install details](docs/install.md)
- [Design](docs/design.md): how AgentK works, one file per area
- [Contributing](../../CONTRIBUTING.md), and [AGENTS.md](AGENTS.md) for AgentK's commands and map

## License

[Apache-2.0](../../LICENSE)

The Claude Code, OpenCode and Kiro logos in `src/web/public/` are their owners' marks, used to show
which agent a session runs on. The Claude Code mascot is drawn from
[dashboard-icons](https://github.com/homarr-labs/dashboard-icons) (Apache-2.0), and OpenCode's is its
favicon from [its repo](https://github.com/anomalyco/opencode) (MIT).
