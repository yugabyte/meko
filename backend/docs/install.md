# Meko backend: install and release

People install Meko with one command on macOS 14+ (Apple Silicon) and Linux (x64, arm64). It runs
as a login service, restarts on failure, updates itself and rolls back an update that keeps failing.
`meko install_extension agentk` then installs AgentK pointed at this Meko, so a fresh machine
gets a working datapack with no account. `meko stop` stops AgentK too. If Meko fails, AgentK keeps
working, its Meko features skipping or retrying.

## Install

```sh
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
meko install_extension agentk      # then, for AgentK
```

Meko's releases are the repo's "latest" (AgentK's are published `--latest=false`), so that is Meko's
`install.sh` (`backend/scripts/install.sh`). It needs a signed-in `gh` or `GITHUB_TOKEN` while the
repo is internal, and:

1. Checks the platform: Linux x64 or arm64, or an Apple Silicon Mac on macOS 14 or newer.
2. Finds the newest `meko-v*` release (or `--version`), downloads
   `meko-<version>-<linux|darwin>-<x64|arm64>.tar.gz` and `SHA256SUMS`, and checks the checksum.
3. Unpacks it to `~/.local/share/meko/app/<version>` (`$XDG_DATA_HOME/meko/app`), outside the data
   folder, clearing macOS's quarantine flag in
   case the tarball came through a browser.
4. Runs that version's `meko install`.

It takes only its own flags (`--version`, `--reinstall`, `--no-service`, `--non-interactive`) and
refuses any other, naming `meko install_extension agentk`. Run again where Meko is installed, it
says so and changes nothing, unless given `--reinstall` or another version.

`meko install` (`internal/cli/install.go`):

- In a release install it points `app/current` at its own version and links `~/.local/bin/meko` to
  `app/current/bin/meko`. A build from source is set up as it is and doesn't update itself.
- It makes the client token (in the secret store) and sets up the login service (below), stopping
  a server started by hand on the same data folder first. `--no-service` skips the service.
- It writes the web listener's access to `web.toml` when neither it nor `config.toml` has a `[web]`
  table: `--access local|remote`, or by AgentK's old rule (remote over SSH or on Linux with no
  display, else local), unless AgentK is on this computer, whose access the handoff takes over
  instead ([web.md](web.md#settings)). It prints the web UI's URL.
- It installs Meko alone. AgentK is `meko install_extension agentk`.

`meko uninstall` removes the service, `~/.local/bin/meko` and the app folder, and keeps the data;
`--purge` first destroys the data folder as `meko destroy` does (it asks, unless `--force`; it
takes `--with-extensions` too). Extensions stay installed unless that destroy removed them, and it
names the ones that stay. `meko destroy` stops the service before the server, so its manager
doesn't start the server again mid-removal; removes the data folder and the client token (also when
it's in the Keychain, outside the folder); and says the service is still installed. The releases
are outside the data folder, so the service can still start Meko on a fresh one.

## Release builds

`backend/scripts/package-release.sh <version> <arch> <node-dir> <out-dir>`, run on the platform
the tarball is for after `make build VERSION=<version>`, `make memory_engine`, `make web_ui` and
`(cd memory_engine && npm prune --omit=dev)`:

```
meko-<version>/
  bin/meko                     the CLI and server (cgo: SQLite and sqlite-vec compiled in)
  node/bin/node, node/LICENSE  Meko's own Node (22.18.0, checked against nodejs.org's SHASUMS256)
  memory_engine/package.json, dist/, node_modules/, stubs/
  web_ui/dist/                 the built web UI, served at / on the web listener (web-ui.md)
  models/                      the pinned models: the embedder and the reranker
  VERSION, LICENSE
```

- `memory_engine/node_modules` is trimmed to the platform: onnxruntime-node keeps only its own
  `bin/napi-v*/<os>/<arch>` and no CUDA or TensorRT providers, `onnxruntime-web` goes (the Node
  build of transformers.js bundles what it needs of it), and so do sharp's WebAssembly and musl
  builds. `stubs/` is there because npm links `file:` dependencies into `node_modules`.
- `make memory_engine` installs with `ONNXRUNTIME_NODE_INSTALL=skip`: onnxruntime-node's install script
  would otherwise download a 270 MB CUDA provider on linux-x64, which also timed out on CI runners.
- `models/` is fetched by the tarball's own Node through the trimmed memory engine
  (`memory_engine/scripts/fetch-model.mjs`): the embedder (all-MiniLM-L6-v2) and the knowledge
  base's reranker (ms-marco-MiniLM-L-6-v2), 47 MB together. The script runs each one, then loads
  it again with remote models off, so a folder that wouldn't work offline fails the build. It's
  transformers.js's cache layout for the pinned revisions, plus copies of each model's JSON files at
  the unrevisioned path: transformers.js 4.3 looks for a model's tokenizer files at the `main`
  revision whatever revision it loads (`pipeline()` for the embedder, `AutoTokenizer` for the
  reranker), so offline it would find none.
- `serve` finds `<bin>/../models` (`memoryengine.FindModels`) and passes it to the memory engine with
  `MEKO_MODELS_OFFLINE=1`, which sets transformers.js's `allowRemoteModels = false` in both
  workers: a release never downloads a model, and the first start works offline. A build from source still downloads into
  the data folder's `models/`.
- Measured on linux-x64 without the reranker: the tarball was 118 MB;
  unpacked, Node 117 MB, the memory engine 194 MB (onnxruntime-node 45 MB), the embedder 24 MB and
  `meko` 24 MB. The reranker adds about 23 MB.
- `scripts/smoke-release.sh <tarball> <version>` unpacks a tarball, checks `meko version --json`,
  starts it on a fresh data folder, waits for the memory engine, saves a memory, finds it, checks that
  nothing was downloaded into `models/`, checks that the web listener (on the port after the API
  listener's, never 7777) serves the web UI at the address `meko ui --no-open` prints, and stops.

Linux tarballs are built in `almalinux:8` (glibc 2.28, so they run on RHEL 8 and newer) with
gcc-toolset-13; the Mac one on `macos-15` with `MACOSX_DEPLOYMENT_TARGET=14.0`. Nothing is signed or
notarized, as with yugabyted's macOS tarball: files that `gh` or `curl` download carry no quarantine
flag, and Go signs darwin/arm64 binaries ad hoc.

## Releases

`.github/workflows/meko-release.yml` runs on pushes to `main` that change `backend/**`,
`scripts/release/**` or itself, and by hand (force, or rebuild a `meko-v` tag). It uses the repo's
release scripts with `--product meko`:

| | AgentK (`agentk-release.yml`) | Meko (`meko-release.yml`) | Presenter (`presenter-release.yml`) |
|---|---|---|---|
| Tag | `agentk-v0.YYYYMMDD.N` (`v0.…` before) | `meko-v0.YYYYMMDD.N` | `presenter-v0.YYYYMMDD.N` |
| Title | its tag | its tag | its tag |
| Releases on | feat/fix/perf/deps subjects other than those naming only `meko` or `presenter` | feat/fix/perf/deps subjects naming `meko`, alone or in a list (`feat(agentk,meko): …`) | feat/fix/perf/deps subjects naming `presenter`, alone or in a list |
| Assets | AgentK tarballs, `SHA256SUMS`, and a stub `install.sh` that tells an older Meko, which looks for it, to update first | Meko tarballs, `SHA256SUMS`, Meko's `install.sh` | `presenter-<version>-<os>-<arch>.tar.gz` (holding `bin/meko_presenter`), `SHA256SUMS`; no installer: Meko installs them |
| Latest | `--latest=false` | `--latest` | `--latest=false` |
| Pruning | newest 5 per lane of its own tags | newest 5 `meko-v` releases | newest 5 `presenter-v` releases |

Plan, then `make test`, then the three builds (each packaged and smoke-tested), then publish:
`SHA256SUMS`, the release with notes from `release-notes.sh --product meko`, provenance attestation,
and pruning. Meko picks each product's releases by its own tag prefixes and tarball names, so an
AgentK or Presenter release is never taken for Meko's, nor the other way round.

## The service

`internal/service` writes the service file from an embedded template, and runs it with launchctl
(`bootstrap`, `bootout`, `kickstart`) or `systemctl --user`:

| | launchd (`~/Library/LaunchAgents/com.yugabyte.meko.plist`) | systemd (`~/.config/systemd/user/meko.service`) |
|---|---|---|
| Runs | `<app>/current/bin/meko serve --detached --data-dir <data>`, with `MEKO_SERVICE` set | the same |
| At login | `RunAtLoad` | `WantedBy=default.target`, and lingering turned on where it can be |
| Restart | `KeepAlive`, `ThrottleInterval 5` | `Restart=always`, `RestartSec=2`, `RestartPreventExitStatus=75` |
| Stop | `ExitTimeOut 35` | `TimeoutStopSec=35`, `KillMode=mixed` |
| stderr | `<data>/logs/service.log` | the same (`StandardError=append:`) |
| Shared home | | `ConditionHost=<host>` |

- When another server holds the data folder, `serve` exits 75, and the systemd unit doesn't restart
  on it. launchd can't skip one exit code (`KeepAlive` has to restart on 0, which is how an update
  restarts), so a `serve` launchd runs waits for the folder instead, saying so once in
  `service.log`.
- `meko start --port` and `meko restart --log-level` are refused while a service runs the data
  folder: the service passes only `--data-dir`, so set them in `config.toml`.
- The stop timeouts are 35 s because `serve` drains requests for up to 10 s, then stops its
  children for up to 15 s more; launchd's default of 20 s would kill it partway.
- The data folder is written into the service file, so the CLI uses a service only when the
  service's `--data-dir` is the folder it's working on (`serviceOf`). A meko on another data folder
  (a test's, say) never touches it.

With the service installed, `meko start` starts it, `meko stop` stops it (a SIGTERM alone would
only see it started again), and `meko restart` restarts it. Without it, they start a detached
`serve` and signal it.

`meko logs [-f] [-n 50] [--server|--memory-engine|--service] [--json]` shows the last lines of
`meko.log` and `memory_engine.log` merged by time, each prefixed with its source, rendering JSON lines as
`time LEVEL msg key=value`. `-f` follows them and reopens a file that shrinks or is replaced
(rotated).

## Extensions

`<data>/extensions.json` (0600) lists the extensions this Meko runs, with the command that controls
each one. The contract is the extension's own CLI: `<command> start`, `stop` and `status`, and
its uninstall (`Catalog.UninstallArgs`; AgentK's is `uninstall --purge --delete-worktrees
--non-interactive`) for `meko destroy --with-extensions`. Every command Meko runs of an extension
(those, its installer and `agentk install`) gets `MEKO_HOME` naming Meko's data folder, so the
extension knows where its own folder is: `<data>/extensions/<name>` (`extensions.Dir`). AgentK's
home is `<data>/extensions/agentk`, wherever Meko's rule puts the data folder (`MEKO_HOME`,
`~/.meko`, or the network-home fallback; AgentK runs the same rule when Meko didn't tell it, and
AgentK's and Presenter's tests read `internal/paths/testdata/resolve-cases.json` as Meko's do). An AgentK whose home is in
another data folder refuses `start`, `stop`, `status`, `uninstall` and `_handoff` run with this one's
`MEKO_HOME`, and does nothing.

- An extension's command is found on PATH, then in `~/.local/bin`, then in its release install's
  `app/current/bin`: its folder in the data folder, then its home from before it moved there
  (`Catalog.OldHomes`, `~/.agent-k-local`). When a registered command is gone, `meko start` and
  `stop` look for it again this way and save what they find in the registry.
- `meko destroy` keeps the extensions' data unless told otherwise. With some there (a registered
  extension, or a known one's folder), it lists each one's folder and size (Meko's own size leaves
  out `extensions/`) and asks a second question, whether to uninstall the extensions and delete
  their data too (AgentK's sessions and worktrees, with any uncommitted work). `--force` asks
  nothing and keeps them; `--with-extensions` removes them without the second question (with
  `--force`, asking nothing). Removing runs each extension's uninstall first; if one fails,
  nothing of Meko's is removed. Keeping stops Meko only, never the extensions, and moves
  every entry but `extensions/` and `extensions.json` aside while holding the lock (below), so
  the next `meko start` still starts them; their Meko key (the old client token) stops working, so
  it says to run `meko install_extension agentk` after `meko start`. A data folder with extension
  data and no `meko.db` (AgentK installed without Meko) has nothing of Meko's to remove; with
  `--with-extensions` the extensions are uninstalled, their data removed, and the folder too if
  that empties it.

- The catalog (`extensions.Known`) has AgentK and Presenter, both `Managed`: Meko installs and
  updates them from their own releases, and neither ships an installer. Their commands are `agentk`
  and `meko_presenter`; the extensions, their folders and tarballs are `agentk` and `presenter`
  (tags `agentk-v*`, and `v*` from before releases were tagged by product; `presenter-v*`).
- `meko install_extension <name>` needs a running Meko. When the extension's command is on PATH or
  in `~/.local/bin` (for AgentK also `agent-k-local`, its name before the rename, and the
  `app/current/bin` of its old home `~/.agent-k-local`: `Catalog.OldCommands`, `OldHomes`), it's used
  as it is, unless `--version` names a release, which is installed over it. Otherwise, or then,
  Meko picks the newest release with this platform's `<name>-<version>-<os>-<arch>.tar.gz` and
  `SHA256SUMS` (skipping its `bad-versions`), or the one `--version` names (even one rolled back),
  and installs it as Meko installs itself (`update.App`): checked against
  `SHA256SUMS`, unpacked, `bin/<command> version --json` checked to name the version (an AgentK
  release from before `--json` prints the bare version, which counts) and the catalog's `CheckArgs`
  run (AgentK's `selftest`), into `<data>/extensions/<name>/app/<version>` (owner-only,
  `App.Private`: folders and programs 0700, files 0600, as everything in the data folder), with
  `app/current` and `~/.local/bin/<command>` pointing at it. Without `GITHUB_TOKEN` or a signed-in
  `gh`, listing the releases of the internal repo fails, and it says to sign in. Then it runs
  `<command> install --meko-url <url> --meko-key <client token>` on the terminal, with
  `--non-interactive` without one, and whatever followed `--` on the command line (AgentK's
  `--agent claude|opencode|kiro`, `--install-deps`, `--no-install-deps`). AgentK's install checks
  its prerequisites (git, curl, gh, a coding agent, Meko's setup for the agents) with the
  `deps.sh` its release carries, asking before it installs any, and sets up its services. An AgentK
  from before `--meko-url` is refused with a message, rather than failing on the flag. Then it
  registers the extension. One `~/.local/bin/<command>` serves all of a user's data folders, but a
  release belongs to one: a link that runs another data folder's release isn't this one's
  (`Catalog.Find` skips it), so Meko installs this folder's own, leaves the link alone
  (`App.MayLink`) and registers `app/current/bin/<command>`. Uninstalls are
  `agentk uninstall --purge --delete-worktrees --non-interactive` and
  `meko_presenter uninstall --purge --non-interactive`, which remove the extension's folder,
  releases and all (`meko destroy --with-extensions` then removes the `~/.local/bin` link Meko made):
  Presenter's decks stay in their Meko datapacks.
- `meko update_extension <name>` (and the Extensions page's **Update**) moves a release install Meko
  made to the newest release, and `--all` does so for every installed extension, going on past one
  that fails (the exit code says whether any did). `--check` only looks, and with no release for
  this platform (or only rolled-back ones) it says so. `--auto=on|off` sets the extension's
  **Update automatically** and updates nothing. A command that isn't Meko's release install (a build
  from source) is refused, and so is an AgentK still in its old home, which Meko updates once it has
  moved into the data folder (it moves itself at its next start). The update is
  `extensions.Update`, the one place every update of an extension goes through:
  - The version to roll back to is the current one, unless that's still on probation (its own
    update's record names it): then it's the one that update replaced, the last known to work.
  - It deletes the other versions, except any in use (below), and stages the new one beside them.
  - With the catalog's `RestartArgs` (AgentK's `agentk restart`, which restarts only its server, so
    its session host and every session in it keep running), it switches `app/current` while the
    extension runs, with `update-pending.json` written (`App.Switch`), then runs them; they return
    once the extension answers. Without (Presenter), it stops the extension, switches, and starts
    it again, up to four times; a switch that fails starts the old version again.
  - If `app/current` then names the old version, `meko _launch` rolled the new one back, and the
    update fails saying so, and whether the old version started.
  - It needs no running Meko, and holds the update lock (below).
- **Probation for extensions** is Meko's, through `meko _launch <name> <launcher> <args>`. An
  extension's launcher (its release's `bin/<command>`) hands its long-running commands to it when
  `meko _launch --supported` succeeds (it finds `meko` on PATH, in `~/.local/bin` or in Meko's
  `app/current/bin`), and runs them itself otherwise, saying a failed update won't roll back. Meko
  sets `MEKO_LAUNCHED=1`, so the launcher it runs doesn't hand over again. For a command in the
  catalog's `Probation` (`serve`) it applies Meko's own rules (`App.Started`, `RollBack`, `Uncount`,
  `Healthy`): it counts each start of the version on probation, rolls back on the start after three
  failed ones (`app/current` back, the version into `bad-versions`, the old version run instead),
  runs the launcher as its child with signals passed on and `MEKO_UPDATED_FROM` set while on
  probation, takes a clean end off the count (exit 0, a `CleanExits` code such as AgentK's 75,
  or a stop it passed on), and after 5 minutes clears the record and prunes. Other commands
  (AgentK's `host`) it execs in its own place. Either way it holds a shared `flock` on
  `<app>/<version>/.in-use` while the process runs (`App.LockInUse`), and pruning skips a version
  whose lock is held (`App.InUse`): AgentK's session host keeps running across server updates, from
  the version it started with, and node-pty needs that folder. A host started before `_launch`
  existed, or by AgentK's server without services (`node cli.js host`, not through the launcher),
  holds no lock, so pruning also keeps the version a live process names in the catalog's
  `RunningRecords` (AgentK's `run/host.json`: `pid` alive, `appDir` or `version`; `App.Running`). A launcher outside an app folder (a
  build from source) is just run. AgentK's own updater once kept `update-pending.json` and
  `bad-versions` in its home: Meko moves such a record into `app/` and counts those bad versions too
  (`App.Legacy`), and adds rolled-back versions to both lists, which a version from before still
  reads.
- **Checks and automatic updates.** The checks that look for Meko's releases ([below](#updates-and-rollback))
  find the extensions' too, from the same list (`Updater.Extensions`, `extensions.Check`), and
  record them in `update.json` under `extensions`; a Meko built from source runs checks for the
  extensions alone (`Updater.ExtensionsOnly`). After a scheduled check that doesn't restart into a new
  Meko, each extension whose **Update automatically** is on (`auto_update` in `extensions.json`;
  missing is on, unless AgentK's own `config.toml` still has its old `[update] channel = "off"`,
  which Meko then saves as off: `Catalog.LegacyUpdatesOff`) and that has a newer release is updated as its Extensions-page job, marked
  `auto`. One whose last update is still on probation waits, and so does one that says it's busy:
  the catalog's `BusyArgs` (Presenter's `meko_presenter _busy`, true while a talk is live) exit 0,
  within 15 s. It's tried again at the next check. Updates someone asks for never wait. When Meko
  installs its own update, it restarts first, and the new Meko's first check, a minute later,
  updates the extensions (a Meko on probation still checks; it only installs no update of its
  own until the probation ends): the same order as `meko update`, which updates Meko, then runs the new
  `meko update_extension --all`. `[update] channel = "off"` stops all of it; `[update] auto` is
  Meko's own.
- **The update lock.** Every update, Meko's and the extensions', scheduled or asked for, holds the
  data folder's update lock (`update.Lock`): a mutex in `serve` and a `flock` on `run/update.lock`,
  which `meko update`, `update_extension` and `install_extension` take in their own processes. A
  second update while one runs is refused (the Status page's `UPDATE_BUSY`, "another update is
  running" on the command line), and a scheduled check skips its turn.
- A command npm installed (`npm link`, `npm install -g`: Presenter from source) is a
  `#!/usr/bin/env node` script in the bin folder of the Node that installed it, and the login
  service's PATH has no such Node (launchd's is `/usr/bin:/bin:/usr/sbin:/sbin`), or an older one.
  So when an extension's command sits in a folder that holds a `node`, Meko runs it (`start`,
  `stop`, `status`, `_handoff`, its uninstall, and its `install` from `install_extension` or the
  Extensions page) with that folder first on PATH (`extensions.CommandEnv`).
- An extension whose catalog entry has `WebUI` (AgentK, Presenter) is served to browsers by the web listener at
  `/ext/<name>/`, on the unix socket `<command> _handoff --json` names; at every start, that handoff
  also brings over the web settings, certificate and login the extension had when it served
  browsers itself ([web.md](web.md#the-handoff-from-agentk)). AgentK listens only on its socket.
- `meko start` starts the extensions after Meko; `meko stop` stops them first (`agentk stop`
  ends AgentK's sessions too; they resume on start), unless given `--keep-extensions`. An
  extension that fails is reported, and the rest go on. `meko restart`, Meko's updates and crashes
  never touch them: they have their own services (AgentK) or processes (Presenter), and an
  extension restarts only for its own update.
- `meko uninstall_extension agentk` only unregisters it.
- The web UI's Extensions page runs the same install and update, the extensions' `start` and
  `stop`, Check now and Update automatically, from the server, without a terminal
  ([web.md](web.md)). AgentK's own page shows its update and its switch through the same routes.
- AgentK's key is Meko's client token, as every client's is ([api.md](api.md#auth)). The Meko Cloud
  link is Meko's too: extensions read it from `meko token --json` and hear of a change on their
  socket ([cloud.md](cloud.md)).

## Updates and rollback

A release install's app folder:

```
~/.local/share/meko/app/     ($XDG_DATA_HOME/meko/app), outside the data folder
  <version>/                 one per installed release, each with bin/meko and VERSION
  current -> <version>       what the service runs
  update-pending.json        an update on probation
  bad-versions               versions rolled back; never offered again
```

`internal/update`:

- **Checks** run inside `serve` a minute after start, then every `[update] interval_hours` (6). The
  client lists the repo's newest 100 releases through GitHub's API with `GITHUB_TOKEN` or `gh auth
  token` (looked for in Homebrew's folders and `~/.local/bin` too, since a service starts with a
  short PATH), and picks the newest stable `meko-v*` release with this platform's tarball and
  `SHA256SUMS` that isn't in `bad-versions`. What it found goes to `<data>/update.json`, which
  `meko status` shows.
- **Applying** happens on its own only under the service (`MEKO_SERVICE`) with `[update] auto`
  (default true); a server started by hand only reports updates. `meko update` installs one now, and
  so does **Update** on the web UI's Status page, through the running server's own updater
  ([web.md](web.md)): under the service, `serve` exits 0 as for an automatic update; started by
  hand, it execs the new `app/current/bin/meko` in its own place once it has stopped.
  Staging downloads the tarball and `SHA256SUMS` beside the installed versions, checks the checksum,
  unpacks it (refusing entries or links outside its folder), and runs the new
  `bin/meko version --json`, which must name the version: a binary that can't run here never
  becomes current. Then `update-pending.json` records `{from, to, schema, attempts}`, `app/current`
  is repointed atomically, and `serve` exits 0 for the service manager to start the new version.
- **Probation**: each start of the pending version counts an attempt (a clean stop takes it back);
  5 minutes up finishes the update and prunes versions older than the one it replaced. Until then
  the scheduled checks still run, and install the extensions' updates, but no update of Meko's.
- **Rollback**: the 4th start of a version that failed 3 times puts back the previous one. If the
  new version migrated the database, `store.RestoreBackup` moves the upgraded file and its WAL aside
  to `meko.db.failed-<schema>` and copies back the `meko.db.bak-<schema>` the migration made; writes
  since the upgrade are only in the file moved aside. Then `app/current` goes back, the version goes
  into `bad-versions`, a WARN goes to `meko.log` and `service.log`, and the old binary is `exec`ed in
  its place.
- While an update is on probation, deleting a knowledge file leaves its bytes on disk
  (`memory.Options.KeepDeletedFiles`): a rollback restores rows from before the update, and they
  must still find their files. The sweep at a start after the update is done removes them.
- `[update] channel = "off"` stops the scheduled checks (Check now and Update on the Status page still work, as `meko update` does); `[update] repo` (or `MEKO_UPDATE_REPO`) points at a fork.

## Secrets

`internal/secrets` keeps the client token and the Meko Cloud link's key (`meko_cloud_key`, [cloud.md](cloud.md)), and is where provider keys and connector tokens will go. `[secrets] store` (`MEKO_SECRETS`) picks the store:

- `keychain`, the default on macOS: generic passwords in the login Keychain, service
  `com.yugabyte.meko`, account `<data folder>/<name>`, through `/usr/bin/security`. The value is
  written through `security -i` on stdin, never as an argument, and read back to check it, since
  `security -i` doesn't report a failed command in its exit status. The item's access list names
  Apple's `security`, which never changes, so an updated, unsigned `meko` meets no Keychain prompt.
  The trade-off: any process of the user can read it through `security`, as it could a 0600 file,
  but it's encrypted at rest and locked while the user is logged out.
- `file`, the default elsewhere: `<data>/secrets/<name>`, 0600 in a 0700 folder. Secret Service is
  missing on most servers, and locked at boot when lingering starts Meko before login.

The client token stays in `client.token` with the file store. With the Keychain, an existing
`client.token` moves into it the first time it's read, and a new token is added without `-U`
(`Keychain.Add`), which refuses an item that's already there: `serve` and `meko token` making one at
once agree on the first, as the file store's hard link makes them. A Keychain that refuses (locked, as over SSH)
makes `serve` wait and retry every 10 s instead of exiting, so launchd doesn't restart it in a loop.
Changing the store doesn't move secrets: the client token is made again.

## Tests

- `internal/service`: unit and plist rendering (quoting, escaping, the data folder read back), and
  the launchctl and systemctl calls through a fake runner.
- `internal/secrets`: the file store's permissions; the Keychain store against a fake `security`
  that logs its arguments (the secret never appears), its error message, and on macOS a real
  Keychain file of the test's own.
- `internal/update`: release picking, staging against a fake GitHub API, checksum and binary
  checks, unsafe tarballs, switching, three failed starts then rollback, a clean stop not counting,
  probation and pruning; the web UI's install (restarting once switched, nothing newer, a failed
  one) and its lock against the scheduled checks.
- `internal/store`: `RestoreBackup` undoes a real upgrade.
- `internal/cli`: `install` with a fake service manager, start, stop and restart through it,
  extensions run on start and stop but not restart or `--keep-extensions`, `uninstall` and
  `--purge`, `install_extension` with an installed `agent-k-local` (an AgentK from before the rename) and from a fake release,
  `logs` merging and following across rotation, `token --json`, `version --json`, and an update
  from the Status page's route ending `serve` with an exec of the new version, or under the
  service a clean exit.
- `scripts/install_test.go`: the installer with a fake `gh`: unpacking and running `meko install`,
  "already installed", `--reinstall`, a bad checksum, AgentK's flags refused, and Intel Macs and
  macOS 13 refused.
- The memory engine: offline loading never downloads, for the embedder and the reranker.
- Each release build runs `scripts/smoke-release.sh` on its own platform.

Not covered automatically: a real launchd or systemd service (`meko install` writes the user's own
service), a real update from GitHub, and the Keychain under launchd. Check them by hand
on a fresh Mac and Linux machine: a reboot, an update (from `meko update` and from the Status
page, with and without the service), a forced rollback and `meko stop` with AgentK.
