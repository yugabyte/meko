# Releases and auto-update

Part of the [AgentK design](../design.md).

- **CI (on PRs that change AgentK, and every push to `main`).** Typecheck, lint and unit tests, on
  Linux and macOS, run in `extensions/agentk` (the workflows, `scripts/check-title.sh`,
  `scripts/ci-scope.sh` and Dependabot's config are at the repo root). `scripts/ci-scope.sh` decides
  from a PR's changed paths whether AgentK's jobs run: `extensions/agentk/`, the root `scripts/` and
  `.github/` (which AgentK's tests read) and any root file outside the known components do, root
  Markdown, `.claude/` and `backend/` don't. Integration tests run
  against a real session host and git, with a stub `claude` binary that fakes the TUI and fires the hooks.
  A separate `PR title` workflow checks the title with the repo root's `scripts/check-title.sh` and reruns when
  the PR is edited. Changes reach `main` only through PRs, squash-merged once the `CI gate` check (over
  every component's jobs, run or skipped) and the title check pass.
- **Versioning (on push to main).** Versions are `0.YYYYMMDD.N`: the UTC date, and `N` counting
  from 0 for each release that day. Commit subjects are `<type>(<component>[/<area>])[!]: <summary>`
  with one of nine types and one of the repo's components (`agentk`, `meko`, `repo`:
  `scripts/check-title.sh`), and PRs are squash-merged with the PR title
  as the subject. A push with any `feat`, `fix`, `perf` or `deps` commit for AgentK since its last
  release runs the tests and cuts the next version, and those subjects become the release notes;
  `ci`, `test`, `docs`, `nit` and `chore` don't release. Releases are scoped by component: Meko
  (`backend/`) releases on its own, from its own tags (`meko-v…`), and its `meko` subjects never
  release AgentK. Every other subject (`agentk`, `repo`, and older ones with no component or a
  free-form one) counts for AgentK. The repo root's `scripts/release/` holds the scripts both
  products use, with `--product agentk|meko`: `next-version.sh`, `release-notes.sh` and
  `prune-releases.sh` (`product.sh` names each product's tags and components).
- **Build matrix (after the tests).** Self-contained tarballs are built for `linux-x64` and
  `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`. Each bundles the Node 22 runtime,
  node-pty (built from source: gcc-toolset-13 on Linux, Xcode's clang on macOS) and the web assets.
  Each is published twice: as `agentk-<version>-<platform>-<arch>.tar.gz` (in `agentk-<version>/`,
  with `bin/agentk`), and as `agent-k-local-<version>-…` (in `agent-k-local-<version>/`), the name
  and layout that the updaters and installers from before the rename to AgentK look for. An
  updater from then skips any release without its name, so without the second copy those installs
  would never update again. Both hold `bin/agent-k-local`, a link to `bin/agentk`, which the
  services installed then start. The updater installs only newer releases, all of which have the
  new name, so it reads only that; the installer falls back to the old name for an older
  `--version`. The copy goes a week after the first release that has it (#238): an install still on a release from before the rename by then needs a reinstall.
  The installer and the updater pick the tarball for their platform. The updater skips releases
  with no build for its platform; the installer takes the latest release (or `--version`) and stops
  with a clear message if it has none. Releases since macOS support always include the macOS
  build: publishing waits for it. A hand-run rebuild takes only tags from after the move to
  `extensions/agentk`. Each release also includes `SHA256SUMS`, a build provenance attestation
  and `install.sh`. Release tarballs carry the usage stats collector's URL (`CALL_HOME_URL`, from the repo variable of that name) when it's set ([Usage stats](usage-stats.md)). Afterwards, AgentK's releases beyond the newest 5 in each lane (`stable`, or the
  prerelease identifier such as `beta`, per tag prefix) are deleted, except the newest `v…` one;
  their tags stay, and Meko's are left alone.

> **glibc.** RHEL 8 devservers ship glibc 2.28, and node-pty is a native module. Linux builds run
> in an AlmaLinux 8 container rather than on `ubuntu-latest`, so the binary loads on older hosts.

```sh
# The repo's one-line install (gh release download --pattern install.sh, from its latest release)
# installs Meko, whose `meko install_extension agentk` installs AgentK pointed at it. AgentK
# alone: its own install.sh from the newest AgentK release (no root; the repo is internal for
# now, so this uses gh).
gh release download "$(gh release list --repo yugabyte/agent-k-local --exclude-drafts \
  --exclude-pre-releases --limit 50 --json tagName \
  --jq '[.[].tagName | select(test("^(agentk-v|v)[0-9]"))][0]')" \
  --repo yugabyte/agent-k-local --pattern install.sh --output - | sh

# what it does
#  0. stops, saying so, if AgentK is already installed in AK_HOME and set up: app/current,
#     config.toml, ~/.local/bin/agentk, and both services enabled (systemd) or loaded
#     (launchd). Updates are offered in the app, and `agentk install` redoes the setup.
#     --reinstall, --deps-only, a --version other than the installed one, or options for
#     `agentk install` (other than --non-interactive) go on
#  1. checks for git, curl, gh and Claude Code; offers to
#     install whatever is missing (it shows the commands and asks first): brew on macOS, the
#     package manager with sudo on Linux (apt-get, dnf, yum, zypper, pacman or apk), gh on Linux
#     from its GitHub release (checksum-verified) into ~/.local/bin, and Claude Code with
#     Anthropic's installer (claude.ai/install.sh, which publishes no checksum) into ~/.local/bin;
#     with GITHUB_TOKEN set, a missing gh is only installed with --install-deps (downloads use
#     curl and the token). It checks the tools themselves afterwards: Homebrew exiting with an
#     error once they work (it builds from source on a macOS it no longer supports) is a warning,
#     while a failed Linux package manager stops it; then offers `gh auth login` and `claude auth
#     login` for whichever isn't signed in. Sessions add ~/.local/bin to PATH after the login profile runs. --install-deps installs without asking,
#     --no-install-deps never installs, --deps-only stops after this step
#  2. unpacks to ~/.agent-k-local/app/<ver>, points app/current at it, then runs `agentk install`:
#  3. asks for your repos folder (default ~/code), access (local: 127.0.0.1, no login; remote:
#     0.0.0.0, HTTPS and a login; remote by default over SSH or with no display) and port
#  4. Meko (optional): verifies the API key (reuses MEKO_API_KEY from ~/.claude/settings.json,
#     else offers a Meko running on this computer, else asks; Enter or --no-meko skips it;
#     --meko-url and --meko-key name one) and offers to run Meko's setup for Claude Code if its
#     plugin, MCP server or key is missing (declining is a warning)
#  5. picks the database location (local disk if ~/.agent-k-local is on NFS), writes config.toml,
#     sets up TLS
#  6. installs both background services: systemd user units with linger if allowed,
#     or LaunchAgents on macOS (needs a desktop login)
#  7. prints the one-time setup link (remote access), or the address (local access)
```

**Offline bundle.** People without access to the repo (it's internal) can't run the installer's
download step, but a release tarball needs nothing else: it carries its own Node and dependencies.
`scripts/offline-bundle.sh <version>` (run with an authenticated `gh`) packs a release's tarballs
(every platform), `SHA256SUMS`, its `install.sh` and an `install-offline.sh` into
`agentk-<version>-offline.tar.gz` (about 160 MB). Unpacked anywhere, `sh install-offline.sh
[options]` runs `install.sh --tarball <its folder>`, which takes the tarball for this computer,
checks it against `SHA256SUMS` and goes on from step 2 above. `--tarball` also takes one tarball
file; it installs the version it holds, so it can't be combined with `--version`. The installer
still checks for the tools (step 1), since AgentK uses git and gh on the user's own repos. Only
releases whose `install.sh` has `--tarball` can be bundled. Such an install still points at this
repo for updates, so its update checks fail ("GitHub API returned 404"): set `update.channel =
"off"` in its `config.toml`, and send a newer bundle to update it. Usage stats and Meko keep
their defaults ([Usage stats](usage-stats.md); `--no-meko` skips Meko).

**Tags.** The repo releases two products. AgentK's releases are tagged `agentk-v<version>`
(`v<version>` before releases were tagged by product); Meko's are `meko-v<version>`. Versions stay
bare (`0.YYYYMMDD.N`): tarball names, `app/<version>` and `bad-versions` use them, and on the day of
the switch `next-version.sh` counted both prefixes so no version was used twice. AgentKs installed
before the switch read only `v…` tags; the last `v…` release (the bridge) reads both, so they reach
the `agentk-v…` releases through it, and pruning never deletes the newest `v…` release (the old
prefix is a lane of its own that stops growing). Meko's
releases are the repo's "latest", since its installer is the one-line install, so AgentK's are
published with `--latest=false`: the installer finds the newest AgentK release by its tag (the
newest non-draft, non-prerelease `agentk-v…` or `v…`), and `--version` tries both tags. The updater
reads both prefixes too (`TAG_PREFIXES`, `tagVersion` in `updater.ts`), skips Meko's, and downloads
and compares (What's new) by each release's own tag. A version whose tag it hasn't listed is tried
under each prefix, since pruning deletes releases but keeps their tags.

**Update loop.** The server checks the GitHub Releases API at startup and every 6 hours
(`update.interval_hours`). The channel can be *stable*, *prerelease* or *off*. When a newer version appears, the rail shows a
"Version X is available" banner with an **Update to the latest version** button (Settings has the
same button); a version that was rolled back is never offered again. When you click it, the server:

1. checks the releases again and takes the newest one, so a release cut since the last check is
   installed now rather than offered right after the restart (if the check fails, it installs the
   version found before). A second click while this runs gets the same update;
2. downloads the tarball and verifies it against `SHA256SUMS`, and against its build provenance
   attestation (with `gh`) when `[update] require_attestation = true`;
3. unpacks it next to the current version and runs its self-test;
4. snapshots the database with `VACUUM INTO state.db.bak-<old-ver>`, lets the new version run its
   migrations on start, and points `app/current` at the new version;
5. restarts only the web server unit. Sessions keep running. Closing, the server gives each
   browser's socket 1 second to answer its close and then drops it, so a page that can't answer
   (a sleeping phone or laptop) doesn't hold the restart for the 30 seconds `ws` would wait. An open
   page whose last state said an update was installing retries its events socket every second
   (for up to 2 minutes, then with the usual backoff), so it reloads into the new version as soon
   as it's up;
6. if the new version fails to come up three times in a row (the service manager restarts it) before
   it has been healthy for 5 minutes, points back to the previous version and marks the new one as
   bad (`bad-versions`, which the updater never offers again). A deliberate stop (SIGTERM, as for a
   service restart) takes its start back off the count, so restarting by hand in that window isn't
   mistaken for a crash loop. A start that fails once it holds the home's lease (a migration,
   Core's start, the web server) writes its error to `server.log`, and the launcher writes the
   rollback there (once `logs/` exists), not only to stderr, which reaches no one where the service
   manager's log isn't kept (a user journal that isn't persistent). A failure before the lease,
   such as an unreadable `config.toml`, still goes only to stderr.

Once a version has been healthy for 5 minutes, the server deletes every installed version under
`app/` except the running one and one to roll back to by hand: the version it updated from, or
(after a manual switch) the newest older one. It also deletes the database snapshots of the versions
it removes. Nothing is removed while an update is being installed or switched to, and never the
version `app/current` points at. Each version is about 180 MB, most of it the bundled Node runtime.
The "Updated to" notice shows from the new version's first boot, without waiting for the healthy
check.

The rail shows "Updated to v0.4.1 (from v0.4.0)" with a **What's new** link. Updates never need a
maintenance window, because they never touch the session host.

**What's new.** Releases come out several times a day and each one's notes cover only the commits
since the release before it, and releases beyond the newest 5 are deleted, so one release's notes
don't say what an update brought. The **What's new** dialog (`WhatsNewDialog.tsx`) shows every
change between two versions instead: the server asks GitHub for the commits between their tags
(`GET /api/update/notes`, `Updater.notes()`, through the same `gh` or HTTPS source as the update
check: `repos/<repo>/compare/<from tag>...<to tag>`, each of either prefix) and lists the `feat`, `fix`, `perf` and `deps`
subjects by type, as the repo root's `scripts/release/release-notes.sh` does (leaving out Meko's `meko` subjects) (`src/server/releasenotes.ts`), with
**Breaking changes** first and each line linked to its PR. GitHub returns at most 250 commits for a
comparison; past that the dialog says so, and **See every commit on GitHub** opens the comparison
there. A range's notes are kept in memory once fetched; nothing is fetched until the dialog opens.
It opens from the "Updated to" banner (the version updated from → this one), the "Version X is
available" banner and Settings while an update is on offer (this version → the offered one), and
otherwise from Settings for the last update (`update.lastUpdatedFrom` in `kv`, written when the
`Updater` is made, before the server starts, so a version that crashes while starting still
counts, and kept after the update's first boot; no link on an install that has never updated, or
after a rollback, when that version is no longer older than the running one). A failed comparison
shows GitHub's error; it names the sign-in only for the HTTPS source with no `GITHUB_TOKEN` and a
401, 403 or 404 (the `gh` source is used only once `gh auth status` passes). This is why release
pruning keeps the tags.

**Open pages reload themselves.** The server stamps its version into every page it serves
(`<meta name="ak-version">`, and the page is sent with `Cache-Control: no-cache`). When a page's
event socket reconnects to a server on another version (an update, a reinstall or a rollback), the
page reloads, keeping its route, so it never keeps running the old UI (`src/web/reload.ts`,
`hooks/useReload.ts`). It waits while a reload could lose something: a dialog, sheet or popover is
open, a session's terminal holds images for Claude (`hasHeldImages()` in `terminals.ts`, whether
or not that session is on screen), a text field has focus, or a textarea holds unsent text. Fields whose text survives a reload
are marked `data-reload-safe` and don't count (the terminal compose box keeps its draft in
`sessionStorage`), and neither does xterm's input. While it waits, a banner offers **Reload**, and
it checks again when focus leaves a field, the tab is shown or hidden, and every 5 seconds. A tab
reloads at most once for a given version (remembered in `sessionStorage`), so a mismatch a reload
can't fix shows the banner instead of looping. Without the meta tag (the Vite dev server), the
first version the server reports counts as the page's.
