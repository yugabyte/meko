# Installing and running AgentK

The [README](../README.md#install) has the short version. This page has the details.

## Install

You need Linux (x64 or arm64) or macOS 14 or newer on Apple Silicon, and a coding agent:
[Claude Code](https://claude.com/claude-code), [OpenCode](https://opencode.ai) 2.0.0 or later,
[Kiro CLI](https://kiro.dev/cli/) 2.32 or later, or more than one.
You also need [Meko](https://mekodata.ai) on this computer: it serves AgentK to your browser, at
`/ext/agentk/` on its own port (7777), and decides who can reach it. Meko also gives projects datapacks (shared
memory) and lets you share projects. AgentK uses the Meko on this computer, which keeps your datapacks
there, or in Meko Cloud once you link it in Meko's web UI (Settings › Meko Cloud, which links to Meko
Cloud's sign-up and login pages to make a key; or `meko cloud_link` on a computer without a browser).
Sharing needs Meko Cloud. Install Meko, and then Meko installs AgentK:

```sh
curl -fsSL https://mekodata.ai/install.sh | sh
meko extension install agentk
```

(or **Install** on Meko's Extensions page). Meko downloads AgentK's newest release, checks it and
unpacks it into its data folder, then runs AgentK's setup, which offers to install anything that's
missing (git, gh, and Claude Code, OpenCode or Kiro CLI when none is there: it asks which, or pass
`meko extension install agentk -- --agent opencode` or `-- --agent kiro`) and to set up Meko for
each agent, asking first (Meko's own setup runs quietly: its output shows only if it fails). Then
it prints the address to open, and `me_koder ui`, `status` and `logs`. Its rows say only what
needs a look: services that run and start at login go without one. On macOS the services are
LaunchAgents: they start when you log in and stop when you log out, and sessions pause while the
Mac sleeps. When AgentK uses the Meko on
this computer, with no Meko Cloud link, it ends with `! Meko Cloud  not linked` and how to link it.

## Who can open it

Who can open it is Meko's setting (`[web] access`: chosen when Meko was installed, in its
`web.toml`, and `config.toml` can override it): **local** (this computer only) or **remote** (other
devices too, like your phone, over HTTPS); remote is the default over SSH or on a machine with no
display. Neither has a login: with remote access, anyone who can reach the port can open AgentK and
its terminals, so pick it only where that's who you mean. `meko ui stop` on the host turns the web
UI off, AgentK's page with it, until `meko ui start`.
An AgentK that served browsers itself (on port 7777, before Meko did) hands its port, access and
certificate over to Meko by itself; its address moves to `/ext/agentk/` on the same port.

## Updates, setup and removal

Meko installs AgentK's updates too: on its own when it finds one (each extension has an **Update
automatically** switch on Meko's Extensions page and in AgentK's Settings › Updates, on by
default), or when you click **Update** (the banner in AgentK, Settings › Updates, or Meko's
Extensions page) or run `meko update` (Meko, then every extension). An update restarts only
AgentK's web server, so sessions keep running, and Meko rolls back a version that fails to start.
To change AgentK's setup (repos folder, Meko) or set up its services again, run `me_koder install`,
or `meko extension install agentk` again (`--version <v>` installs another release).
`meko extension delete agentk` removes AgentK (it stops, ending its sessions) and keeps its home;
`--purge` deletes that too.

## Where AgentK keeps its data

AgentK keeps its data in Meko's data folder, under `extensions/agentk`: `~/.meko/extensions/agentk`,
or under `MEKO_HOME` when you set that for Meko. You don't pick another place. If your home directory
is on a network filesystem (NFS, say), Meko's data folder, and AgentK's with it, is on a local disk
instead: `$XDG_STATE_HOME/meko` when that's local, else `/var/tmp/<you>/meko`. Many Linux systems
clean `/var/tmp` by age, so set `XDG_STATE_HOME` to a local folder there (AgentK warns when it's in
`/var/tmp`). AgentK runs only on the host you installed it on.

## Installs from before Meko

An install from before Meko kept its data in `~/.agent-k-local`. It moves into Meko's data folder by
itself right after updating to a release that brings the move (and Meko updates it from then on): every session ends and resumes there,
and `~/.agent-k-local` becomes a link to the new place. If the Meko installed here is older than
the one whose `meko destroy` keeps AgentK's data, the move waits until you update it (`meko update`).

## A home under /net

If your home is under `/net` (the automounter's `/net/<host>/...`), Claude Code 2.1.292 and later
asks before every file it reads or edits there, even in auto mode. When the home is this host's
own folder, AgentK starts Claude in it without the `/net/<host>` prefix (with `HOME` to
match), so sessions don't stop for that. In plain Claude Code, open your repos by that path too.

## On a Mac

On a Mac, install while you're logged in to the desktop (in person or with Screen Sharing): Claude
Code keeps its login in your Keychain, so AgentK runs while you're logged in. For a Mac mini
that stays on, turn on automatic login and stop it sleeping (System Settings > Energy). With the
macOS firewall on, allow incoming connections for `~/.meko/extensions/agentk/app/current/node/bin/me_koder` so
your phone can connect.

## Commands

| Command | What it does |
|---|---|
| `me_koder ui` | Open AgentK in your browser through Meko (`open` on macOS, `xdg-open` on Linux); with no display to open it on, such as over SSH, it prints the URL |
| `me_koder status` | Shaped like `meko status`: a headline (`✓ AgentK <version> is running (pid, up)`, which `meko status` shows in its AgentK row), a row each for the server (`healthy`), the session host and its sessions, and the services, then the web UI's URL, the home and the log; exits 3 when no server runs this home |
| `me_koder stop` / `start` | Stop the server and the session host, ending every session, and start them again (sessions resume). The services also start again at the next login |
| `me_koder restart` | Restart the web server only (sessions keep running), and wait until it answers; Meko runs it after installing an update |
| `me_koder logs -f` | Follow the server log (`logs/server.log` in AgentK's home) |
| `me_koder uninstall [--purge [--delete-worktrees]]` | Remove the services; `--purge` also deletes AgentK's home (in Meko's data folder) except session worktrees and designs, which only `--delete-worktrees` removes (after reporting uncommitted changes). Branches are always kept. |
| `me_koder attach [session]` | Show a session's terminal in yours (`Ctrl+\` detaches; Claude keeps running); with no name, list the sessions |
| `me_koder host restart` | Restart the session host (its sessions resume); with the server stopped, `host stop` ends every session until the server starts |

The command is `me_koder`, so a process list, `~/.local/bin` and the services say AgentK is a Meko
extension. It was `agentk` before, and `agent-k-local` before the rename to AgentK. An install from
then gets `me_koder` when Meko next starts, and the old commands go; its services keep their old
names (`agentk.service`, `com.yugabyte.agentk`, or `agent-k-local…`) until you run `me_koder
install`, which renames the server's at once and the session host's once the host isn't running.
