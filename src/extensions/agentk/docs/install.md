# Installing and running Kode

The [README](../README.md#install) has the short version. This page has the details.

## Install

You need Linux (x64 or arm64) or macOS 14 or newer on Apple Silicon, and a coding agent:
[Claude Code](https://claude.com/claude-code), [OpenCode](https://opencode.ai) 2.0.0 or later,
[Kiro CLI](https://kiro.dev/cli/) 2.32 or later, or more than one.
You also need [Meko](https://mekodata.ai) on this computer: it serves Kode to your browser, at
`/ext/kode/` on its own port (7777), and decides who can reach it. Meko also gives projects datapacks (shared
memory) and lets you share projects. Kode uses the Meko on this computer, which keeps your datapacks
there, or in Meko Cloud once you link it in Meko's web UI (Settings › Meko Cloud, which links to Meko
Cloud's sign-up and login pages to make a key; or `meko cloud-link` on a computer without a browser).
Sharing needs Meko Cloud. Install Meko, and then Meko installs Kode:

```sh
curl -fsSL https://mekodata.ai/install.sh | sh
meko extension install kode
```

(or **Install** on Meko's Extensions page). When Meko's installer adds `~/.local/bin` to your PATH,
run the second command in a new terminal: the one the installer ran in keeps its old PATH. Meko downloads Kode's newest release, checks it and
unpacks it into its data folder, then runs Kode's setup, which offers to install anything that's
missing (git, curl, and Claude Code, OpenCode or Kiro CLI when none is there: it asks which, or pass
`meko extension install kode -- --agent opencode` or `-- --agent kiro`) and to set up Meko for
each agent, asking first (Meko's own setup runs quietly: its output shows only if it fails).
gh, GitHub's command-line tool, is optional: Kode needs it to open PRs and follow reviews, and
asks for it the first time something needs it, with a dialog that installs it (without root) and
signs it in. Setup offers it in a question of its own once the rest is there, `-- --install-deps`
installs it with the rest, and an install that can't ask (Meko's Extensions page) goes on without
it and says so. Then
it prints the address to open, and `meko kode ui`, `status` and `logs`. Its rows say only what
needs a look: services that run and start at login go without one. On macOS the services are
LaunchAgents: they start when you log in and stop when you log out, and sessions pause while the
Mac sleeps. When Kode uses the Meko on
this computer, with no Meko Cloud link, it ends with `! Meko Cloud  not linked` and how to link it.

## Who can open it

Who can open it is Meko's setting (`[web] access`: chosen when Meko was installed, in its
`web.toml`, and `config.toml` can override it): **local** (this computer only) or **remote** (other
devices too, like your phone, over HTTPS); remote is the default over SSH or on a machine with no
display. Neither has a login: with remote access, anyone who can reach the port can open Kode and
its terminals, so pick it only where that's who you mean. `meko ui stop` on the host turns the web
UI off, Kode's page with it, until `meko ui start`.
A Kode that served browsers itself (on port 7777, before Meko did) hands its port, access and
certificate over to Meko by itself; its address moves to `/ext/kode/` on the same port.

## Updates, setup and removal

Meko installs Kode's updates too: on its own when it finds one (each extension has an **Update
automatically** switch on Meko's Extensions page and in Kode's Settings › Updates, on by
default), or when you click **Update** (the banner in Kode, Settings › Updates, or Meko's
Extensions page) or run `meko update` (Meko, then every extension). An update restarts only
Kode's web server, so sessions keep running, and Meko rolls back a version that fails to start.
To change Kode's setup (repos folder, Meko) or set up its services again, run `meko kode install`,
or `meko extension install kode` again (`--version <v>` installs another release).
`meko extension delete kode` removes Kode (it stops, ending its sessions) and keeps its home;
`--purge` deletes that too.

## Where Kode keeps its data

Kode keeps its data in Meko's data folder, under `extensions/kode`: `~/.meko/extensions/kode`,
or under `MEKO_HOME` when you set that for Meko. You don't pick another place. If your home directory
is on a network filesystem (NFS, say), Meko's data folder, and Kode's with it, is on a local disk
instead: `$XDG_STATE_HOME/meko` when that's local, else `/var/tmp/<you>/meko`. Many Linux systems
clean `/var/tmp` by age, so set `XDG_STATE_HOME` to a local folder there (Kode warns when it's in
`/var/tmp`). Kode runs only on the host you installed it on.

## Installs from before Meko

An install from before Meko kept its data in `~/.agent-k-local`. It moves into Meko's data folder by
itself right after updating to a release that brings the move (and Meko updates it from then on): every session ends and resumes there,
and `~/.agent-k-local` becomes a link to the new place. If the Meko installed here is older than
the one whose `meko destroy` keeps Kode's data, the move waits until you update it (`meko update`).

## A home under /net

If your home is under `/net` (the automounter's `/net/<host>/...`), Claude Code 2.1.292 and later
asks before every file it reads or edits there, even in auto mode. When the home is this host's
own folder, Kode starts Claude in it without the `/net/<host>` prefix (with `HOME` to
match), so sessions don't stop for that. In plain Claude Code, open your repos by that path too.

## On a Mac

On a Mac, install while you're logged in to the desktop (in person or with Screen Sharing): Claude
Code keeps its login in your Keychain, so Kode runs while you're logged in. For a Mac mini
that stays on, turn on automatic login and stop it sleeping (System Settings > Energy). With the
macOS firewall on, allow incoming connections for `~/.meko/extensions/kode/app/current/node/bin/me_kode` so
your phone can connect.

## Commands

| Command | What it does |
|---|---|
| `meko kode ui` | Open Kode in your browser through Meko (`open` on macOS, `xdg-open` on Linux); with no display to open it on, such as over SSH, it prints the URL |
| `meko kode status` | Laid out as `meko status`: a row each for the server (`healthy`), the session host and its sessions, and the services, then a `● Kode <version> is running · pid, up` headline, the web UI's URL, the home and the log as plain rows, and what to run next (`meko kode ui`, or `meko kode start` when stopped); exits 3 when no server runs this home. Run by `meko status` (`MEKO_SUMMARY=1`), it prints only `✓ Kode <version> is running (pid, up)`, which `meko status` shows in its Kode row; off a terminal the full output starts with that line too, for a Meko from before that |
| `meko kode stop` / `start` | Stop the server and the session host, ending every session, and start them again (sessions resume). The services also start again at the next login |
| `meko kode restart` | Restart the web server only (sessions keep running), and wait until it answers; Meko runs it after installing an update |
| `meko kode logs -f` | Follow the server log (`logs/server.log` in Kode's home) |
| `meko kode uninstall [--purge [--delete-worktrees]]` | Remove the services; `--purge` also deletes Kode's home (in Meko's data folder) except session worktrees and designs, which only `--delete-worktrees` removes (after reporting uncommitted changes). Branches are always kept. |
| `meko kode attach [session]` | Show a session's terminal in yours (`Ctrl+\` detaches; Claude keeps running); with no name, list the sessions |
| `meko kode review [--base <branch>] [--pr <pr> \| --description <text>] [--depth 1-5] [--strategy <id>] [--json]` | Review the change in this folder (from where it left its base to the working tree, uncommitted files included) with Kode's code review, on Claude Code, without a session or the server; print the kept findings and exit 0, or 1 when it couldn't review. See [From the command line](design/code-review.md#from-the-command-line) |
| `meko kode host restart` | Restart the session host (its sessions resume); with the server stopped, `host stop` ends every session until the server starts |

Kode has no command of its own: `meko kode <command>` runs it for the Meko it belongs to. Its
program is `me_kode`, so a process list and the services say Kode is a Meko extension. It was
`me_koder` before, `agentk` before that, and `agent-k-local` before the rename to Kode. Meko
removes those commands from `~/.local/bin` when it next starts; an install's services keep their old
names (`me_koder.service`, `com.yugabyte.me_koder`, `agentk…` or `agent-k-local…`) until you run
`meko kode install`, which renames the server's at once and the session host's once the host isn't running.
