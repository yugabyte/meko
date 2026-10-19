# Surviving crashes, restarts and updates

Part of the [AgentK Local design](../design.md).

Two systemd user units run with lingering enabled, so they start at boot without anyone logging
in:

- `agent-k-local-host.service` runs the session host (`agent-k-local host`). It is never
  restarted by updates or reinstalls.
- `agent-k-local.service` is the web server, with `Restart=always`. Updates restart only this unit.

Keeping the session host in its own unit is the key point. If it were a child of the server's unit,
systemd would kill every Claude session on each update. The server never starts a host of its own
while the host service is installed (it reports the service as down), nor while it runs as a
service itself without the host's (it says to run `agent-k-local install`); without services (a
source checkout, `--no-service`) it starts one detached, in its own process group.

**The host keeps its version.** A running host keeps the code it started with across updates:
restarting it ends every Claude (the server's recovery resumes them). So the updater never deletes
the version directory `run/host.json` names (node-pty runs a helper from it on macOS), and the
server works with hosts of its own protocol back to `MIN_PROTOCOL`; an older one has to be
restarted (`agent-k-local host restart`), which the server's log says. `agent-k-local host stop`
ends every session in the host, and refuses while the server for this home runs: it would start
the host again at once and resume them all. With the server stopped, they resume when it starts.
`agent-k-local stop` stops both, the server first, and `agent-k-local start` starts them again.
Installed services stop through systemd (`systemctl --user stop`) or launchd (`bootout`), since
both restart a process that is only killed or exits on its own. Then it checks the server's lease:
a server outside the service (`serve` by hand, or any server without services) gets SIGTERM, but
only when the lease names this machine and its pid is an `agent-k-local serve` (after a crash the
pid can be another process's); a lease held on another host is refused. The host goes last,
through its service or its stop request. The services still start again at the next login (or
boot, with lingering) or reinstall; `start` starts them sooner.

**On macOS** the same two services are LaunchAgents in your `gui/<uid>` domain
(`~/Library/LaunchAgents/com.yugabyte.agent-k-local.plist` and `….host.plist`), with `RunAtLoad`
and `KeepAlive`. They start when you log in, not at boot: Claude Code keeps its login in the
Keychain, which is only unlocked during a desktop login, so the installer refuses to run without
one. The host runs in the foreground, as launchd wants. Both jobs set
`LANG=en_US.UTF-8`, because launchd sets no locale and Claude needs a UTF-8 one. Updates
restart the server with `launchctl kickstart -k`, or by exiting and letting `KeepAlive` bring it
back; reinstalling never reloads a loaded host job.

**Versions before the session host** ran every Claude in tmux, in a tmux service. The server
moved an install off it by itself on its first start after the update (it added the host service,
ended the sessions in tmux, and removed the tmux service once the update was final). That shipped
on 2026-10-09 and was removed about a week later. A Claude must never run in tmux and in the host at
once, so what's left is a guard: while the tmux service is installed (`legacyTmuxInstalled`), the
server refuses to start, saying to run `agent-k-local install`. An install that updated itself from
a tmux version therefore fails to start and rolls back to it (the launcher's rollback; see
[Releases](releases.md)), with its sessions still running in tmux. `agent-k-local install`, which
the installer runs, moves it: before it installs the services it removes the old ones, stopping
the old server first and then the tmux service, which ends the Claudes in it, and the new server's
recovery resumes them in the host (auto-continuing a turn that was cut off). `agent-k-local
uninstall` removes the tmux service too. Removing it ends the tmux server on its socket, which
every AgentK home on the account shared.

All of this sits behind one interface in `src/server/service.ts` (install, start, stop, restart,
state, remove, boot check, the old tmux service), with a systemd and a launchd implementation picked by platform.
`servicesFor(home)` uses the installed services only when their `AK_HOME` is exactly this home, so
a dev server or a second home never acts on another install's services.

## One server per home

Two servers on one `~/.agent-k-local` break each other: each binds the hook socket over the
other's (a Unix socket bound on one host can't be reached from another, so hooks are lost and
sessions stay on Starting), and both write the same SQLite file. This happens when the home is on
a file system other hosts mount, like a home directory shared over NFS: every host you log in to
sees the same `~/.config/systemd/user`, so its systemd starts the enabled units too.

- **The units run only on the install host.** Both systemd units carry `ConditionHost=` with the
  host name at install time. On any other host systemd skips them: nothing starts and nothing is
  logged. Renaming the host means running `agent-k-local install` again, which rewrites the units
  (the one-line installer stops early on an install it finds set up).
- **A lease backs it up** (`src/server/instance.ts`). Before `serve` opens the database or binds
  anything it takes `server.lock`, which holds the host, pid, version, port and a heartbeat it
  rewrites every 10 seconds; a lease not rewritten for 45 seconds is free. A lease from another
  host that is still beating refuses the start at once. On this host a live pid proves nothing
  (after a crash the pid may belong to another process), and an old server may still be exiting
  (an update's restart), so the start waits until the lease is released, its pid exits or it
  goes stale, and is refused only if the lease beats meanwhile. A refused start says which host
  holds the home, writes nothing to the shared log, and exits with status 75. The server unit has
  `RestartPreventExitStatus=75`, and a refused start is taken back off an update's start count, so
  it can't cause a rollback. A server
  that finds another one named in the lease (it stalled for longer than 45 seconds) stops. File
  locks aren't used: they aren't reliable across NFS clients.
- **`install` refuses** while a live server on another host holds the home, since it would pin the
  units to the wrong host.

| Event | What survives | What happens |
|---|---|---|
| A phone's page goes to the background, or its network changes | Everything | The server drops the page's sockets once they stop answering its pings. When the page is shown again, it pings its events socket and opens a new one if there's no answer, so it shows the current state instead of the one it had when it was hidden. After 30 seconds or more hidden, it also lets go of its terminals and reconnects the ones on screen, even if the phone kept its timers from running. |
| Server crash or update | Everything | systemd (or launchd) restarts the server. It matches `state.db` against the session host's panes, and browsers reconnect automatically. An open page that finds the server on another version reloads itself (see [Releases and auto-update](releases.md)). |
| A second server starts on the same home (another host that mounts it) | Everything | The units don't start on other hosts, and a server started by hand refuses while the lease is held. If the hook socket goes missing anyway, the running server binds it again within 10 seconds. |
| Session host crash or restart | Worktrees, transcripts, state | The unit restarts the host, and the server relaunches every session with `desired: running` using `claude --resume`. |
| Host reboot or crash | Worktrees, transcripts, state | Both units start at boot (on macOS, when you log in) and the same recovery runs. Sessions show as *recovered* until you interact. |

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
