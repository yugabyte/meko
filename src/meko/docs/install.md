# Meko backend: install and release

People install Meko with one command on macOS 14+ (Apple Silicon) and Linux (x64, arm64). It runs
as a login service, restarts on failure, updates itself and rolls back an update that keeps failing.
`meko extension install agentk` then installs AgentK pointed at this Meko, so a fresh machine
gets a working datapack with no account. `meko stop` stops AgentK too. If Meko fails, AgentK keeps
working, its Meko features skipping or retrying.

## Install

```sh
curl -fsSL https://mekodata.ai/install.sh | sh
meko extension install agentk      # then, for AgentK
```

`mekodata.ai/install.sh` is a 302 redirect, set on the website, to
`https://github.com/yugabyte/meko/releases/latest/download/install.sh`. Meko's releases are the
repo's "latest" (AgentK's and Presenter's are published `--latest=false`), so that is the newest
Meko release's `install.sh` (`src/meko/scripts/install.sh`), and a release needs no change on the
website. It's a 302, not a 301, so no cache keeps it for good. The link is `mekodata.ai`:
`www.mekodata.ai` redirects elsewhere.

The installer downloads with a signed-in `gh`, else with curl and `GITHUB_TOKEN`, else with curl
and no token. With no token it reads the newest version from where
`github.com/yugabyte/meko/releases/latest` redirects rather than from GitHub's API, which allows 60
calls an hour per address without one. Everything runs from a `main` function called on the
script's last line, so a download cut short runs nothing and `meko install` doesn't read the rest
of the script as its input. It:

1. Checks the platform: Linux x64 or arm64, or an Apple Silicon Mac on macOS 14 or newer.
2. Finds the newest `meko-v*` release (or `--version`), downloads
   `meko-<version>-<linux|darwin>-<x64|arm64>.tar.gz` and `SHA256SUMS`, and checks the checksum.
3. Unpacks it to `~/.local/share/meko/app/<version>` (`$XDG_DATA_HOME/meko/app`), outside the data
   folder, clearing macOS's quarantine flag in
   case the tarball came through a browser.
4. Runs that version's `meko install`.

It takes only its own flags (`--version`, `--reinstall`, `--no-service`, `--non-interactive`) and
refuses any other, naming `meko extension install agentk`. Run again where Meko is installed, it
says so and changes nothing, unless given `--reinstall` or another version.

`meko install` (`internal/cli/install.go`):

- In a release install it points `app/current` at its own version and links `~/.local/bin/meko` and
  `~/.local/bin/meko-cli` to `app/current/bin/meko` (the binary runs `meko-cli`'s commands when run by
  that name). A build from source is set up as it is and doesn't update itself. Since updates
  don't run `meko install`, a release's `serve` also makes `~/.local/bin/meko-cli` (`linkMekoCLI`)
  when this install's `~/.local/bin/meko` is there and nothing is at `~/.local/bin/meko-cli`, but
  only once the release is past probation: at a start with no update pending, or when the update
  is done ([below](#updates-and-rollback)). A release rolled back after making it would leave
  `meko-cli` pointing at an older `meko` that doesn't know the name. Run as `mekosh`, the binary runs nothing
  and names the `meko-cli` command instead (exit 2), so a `~/.local/bin/mekosh` link stays.
- It makes the client token (in the secret store) and sets up the login service (below), stopping
  a server started by hand on the same data folder first. `--no-service` skips the service.
- It writes the web listener's access to `web.toml` when neither it nor `config.toml` sets one:
  `--access local|remote`, or by AgentK's old rule (remote over SSH or on Linux with no display,
  else local), unless AgentK is on this computer, whose access the handoff takes over instead
  ([web.md](web.md#settings)). Neither access has a login. `--no-ui` turns the web UI off there
  too (`[web] enabled = false`; `meko ui start` turns it on). It prints the web UI's URL.
- It installs Meko alone. AgentK is `meko extension install agentk`.

`meko uninstall` removes the service, `~/.local/bin/meko`, `~/.local/bin/meko-cli`,
`~/.local/bin/mekosh` (when they're links into the app folder) and the app folder,
and keeps the data;
`--purge` first destroys the data folder as `meko destroy` does (it asks, unless `--force`; it
takes `--with-extensions` too). Extensions stay installed unless that destroy removed them, and it
names the ones that stay. `meko destroy` stops the service before the server, so its manager
doesn't start the server again mid-removal; removes the data folder and the client token (also when
it's in the Keychain, outside the folder); and says the service is still installed. The releases
are outside the data folder, so the service can still start Meko on a fresh one.

## Release builds

`src/meko/scripts/package-release.sh <version> <arch> <node-dir> <out-dir>`, run on the platform
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
gcc-toolset-13; the Mac one on `macos-15` with `MACOSX_DEPLOYMENT_TARGET=14.0`. Their downloads
retry (dnf's toolchain, and Node through the repo's `scripts/release/fetch-node.sh`), and each
step that downloads has a timeout of its own. Nothing is signed or notarized, as with yugabyted's macOS tarball: files that `gh` or `curl`
download carry no quarantine flag, and Go signs darwin/arm64 binaries ad hoc.

## Releases

`.github/workflows/meko-release.yml` runs on pushes to `main` that change `src/meko/**`,
`scripts/release/**` or itself, and by hand (force, or rebuild a `meko-v` tag whose tree has
`src/meko`). It uses the repo's
release scripts with `--product meko`:

| | AgentK (`agentk-release.yml`) | Meko (`meko-release.yml`) | Presenter (`presenter-release.yml`) |
|---|---|---|---|
| Tag | `agentk-v0.YYYYMMDD.N` (`v0.…` before) | `meko-v0.YYYYMMDD.N` | `presenter-v0.YYYYMMDD.N` |
| Title | its tag | its tag | its tag |
| Releases on | feat/fix/perf/deps subjects other than those naming only `meko` or `presenter` | feat/fix/perf/deps subjects naming `meko`, alone or in a list (`feat(agentk,meko): …`) | feat/fix/perf/deps subjects naming `presenter`, alone or in a list |
| Assets | AgentK tarballs, `SHA256SUMS`, `SHA256SUMS.sig`, and a stub `install.sh` that tells an older Meko, which looks for it, to update first | Meko tarballs, `SHA256SUMS`, `SHA256SUMS.sig`, Meko's `install.sh` | `presenter-<version>-<os>-<arch>.tar.gz` (holding `bin/me_presenter`, and `bin/meko_presenter`, its name before, as a link), `SHA256SUMS`, `SHA256SUMS.sig`; no installer: Meko installs them |
| Latest | `--latest=false` | `--latest` | `--latest=false` |
| Pruning | newest 5 per lane of its own tags | newest 5 `meko-v` releases | newest 5 `presenter-v` releases |

Plan, then `make test`, then the three builds (each packaged and smoke-tested), then publish:
`SHA256SUMS` and its signature ([below](#signed-releases)), the release with notes from
`release-notes.sh --product meko`, provenance attestation, and pruning. Meko picks each product's
releases by its own tag prefixes and tarball names, so an AgentK or Presenter release is never taken
for Meko's, nor the other way round.

### Signed releases

Each release workflow signs its `SHA256SUMS` with an Ed25519 key that only a run from `main` can
use, and Meko checks that signature before it trusts the checksums. A release built from another
branch, or published by hand, can't carry a signature that verifies.

- **The key** is the secret `RELEASE_SIGNING_KEY` of the repo's GitHub Environment `release`,
  whose deployment-branch rule allows only `main`. Each publish job says `environment: release`.
  A run dispatched from another branch can edit its workflow, but not the environment's rule, so
  GitHub never hands it the key, and the release fails at the publish job. Changing the rule takes
  a repo admin. The environment must exist before a workflow names it: GitHub makes one that
  doesn't exist on the spot, with no branch rule. A maintainer made the key on their own machine
  and keeps its backup in the team's password manager; only the public key is in the repo.
- **Signing** ("Sign SHA256SUMS", after "Write SHA256SUMS") writes `SHA256SUMS.sig`, one line for
  each key the environment holds: `<key id> <base64 signature>`. The key id is the first 8 bytes,
  in hex, of the sha256 of the raw 32-byte public key, and the signature is the 64-byte Ed25519
  signature of `SHA256SUMS`'s bytes, made with `openssl pkeyutl -sign -rawin`. The step is inline
  in each workflow, not a script in `scripts/release/`, because a rebuild checks out an older tag.
- **The check before publishing** ("Check the signature against Meko's keys") verifies the first
  line, which is always `RELEASE_SIGNING_KEY`'s, with the `.pub` of the same key id, and fails the
  release when none matches: a secret that isn't the key Meko has would otherwise publish releases
  every Meko refuses. It reads the keys from the workflow's own commit (`$GITHUB_SHA`, with
  `git show`), not from the checkout, since a rebuild's older tag may lack a newer key.
- **Meko's keys** are `src/meko/internal/update/keys/*.pub` (PEM), built into it with `go:embed`.
  Staging (`update.App.Stage`, for Meko and its extensions alike) downloads `SHA256SUMS.sig` when
  the release has one, checks it (`VerifySumsSignature`), then the checksum:

  | `SHA256SUMS.sig` | Meko |
  |---|---|
  | A line by one of its keys verifies, and none fails | installs it |
  | A line by one of its keys doesn't verify, or the file is malformed | refuses it, whatever the setting |
  | Missing, or every line is by a key it doesn't know | refuses it with `[update] require_signature` (the default); with `require_signature = false`, installs it with a warning |

  A key it doesn't know counts as no signature: someone forging a release could leave the file out
  anyway, and it keeps a Meko too old for a newer key able to update with the setting off.
  `require_signature = false` (or `MEKO_UPDATE_REQUIRE_SIGNATURE=false`) is for releases made
  before signing, with `meko extension install --version`, and for a fork in `[update] repo`. The
  one-line install (`install.sh`) checks only the checksum.
- **Checking a release by hand**, with OpenSSL 3 (1.1.1 and macOS's LibreSSL have no `-rawin`):

  ```sh
  cut -d ' ' -f 2 SHA256SUMS.sig | head -n 1 | openssl base64 -d -A -out sig.bin
  openssl pkeyutl -verify -pubin -inkey src/meko/internal/update/keys/release-2026.pub -rawin \
    -in SHA256SUMS -sigfile sig.bin
  sha256sum -c --ignore-missing SHA256SUMS   # shasum -a 256 -c on macOS
  ```

  The first line's key id names the key: `openssl pkey -pubin -in <key>.pub -outform DER | tail -c 32
  | openssl dgst -sha256 -r | cut -c1-16` prints a `.pub`'s.
- The provenance attestation stays as it was, for `gh attestation verify`; Meko doesn't read it.

#### Changing the key

Releases are signed with both keys while installed Mekos learn the new one. Each step's order
matters: a Meko that knows neither key on a release can't update without the setting off.

1. A maintainer makes a new pair on their own machine (`openssl genpkey -algorithm ed25519 -out
   release-signing.pem`, then `openssl pkey -in release-signing.pem -pubout -out
   release-<year>.pub`) and backs the private key up. A PR adds `keys/release-<year>.pub`, so its
   Meko release trusts both keys.
2. In the `release` environment, `RELEASE_SIGNING_KEY_OLD` gets the old private key and
   `RELEASE_SIGNING_KEY` the new one. Every release now has two lines, the new key's first, and
   every Meko, old or new, accepts it.
3. After 90 days (installs with automatic updates off can lag), delete `RELEASE_SIGNING_KEY_OLD`,
   and a PR drops the old `.pub`. A Meko older than step 1's release then says the release is
   signed with a key it doesn't know: the one-line install, or
   `MEKO_UPDATE_REQUIRE_SIGNATURE=false meko update` once, brings it up to date.

For a **leaked key**, step 1's PR also drops the leaked `.pub`, its release is still signed with
both keys (installed Mekos know only the leaked one), and `RELEASE_SIGNING_KEY_OLD` is deleted
right after it. An install trusts the leaked key until it updates: within hours with automatic
updates. A **lost key** with no backup allows no overlap, so installed Mekos recover as in step 3.

## The service

`internal/service` writes the service file from an embedded template, and runs it with launchctl
(`bootstrap`, `bootout`, `kickstart`) or `systemctl --user`:

| | launchd (`~/Library/LaunchAgents/com.yugabyte.meko.plist`) | systemd (`~/.config/systemd/user/meko.service`) |
|---|---|---|
| Runs | `<app>/service/meko serve --detached --data-dir <data>`, a script that execs `<app>/current/bin/meko` (a build from source: the binary), with `MEKO_SERVICE` set | `<app>/current/bin/meko serve --detached --data-dir <data>`, with `MEKO_SERVICE` set |
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
- A release's LaunchAgent runs `<app>/service/meko`, never `app/current/bin/meko` itself. macOS's
  Background Task Management shows "Background Items Added" again whenever the program a
  LaunchAgent names turns out to be another file, which `app/current/bin/meko` is after every
  update. An update never replaces the launcher, and `meko install` and `serve` rewrite it, or the
  plist, only when their content changes, since a rewritten file is announced too. `serve` under
  launchd brings the plist up to date at every start (`refreshLaunchAgent`) without reloading the
  job, which runs from the new plist from the next login: that moves an install from before the
  launcher onto it, with one last alert. The launcher (0700, in a 0700 folder) is in the app
  folder, not the data folder, so the service `meko destroy` keeps still starts; `meko uninstall`
  removes it with the plist. A build from source doesn't update itself, so its plist runs the
  binary.
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
--non-interactive`) for `meko destroy --with-extensions`. `status` exits 0 only while the
extension runs (AgentK and Presenter exit 3 when stopped), and its first line is shown in `meko
status`'s row. Meko runs every extension's `status` at once and gives each 3 s
(`extensions.StatusTimeout`); an update's check of whether the extension runs (`Entry.Status`)
waits up to 15 s. Every command Meko runs of an extension
(those, its installer and `me_koder install`) gets `MEKO_HOME` naming Meko's data folder, so the
extension knows where its own folder is: `<data>/extensions/<name>` (`extensions.Dir`). AgentK's
home is `<data>/extensions/agentk`, wherever Meko's rule puts the data folder (`MEKO_HOME`,
`~/.meko`, or the network-home fallback; AgentK runs the same rule when Meko didn't tell it, and
AgentK's and Presenter's tests read `internal/paths/testdata/resolve-cases.json` as Meko's do). An AgentK whose home is in
another data folder refuses `start`, `stop`, `status`, `uninstall` and `_handoff` run with this one's
`MEKO_HOME`, and does nothing.

### The summary rows

`meko start` and `meko stop` run each extension's `start` and `stop` with `MEKO_SUMMARY=1` too
(`Entry.RunSummary`), and pass its output through. With it set, the extension prints exactly one
line, a row of Meko's summary (`internal/cli/report.go`): two spaces, a mark (`✓` done, `!` a
warning, `✗` failed), its title padded to 15 characters, a space, then what happened, such as
`  ✓ AgentK          started; sessions resume in a few seconds`. The mark is colored (green,
yellow, red) only when stdout is a terminal and `NO_COLOR` is unset. It leaves out hints Meko's own
rows cover, such as `me_koder status` after a start (`meko start` ends with what to run next), but
keeps what only it knows: AgentK's stop row says where its sessions resume, `meko start`, or
`me_koder serve` when it runs without services. On a failure it prints its error
to stderr as usual and exits non-zero, and Meko adds its own `✗` row after it. Run without
`MEKO_SUMMARY` (by hand, by an older Meko, or by `meko extension update` and the Extensions page,
which keep its output for their logs) it prints what it always has. AgentK and Presenter format the
row in `src/server/summary.ts`.

`meko update`, `meko extension update` and `meko extension install` print the rows of the steps that
otherwise print nothing (`internal/spin`): `Release`, for the release they download, check and unpack
(`  ✓ Release         AgentK 0.20261030.0 (agentk-v0.20261030.0), downloaded and checked`, or `meko
<version> installed`), and for `meko update` under the service, `Server`, for the restart. While such a
step runs on a terminal, its row shows rotating dots (gh's spinner, which `install.sh`'s download
shows), and so does the listing of the releases, which leaves no row. Off a terminal, as in the
Extensions page's jobs, a step prints what it's doing as a plain line when it starts (`Installing
AgentK <version> (<tag>)`, `Restarting into meko <version>`), so the output isn't empty while it
downloads, then its finished row. On a failure the row is `✗` with what it was doing, and the error
follows on stderr.

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
  it says to run `meko extension install agentk` after `meko start`. A data folder with extension
  data and no `meko.db` (AgentK installed without Meko) has nothing of Meko's to remove; with
  `--with-extensions` the extensions are uninstalled, their data removed, and the folder too if
  that empties it.

- The catalog (`extensions.Known`) has AgentK and Presenter, both `Managed`: Meko installs and
  updates them from their own releases, and neither ships an installer. Their commands are
  `me_koder` and `me_presenter`, so a process list, `~/.local/bin` and the services say they're
  Meko's extensions (a release's Node is `node/bin/me_koder` or `node/bin/me_presenter` too); the
  extensions, their folders and tarballs are `agentk` and `presenter` (tags `agentk-v*`, and `v*`
  from before releases were tagged by product; `presenter-v*`).
- The commands' old names (`Catalog.OldCommands`: AgentK's `agentk` and `agent-k-local`,
  Presenter's `meko_presenter`) stay in their releases' `bin/` as links, for the services and older
  Mekos that run them. A version unpacked before the rename has only its old name: before Meko
  stages a version or points `app/current` at one (an install, an update, a rollback), and at its
  start for every installed one, it adds `bin/<command>` as a link to the old name
  (`update.App.Adopt`). At its start Meko also moves each extension installed under an old name onto
  the new one (`Registry.Rename`): it links `~/.local/bin/<command>`, points the registry at it, and
  removes the old names' links into this data folder's releases. `meko extension install` does the
  same.
- `meko extension install <name>` needs a running Meko. When the extension's command is on PATH or
  in `~/.local/bin` (or one of its old names, and for AgentK the `app/current/bin` of its old home
  `~/.agent-k-local`: `Catalog.OldCommands`, `OldHomes`), it's used
  as it is, unless `--version` names a release, which is installed over it. Otherwise, or then,
  Meko picks the newest release with this platform's `<name>-<version>-<os>-<arch>.tar.gz` and
  `SHA256SUMS` (skipping its `bad-versions`), or the one `--version` names (even one rolled back),
  and installs it as Meko installs itself (`update.App`): `SHA256SUMS`'s signature checked
  ([Signed releases](#signed-releases)), the tarball checked against `SHA256SUMS`, unpacked, `bin/<command> version --json` checked to name the version (an AgentK
  release from before `--json` prints the bare version, which counts) and the catalog's `CheckArgs`
  run (AgentK's `selftest`), into `<data>/extensions/<name>/app/<version>` (owner-only,
  `App.Private`: folders and programs 0700, files 0600, as everything in the data folder), with
  `app/current` and `~/.local/bin/<command>` pointing at it. Without `GITHUB_TOKEN` or a signed-in
  `gh`, a refusal from GitHub says why: GitHub's 60 API calls an hour for a caller with no token are
  spent (it names when they come back), or, for a 401, 403 or 404, the repo may be private;
  either way it offers signing in (`update.tokenlessHint`). Then it runs
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
  `me_koder uninstall --purge --delete-worktrees --non-interactive` and
  `me_presenter uninstall --purge --non-interactive`, which remove the extension's folder,
  releases and all (`meko destroy --with-extensions` then removes the `~/.local/bin` link Meko made):
  Presenter's decks stay in their Meko datapacks.
- `meko extension update <name>` (and the Extensions page's **Update**) moves a release install Meko
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
  - With the catalog's `RestartArgs` (AgentK's `me_koder restart`, which restarts only its server, so
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
  the catalog's `BusyArgs` (Presenter's `me_presenter _busy`, true while a talk is live) exit 0,
  within 15 s. It's tried again at the next check. Updates someone asks for never wait. When Meko
  installs its own update, it restarts first, and the new Meko's first check, a minute later,
  updates the extensions (a Meko on probation still checks; it only installs no update of its
  own until the probation ends): the same order as `meko update`, which updates Meko, then runs the new
  `meko extension update --all`. `[update] channel = "off"` stops all of it; `[update] auto` is
  Meko's own.
- **The update lock.** Every update, Meko's and the extensions', scheduled or asked for, holds the
  data folder's update lock (`update.Lock`): a mutex in `serve` and a `flock` on `run/update.lock`,
  which `meko update`, `extension update` and `extension install` take in their own processes. A
  second update while one runs is refused (the Status page's `UPDATE_BUSY`, "another update is
  running" on the command line), and a scheduled check skips its turn.
- A command npm installed (`npm link`, `npm install -g`: Presenter from source) is a
  `#!/usr/bin/env node` script in the bin folder of the Node that installed it, and the login
  service's PATH has no such Node (launchd's is `/usr/bin:/bin:/usr/sbin:/sbin`), or an older one.
  So when an extension's command sits in a folder that holds a `node`, Meko runs it (`start`,
  `stop`, `status`, `_handoff`, its uninstall, and its `install` from `extension install` or the
  Extensions page) with that folder first on PATH (`extensions.CommandEnv`).
- An extension whose catalog entry has `WebUI` (AgentK, Presenter) is served to browsers by the web listener at
  `/ext/<name>/`, on the unix socket `<command> _handoff --json` names; at every start, that handoff
  also brings over the web settings and certificate the extension had when it served browsers
  itself (its old login is ignored) ([web.md](web.md#the-handoff-from-agentk)). AgentK listens only on its socket.
- `meko start` starts the extensions after Meko; `meko stop` stops them first (`me_koder stop`
  ends AgentK's sessions too; they resume on start), unless given `--keep-extensions`. An
  extension that fails is reported, and the rest go on. `meko restart`, Meko's updates and crashes
  never touch them: they have their own services (AgentK) or processes (Presenter), and an
  extension restarts only for its own update.
- `meko extension delete <name>` removes an extension (`runDeleteExtension`). After asking (unless
  `--force`), and holding the update lock (which keeps updates and release downloads out, not an
  install's own setup), it runs the catalog's `RemoveArgs` (`uninstall --non-interactive`: the
  extension stops and removes its services, and AgentK's sessions end), removes
  `<data>/extensions/<name>/app` and the `~/.local/bin` link into it, and unregisters it. Its data
  stays. With `--purge` it runs `UninstallArgs` instead and removes `<data>/extensions/<name>`, as
  `meko destroy --with-extensions` does for every extension. If the extension's uninstall fails,
  nothing is removed. If a version still runs after it (`App.Live`: a session host whose stop
  failed), the releases and the registration stay, and it says what runs. Meko removes only the
  links it made into its releases (`UnlinkCommand`, and its old names'), never a build from source,
  but the extension's own uninstall may remove its links: AgentK's removes `~/.local/bin/me_koder`,
  `agentk` and `agent-k-local`
  whatever they point to, a link into another data folder's release too. A registered extension Meko
  doesn't know is only forgotten. A damaged extension (registered, but its command is missing and
  `Catalog.Find` doesn't find it again; `meko start`, `stop` and `update` say so, naming this
  command) has no uninstall to run, so its services, if any, stay: a plain delete removes its
  releases and forgets it, and `--purge` is refused, since such a service may still use its folder.
  The same goes for a `--purge` after a plain delete, which left no command to run: installing it
  again first lets the purge run its uninstall.
- `meko extension list` shows every known extension and its state without a running server: it
  runs each installed one's `status` at once (`StatusTimeout` each), reads the release installed and
  the last check's `update.json`, and looks for a missing command again without saving what it finds.
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
  service/meko               what the launchd service runs: execs current/bin/meko
  update-pending.json        an update on probation
  bad-versions               versions rolled back; never offered again
```

`internal/update`:

- **Checks** run inside `serve` a minute after start, then every `[update] interval_hours` (6). The
  client lists the repo's newest 100 releases through GitHub's API with `GITHUB_TOKEN` or `gh auth
  token`, or with no token on a public repo, within GitHub's 60 API calls an hour for a caller
  with none (looked for in Homebrew's folders and `~/.local/bin` too, since a service starts with a
  short PATH), and picks the newest stable `meko-v*` release with this platform's tarball and
  `SHA256SUMS` that isn't in `bad-versions`. What it found goes to `<data>/update.json`, which
  `meko status` shows.
- **Applying** happens on its own only under the service (`MEKO_SERVICE`) with `[update] auto`
  (default true); a server started by hand only reports updates. `meko update` installs one now, and
  so does **Update** on the web UI's Status page, through the running server's own updater
  ([web.md](web.md)): under the service, `serve` exits 0 as for an automatic update; started by
  hand, it execs the new `app/current/bin/meko` in its own place once it has stopped.
  Staging downloads the tarball, `SHA256SUMS` and `SHA256SUMS.sig` beside the installed versions,
  checks the signature ([Signed releases](#signed-releases)), then the checksum,
  unpacks it (refusing entries or links outside its folder), and runs the new
  `bin/meko version --json`, which must name the version: a binary that can't run here never
  becomes current. Then `update-pending.json` records `{from, to, schema, attempts}`, `app/current`
  is repointed atomically, and `serve` exits 0 for the service manager to start the new version.
- **Probation**: each start of the pending version counts an attempt (a clean stop takes it back);
  5 minutes up finishes the update, prunes versions older than the one it replaced, and makes
  `~/.local/bin/meko-cli` if it isn't there ([above](#install)). Until then
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
- `[update] channel = "off"` stops the scheduled checks (Check now and Update on the Status page still work, as `meko update` does); `[update] repo` (or `MEKO_UPDATE_REPO`, default `yugabyte/meko`: `update.DefaultRepo`) points at a fork, whose releases Meko's keys didn't sign, so it also needs `[update] require_signature = false` (or `MEKO_UPDATE_REQUIRE_SIGNATURE=false`). With that setting off, a release with no signature, or one by a key Meko doesn't know, installs with a warning once it's staged: a `!` row in `meko update`, `meko extension install` and `meko extension update`, and in the Extensions page's job output, and a WARN in `meko.log` for every one `serve` installs (Meko's own updates, and the extensions' automatic updates and Extensions page actions). A release refused at a later check gets no warning.

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
- `internal/update`: release picking, staging against a fake GitHub API, the signature (good, bad,
  by an unknown key, missing, with `require_signature` on and off; malformed files; Meko's own keys
  parsing; a signature `openssl` made, where it has `-rawin`), checksum and binary
  checks, unsafe tarballs, switching, three failed starts then rollback, a clean stop not counting,
  probation and pruning; the web UI's install (restarting once switched, nothing newer, a failed
  one) and its lock against the scheduled checks.
- `internal/store`: `RestoreBackup` undoes a real upgrade.
- `internal/cli`: `install` with a fake service manager, start, stop and restart through it,
  extensions run on start and stop but not restart or `--keep-extensions`, `uninstall` and
  `--purge`, `extension install` with an installed `agent-k-local` (an AgentK from before the rename) and from a fake release (signed with a test key, and an unsigned one refused, then installed with the setting off),
  `extension list` (states, damaged, a slow status) and `extension delete` (keeping or purging the
  data, a refused or failing one, a damaged one and its refused purge, a release still in use, an
  unknown one), `meko update` running the new meko's
  `extension update --all`,
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
