# Releases and auto-update

Part of the [AgentK Local design](../design.md).

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
  from 0 for each release that day. Commit subjects are `<type>[(<component>)][!]: <summary>`
  with one of nine types (`scripts/check-title.sh`), and PRs are squash-merged with the PR title
  as the subject. A push with any `feat`, `fix`, `perf` or `deps` commit since the last release
  runs the tests and cuts the next version, and those subjects become the release notes; `ci`,
  `test`, `docs`, `nit` and `chore` don't release.
- **Build matrix (after the tests).** Self-contained tarballs are built for `linux-x64` and
  `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`. Each bundles the Node 22 runtime,
  node-pty (built from source: gcc-toolset-13 on Linux, Xcode's clang on macOS) and the web assets.
  The installer and the updater pick the tarball for their platform. The updater skips releases
  with no build for its platform; the installer takes the latest release (or `--version`) and stops
  with a clear message if it has none. Releases since macOS support always include the macOS
  build: publishing waits for it. A hand-run rebuild takes only tags from after the move to
  `extensions/agentk`. Each release also includes `SHA256SUMS`, a build provenance attestation
  and `install.sh`. Release tarballs carry the usage stats collector's URL (`CALL_HOME_URL`, from the repo variable of that name) when it's set ([Usage stats](usage-stats.md)). Afterwards, releases beyond the newest 5 in each lane (`stable`, or the
  prerelease identifier such as `beta`) are deleted; their tags stay.

> **glibc.** RHEL 8 devservers ship glibc 2.28, and node-pty is a native module. Linux builds run
> in an AlmaLinux 8 container rather than on `ubuntu-latest`, so the binary loads on older hosts.

```sh
# install (one line, no root). The repo is internal for now, so this uses gh.
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh

# what it does
#  0. stops, saying so, if AgentK Local is already installed in AK_HOME and set up: app/current,
#     config.toml, ~/.local/bin/agent-k-local, and both services enabled (systemd) or loaded
#     (launchd). Updates are offered in the app, and `agent-k-local install` redoes the setup.
#     --reinstall, --deps-only, a --version other than the installed one, or options for
#     `agent-k-local install` (other than --non-interactive) go on
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
#  2. unpacks to ~/.agent-k-local/app/<ver>, points app/current at it, then runs `agent-k-local install`:
#  3. asks for your repos folder (default ~/code), access (local: 127.0.0.1, no login; remote:
#     0.0.0.0, HTTPS and a login; remote by default over SSH or with no display) and port
#  4. Meko (optional): verifies the API key (reuses MEKO_API_KEY from ~/.claude/settings.json, or
#     asks; Enter or --no-meko skips it) and offers to run Meko's setup for Claude Code if its
#     plugin, MCP server or key is missing (declining is a warning)
#  5. picks the database location (local disk if ~/.agent-k-local is on NFS), writes config.toml,
#     sets up TLS
#  6. installs both background services: systemd user units with linger if allowed,
#     or LaunchAgents on macOS (needs a desktop login)
#  7. prints the one-time setup link (remote access), or the address (local access)
```

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
   mistaken for a crash loop. Each failed start's error and the rollback are written to
   `server.log`, not only to stderr, which reaches no one where the service manager's log isn't
   kept (a user journal that isn't persistent).

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
check: `repos/<repo>/compare/v<from>...v<to>`) and lists the `feat`, `fix`, `perf` and `deps`
subjects by type, as `scripts/release-notes.sh` does (`src/server/releasenotes.ts`), with
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
