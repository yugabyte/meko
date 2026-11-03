# Surviving crashes, restarts and updates

Part of the [AgentK design](../design.md).

Two systemd user units run with lingering enabled, so they start at boot without anyone logging
in:

- `agentk-host.service` runs the session host (`agentk host`). It is never
  restarted by updates or reinstalls.
- `agentk.service` is the web server, with `Restart=always`. Updates restart only this unit.

(On macOS they're the LaunchAgents `com.yugabyte.agentk.host` and `com.yugabyte.agentk`.) Until the
rename to AgentK they were `agent-k-local-host.service` and `agent-k-local.service`
(`com.yugabyte.agent-k-local.host` and `com.yugabyte.agent-k-local`). A service installed under
its old name keeps it: `service.ts` names whichever is installed, and an update doesn't touch the
units, which start `bin/agent-k-local`, a link to `bin/agentk` in every release since. `agentk
install` moves them to the new names (`dropOldNames`): the server's at once, since it restarts the
server anyway, and the host's only while the host isn't running, since stopping it would end every
session. Until the host stops, the server's new unit names the host by its old name.

Keeping the session host in its own unit is the key point. If it were a child of the server's unit,
systemd would kill every Claude session on each update. The server never starts a host of its own
while the host service is installed (it reports the service as down), nor while it runs as a
service itself without the host's (it says to run `agentk install`); without services (a
source checkout, `--no-service`) it starts one detached, in its own process group.

**The host keeps its version.** A running host keeps the code it started with across updates:
restarting it ends every Claude (the server's recovery resumes them). So the version folder it
started from is never deleted while it runs (node-pty runs a helper from it on macOS): Meko's
launcher, which starts the host (`meko _launch`), holds a lock on that folder for as long as the
host runs, and Meko's pruning skips a locked folder ([Releases](releases.md)). The
server works with hosts of its own protocol back to `MIN_PROTOCOL`; an older one has to be
restarted (`agentk host restart`), which the server's log says. `agentk host stop`
ends every session in the host, and refuses while the server for this home runs: it would start
the host again at once and resume them all. With the server stopped, they resume when it starts.
`agentk stop` stops both, the server first, and `agentk start` starts them again.
Installed services stop through systemd (`systemctl --user stop`) or launchd (`bootout`), since
both restart a process that is only killed or exits on its own. Then it checks the server's lease:
a server outside the service (`serve` by hand, or any server without services) gets SIGTERM, but
only when the lease names this machine and its pid is an `agentk serve` (after a crash the
pid can be another process's); a lease held on another host is refused. The host goes last:
its service is stopped, then any host still on the socket (a detached one a server started before
the services were installed) gets a stop request. The services still start again at the next login (or
boot, with lingering) or reinstall; `start` starts them sooner.

**On macOS** the same two services are LaunchAgents in your `gui/<uid>` domain
(`~/Library/LaunchAgents/com.yugabyte.agentk.plist` and `….host.plist`), with `RunAtLoad`
and `KeepAlive`. They start when you log in, not at boot: Claude Code keeps its login in the
Keychain, which is only unlocked during a desktop login, so the installer refuses to run without
one. The host runs in the foreground, as launchd wants. Both jobs set
`LANG=en_US.UTF-8`, because launchd sets no locale and Claude needs a UTF-8 one. Updates
restart the server with `launchctl kickstart -k`, or by exiting and letting `KeepAlive` bring it
back. Reinstalling never reloads a loaded host job, and always reloads the server's (with
`bootout` and `bootstrap`), since the file on disk may be newer than the job launchd loaded.

Both jobs run `<home>/service/me_agentk`, a script that execs `app/current/bin/agentk`; macOS
lists a job under its program's file name, so Login Items shows AgentK as `me_agentk`. macOS's
Background Task Management shows "Background Items Added" again whenever the program a LaunchAgent
names turns out to be another file, and `app/current/bin/agentk` is another file after every
update. An update never replaces the launcher. `install` and the server's start
(`refreshServiceFiles`) write it, and the plists, only when their text changes, since a rewritten
file is announced too. At its start the server also brings plists from before the launcher onto it
without reloading the jobs, which run from the new plists from the next login (or, for the server,
the next `install`). That move shows the alert one last time.

**Their PATH.** systemd and launchd start both services with a thin PATH (launchd's is
`/usr/bin:/bin:/usr/sbin:/sbin`). At its start, each one reads PATH from the user's login shell
(`adoptLoginPath` in `launcher.ts`), then adds `~/.local/bin`, `/usr/local/bin` and `/opt/homebrew/bin`
when they're missing. The shell gets 5 s. A slow profile can take longer. So can a profile that
reads `~/Downloads`, `~/Documents` or `~/Desktop` on macOS: macOS asks whether Meko may open the
folder, and the shell waits for the answer. macOS asks again after each Meko update, because the
`meko` program is a new one. When the shell gives no PATH, the service keeps the thin PATH with
those three folders, and `server.log` (or `host.log`) says why. If AgentK still can't find `gh`, its
error says so in words (`ghMissing` in `github.ts`), not as `spawn gh ENOENT`. It says to install
`gh`, or, when the shell gave no PATH, to allow macOS's prompt, and then to run `agentk restart`.
Sessions don't depend on this: their launcher runs the login shell itself.

**Versions before the session host** ran every Claude in tmux, in a tmux service. The server
moved an install off it by itself on its first start after the update (it added the host service,
ended the sessions in tmux, and removed the tmux service once the update was final). That shipped
on 2026-10-09 and was removed about a week later. A Claude must never run in tmux and in the host at
once, so what's left is a guard (`Core.start`): while the tmux service is installed
(`legacyTmuxInstalled`), the server refuses to start, saying to run `agentk install`, unless
the host service is installed too and none of this home's sessions that aren't archived is in
tmux (`legacytmux.ts` lists the sessions on the old socket). An
install updated from a tmux version therefore fails to start and is rolled back to it (Meko's
launcher; see [Releases](releases.md)), with its sessions still running in tmux.
`agentk install`, which `meko extension install agentk` runs, moves it: before it installs the services it
removes the old ones, stopping the old server first and then the tmux service, which ends the
Claudes in it, and the new server's recovery resumes them in the host (auto-continuing a turn that
was cut off). `agentk uninstall` removes the tmux service too. Removing it ends the tmux
server on its socket, which every AgentK home on the account shared.

An install can have both services: the move kept the tmux service while any `ak-` pane it didn't
end was still in tmux (it ended only this home's sessions, so a deleted session's Claude, or
another home's, kept it there). Its sessions already run in the host, so the server starts and logs
a warning, and leaves the tmux service running rather than end the Claudes still in it.
(Refusing there, as versions 0.20261018.3 to 0.20261020.2 did, rolled back every update.) The host
service alone doesn't prove the move finished: a tmux version reinstalled by hand after it keeps
the host's file and resumes its sessions in tmux, which is why the guard also looks for this home's
sessions there. With both services installed, `agentk install` stops the server (it may be
the tmux version's, which would start them in tmux again) and removes the tmux service, ending any
Claude still in it, and leaves the host and its sessions alone.

All of this sits behind one interface in `src/server/service.ts` (install, start, stop, restart,
state, remove, boot check, the old tmux service), with a systemd and a launchd implementation picked by platform.
`servicesFor(home)` uses the installed services only when their `AK_HOME` is exactly this home, so
a dev server or a second home never acts on another install's services.

## One server per home

Two servers on one AgentK home break each other: each binds the hook socket over the
other's (a Unix socket bound on one host can't be reached from another, so hooks are lost and
sessions stay on Starting), and both write the same SQLite file. This happens when the home is on
a file system other hosts mount, like a home directory shared over NFS: every host you log in to
sees the same `~/.config/systemd/user`, so its systemd starts the enabled units too.

- **The units run only on the install host.** Both systemd units carry `ConditionHost=` with the
  host name at install time. On any other host systemd skips them: nothing starts and nothing is
  logged. Renaming the host means running `agentk install` again (or `meko extension install
  agentk`), which rewrites the units.
- **A lease backs it up** (`src/server/instance.ts`). Before `serve` opens the database or binds
  anything it takes `server.lock`, which holds the host, pid, version, port and a heartbeat it
  rewrites every 10 seconds; a lease not rewritten for 45 seconds is free. A lease from another
  host that is still beating refuses the start at once. On this host a live pid proves nothing
  (after a crash the pid may belong to another process), and an old server may still be exiting
  (an update's restart), so the start waits until the lease is released, its pid exits or it
  goes stale, and is refused only if the lease beats meanwhile. A refused start says which host
  holds the home, writes nothing to the shared log, and exits with status 75. The server unit has
  `RestartPreventExitStatus=75`, and Meko's launcher doesn't count exit 75 as a failed start of an
  update, so it can't cause a rollback. A server
  that finds another one named in the lease (it stalled for longer than 45 seconds) stops. File
  locks aren't used: they aren't reliable across NFS clients.
- **`install` refuses** while a live server on another host holds the home, since it would pin the
  units to the wrong host.

| Event | What survives | What happens |
|---|---|---|
| A phone's page goes to the background, or its network changes | Everything | The server drops the page's sockets once they stop answering its pings. When the page is shown again, it pings its events socket and opens a new one if there's no answer, so it shows the current state instead of the one it had when it was hidden. After 30 seconds or more hidden, it also lets go of its terminals and reconnects the ones on screen, even if the phone kept its timers from running. |
| Server crash or update | Everything | systemd (or launchd) restarts the server. It matches `state.db` against the session host's panes, and browsers reconnect automatically. An open page that finds the server on another version reloads itself (see [Releases and updates](releases.md)). |
| A second server starts on the same home (another host that mounts it) | Everything | The units don't start on other hosts, and a server started by hand refuses while the lease is held. If the hook socket goes missing anyway, the running server binds it again within 10 seconds. |
| Session host crash or restart | Worktrees, transcripts, state | The unit restarts the host, and the server relaunches every session with `desired: running` using `claude --resume`. |
| Host reboot or crash | Worktrees, transcripts, state (after a crash or power cut, less the last `state.db` commits before it: `synchronous=NORMAL`, see [Projects and sessions](projects-and-sessions.md)) | Both units start at boot (on macOS, when you log in) and the same recovery runs. Sessions show as *recovered* until you interact. |

```mermaid
sequenceDiagram
  participant SD as systemd user
  participant T as host unit
  participant S as server
  participant C as claude
  SD->>T: start at boot (linger)
  SD->>S: start
  S->>S: open state.db
  S->>T: list panes
  loop each session with desired = running and no pane (not one still being created, launched or stopped)
    S->>S: verify worktree, recreate from its branch (or origin/branch) if missing; fail if the branch is gone
    S->>T: spawn pane in worktree
    alt session was mid-turn
      T->>C: claude --resume session-id "continue prompt"
    else idle or waiting on you
      T->>C: claude --resume session-id
    end
    C-->>S: SessionStart hook
    S->>S: mark as recovered
  end
```

A turn that was in progress when the host died is lost, but the conversation up to that point
comes back. **Auto-continue is on by default**, with guards:

- **Only sessions that were working.** The session's status is written to `state.db` on every hook
  event, with full sync. A session whose last status was *working* is resumed with a continue
  prompt. Sessions that were idle or waiting on you come back exactly as they were.
- **Check before continuing.** The prompt says the host restarted mid-turn. It asks Claude to check
  the worktree and the last commands (and, in the Review stage, the PR threads) before continuing,
  so finished steps aren't repeated and replies aren't posted twice.
- **No crash loops.** A session is auto-continued at most once every 15 minutes. Once it has been
  auto-continued twice within an hour, the next mid-turn recovery parks it as *needs you* instead. Each project can turn
  auto-continue off.
- **Failed restarts back off.** If a session can't be restarted (for example, its worktree can't
  be recreated), it shows *exited* with the reason and is retried every 5 minutes; if its branch
  is gone, it waits for you to resume or discard it. A session host error other than "no host
  running" skips that check rather than being read as every pane being gone, and a live pane is never
  replaced.

## Moving the home into Meko's data folder

Until #236 the home was `~/.agent-k-local` (or what an install named with `AK_HOME`). It now lives
in Meko's data folder ([Where AgentK lives](projects-and-sessions.md#where-agentk-lives)), and an
older home moves there by itself (`src/server/movehome.ts`). Moving ends every session, since each
one's Claude runs in a worktree inside the home; recovery resumes them from the new place, and a
session that was mid-turn gets its continue prompt as after any host restart.

- **When.** Right after a release with the move has proved healthy, whatever the sessions are
  doing: once the release is off probation (`HomeMove`, run by the server at its start and again 5
  minutes later; a start with `MEKO_UPDATED_FROM`, which Meko's launcher sets while an update is on
  probation, waits those 5 minutes: `onProbation` in `updates.ts`), so a bad release is rolled back
  first.
  Only a release install whose home isn't in a Meko data folder yet moves (a source checkout or a
  test never does, nor a home the installer already put in one). A move that can't happen is logged
  at each start, and the install keeps running where it is: a target that isn't empty, too little
  free space for a copy, git older than 2.30, a `MEKO_HOME` on a network filesystem, another server
  holding the home's lease, or a Meko installed here whose `meko destroy -h` doesn't offer
  `--with-extensions` (it's older than the Meko that keeps extension data, so its `destroy` would
  delete the moved home; `meko update` first). Taking the target from the login shell's `MEKO_HOME`
  and `XDG_STATE_HOME` and from `[meko] home` keeps a server under its service from choosing another
  folder than the installer did.
- **Who.** The move stops the server and the session host, so it can't run in either's unit (systemd
  or launchd would end it with them). The server starts a one-shot job outside both: a transient
  unit (`systemd-run --user --unit agentk-move-home`, `Restart=on-failure`) or a LaunchAgent of its
  own (`com.yugabyte.agentk.move-home`, which removes itself at the end), running the release's
  hidden `agentk _move-home --to <dir>`. It logs to `agentk-move-home.log` beside the new home (in
  Meko's `extensions/`), which survives a copy's removal of the old home. Without services, `serve`
  does the move itself at its next start, before it opens anything, unless another server holds the
  home's lease (then the start is refused as ever).
- **What.**
  1. Stop the server, then the host (`stopAll`, which `agentk stop` uses too).
  2. Put the home in place: `rename(2)` on one filesystem; on another (a home folder on NFS moving
     to local disk), copy it to `<new>.partial` (modes, links and times kept; sockets skipped),
     rename that into place, and move the original aside to `<old>.moved`, removed once the server
     answers on the new home. Then `~/.agent-k-local` (the old path) becomes a link to the new
     home, so an interrupted move, a rollback to an older version, scripts and build outputs that
     name the old path keep working, and `git worktree prune` can't drop worktrees whose paths look
     missing.
  3. Rewrite, in order: a database kept outside the home joins it (checkpointed, with its
     snapshots; `[state] db_path` cleared); the database's paths (`sessions.worktree_path`,
     `transcript_path`, `start_images`, `worktrees.path`, `usage_files.path`, repo paths) and Claude
     Code's transcript folders, which are named after each worktree's path: `--resume` looks there,
     recovery resumes only when the transcript is found (else it starts the session over), and
     usage reading keys its offsets on the transcript's path (else it would count everything again);
     each running session gets an activity line; Claude Code's per-folder entries in
     `~/.claude.json` (copied to the new paths; the old ones stay); Kiro's sessions' `cwd`;
     `git worktree repair` in each worktree; the `~/.local/bin` links; TLS paths in `config.toml`.
     Files AgentK writes at each start or launch (the hook scripts and settings, the mod, the Kiro
     agent) need nothing.
  4. Install the services on the new home, under their new names (`dropOldNames`: the host is
     stopped), and start them.
- **Going on after a stop.** `move.json` (the journal: from, to, method, the steps done) is written
  into the home before it moves, so it goes along. Every step does nothing the second time. While
  the journal says `moving`, a server refuses to start (exit 75) and starts the job again, as after
  a reboot that ended it; without services, `serve` goes on with the move itself. A rerun finds
  what's done: a copy cut off is started over, the original untouched until the copy is whole. The
  journal is removed once the server answers on the new home (or the server removes it at its next
  start). The activity line is added once per session, and Claude Code's transcript folders are
  merged without losing a file: one the new folder has already stays in the old one.
- **Services found by folder, not spelling.** A release's home is its real path (the launcher
  resolves its folder with `cd -P`), while a unit names the home as spelled, maybe through a
  symlinked `/home`, the link the move leaves, or a bind mount that puts the same disk at another
  path; `servicesFor` (`samePath`) takes the two for the same home when they're spelled alike, have
  the same real path, or are the same folder (device and inode), so `stop`, `start`, `status`, the
  trigger and a start mid-move all find the services.
- **Meko.** Meko finds AgentK by its command, `~/.local/bin/agentk`, which the move repoints;
  `meko destroy` leaves `extensions/` alone unless asked ([Meko's install
  docs](../../../../backend/docs/install.md)).
