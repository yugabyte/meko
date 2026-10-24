# Meko backend: install and release

Status: built in milestone 5 (#171). The design and its decisions, as approved, are in the
milestone's design; this file describes what was built and stays current with the code. The
foundation is [design.md](design.md).

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
  web_ui/dist/                 the built web UI, served at / (web-ui.md)
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
- Measured on linux-x64 for 0.20261022.0, before the reranker came (#170): the tarball was 118 MB;
  unpacked, Node 117 MB, the memory engine 194 MB (onnxruntime-node 45 MB), the embedder 24 MB and
  `meko` 24 MB. The reranker adds about 23 MB.
- `scripts/smoke-release.sh <tarball> <version>` unpacks a tarball, checks `meko version --json`,
  starts it on a fresh data folder, waits for the memory engine, saves a memory, finds it, checks that
  nothing was downloaded into `models/`, and stops.

Linux tarballs are built in `almalinux:8` (glibc 2.28, so they run on RHEL 8 and newer) with
gcc-toolset-13; the Mac one on `macos-15` with `MACOSX_DEPLOYMENT_TARGET=14.0`. Nothing is signed or
notarized, as with yugabyted's macOS tarball: files that `gh` or `curl` download carry no quarantine
flag, and Go signs darwin/arm64 binaries ad hoc.

## Releases

`.github/workflows/meko-release.yml` runs on pushes to `main` that change `backend/**`,
`scripts/release/**` or itself, and by hand (force, or rebuild a `meko-v` tag). It uses the repo's
release scripts with `--product meko`:

| | AgentK (`agentk-release.yml`) | Meko (`meko-release.yml`) |
|---|---|---|
| Tag | `agentk-v0.YYYYMMDD.N` (`v0.…` before) | `meko-v0.YYYYMMDD.N` |
| Title | its tag | its tag |
| Releases on | feat/fix/perf/deps subjects other than `meko` | feat/fix/perf/deps subjects with component `meko` |
| Assets | AgentK tarballs, `SHA256SUMS`, its `install.sh` | Meko tarballs, `SHA256SUMS`, Meko's `install.sh` |
| Latest | `--latest=false` | `--latest` |
| Pruning | newest 5 per lane of its own tags | newest 5 `meko-v` releases |

Plan, then `make test`, then the three builds (each packaged and smoke-tested), then publish:
`SHA256SUMS`, the release with notes from `release-notes.sh --product meko`, provenance attestation,
and pruning. Installed AgentK updaters never pick a Meko release: they read only AgentK's tags and
need an `agentk-*` tarball (`agent-k-local-*` before the rename to AgentK).

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
`serve` and signal it, as before.

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
both test suites read `internal/paths/testdata/resolve-cases.json`).

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

- `meko install_extension agentk` needs a running Meko. When `agentk` (or `agent-k-local`, its name
  before the rename, which an install from then may have alone: `Catalog.OldCommands`) is on PATH or
  in `~/.local/bin`, it runs `<command> install --meko-url <url> --meko-key <client token>`, which
  keeps AgentK's other settings. Otherwise it downloads the newest AgentK release's `install.sh`
  (tags `agentk-v*`, and `v*` from before) and runs it with the same two flags. An
  AgentK from before `--meko-url` is refused with a message, rather than failing on the flag: an
  installed one by its `install.js`, a release by its `install.sh`. So `install_extension` needs
  an AgentK release from after #229, which itself is a `meko` commit and cut no AgentK release.
  Nothing else is passed, apart from `--non-interactive` without a terminal: AgentK asks its own questions. Then it registers
  `agentk`.
- `meko start` starts the extensions after Meko; `meko stop` stops them first (`agentk stop`
  ends AgentK's sessions too; they resume on start), unless given `--keep-extensions`. An
  extension that fails is reported, and the rest go on. `meko restart`, updates and crashes never
  touch them: AgentK has its own services and updater.
- `meko uninstall_extension agentk` only unregisters it.
- AgentK's key is Meko's client token until per-client keys (#195).

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
  (default true); a server started by hand only reports updates, and `meko update` installs one now.
  Staging downloads the tarball and `SHA256SUMS` beside the installed versions, checks the checksum,
  unpacks it (refusing entries or links outside its folder), and runs the new
  `bin/meko version --json`, which must name the version: a binary that can't run here never
  becomes current. Then `update-pending.json` records `{from, to, schema, attempts}`, `app/current`
  is repointed atomically, and `serve` exits 0 for the service manager to start the new version.
- **Probation**: each start of the pending version counts an attempt (a clean stop takes it back);
  5 minutes up finishes the update and prunes versions older than the one it replaced.
- **Rollback**: the 4th start of a version that failed 3 times puts back the previous one. If the
  new version migrated the database, `store.RestoreBackup` moves the upgraded file and its WAL aside
  to `meko.db.failed-<schema>` and copies back the `meko.db.bak-<schema>` the migration made; writes
  since the upgrade are only in the file moved aside. Then `app/current` goes back, the version goes
  into `bad-versions`, a WARN goes to `meko.log` and `service.log`, and the old binary is `exec`ed in
  its place.
- While an update is on probation, deleting a knowledge file leaves its bytes on disk
  (`memory.Options.KeepDeletedFiles`): a rollback restores rows from before the update, and they
  must still find their files. The sweep at a start after the update is done removes them.
- `[update] channel = "off"` stops checks; `[update] repo` (or `MEKO_UPDATE_REPO`) points at a fork.

## Secrets

`internal/secrets` keeps the client token now, and provider keys (#190, #191) and connector tokens
(#194) later. `[secrets] store` (`MEKO_SECRETS`) picks the store:

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
  probation and pruning.
- `internal/store`: `RestoreBackup` undoes a real upgrade.
- `internal/cli`: `install` with a fake service manager, start, stop and restart through it,
  extensions run on start and stop but not restart or `--keep-extensions`, `uninstall` and
  `--purge`, `install_extension` with an installed `agent-k-local` (an AgentK from before the rename) and from a fake release,
  `logs` merging and following across rotation, `token --json`, `version --json`.
- `scripts/install_test.go`: the installer with a fake `gh`: unpacking and running `meko install`,
  "already installed", `--reinstall`, a bad checksum, AgentK's flags refused, and Intel Macs and
  macOS 13 refused.
- The memory engine: offline loading never downloads, for the embedder and the reranker.
- Each release build runs `scripts/smoke-release.sh` on its own platform.

Not covered automatically: a real launchd or systemd service (`meko install` writes the user's own
service), a real update from GitHub, and the Keychain under launchd. They're in the milestone's
manual test plan: a fresh Mac and Linux machine, a reboot, an update, a forced rollback and
`meko stop` with AgentK.
