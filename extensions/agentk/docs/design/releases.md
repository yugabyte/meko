# Releases and updates

Part of the [AgentK design](../design.md).

- **CI (on PRs that change AgentK, and every push to `main`).** Typecheck, lint and unit tests, on
  Linux and macOS, run in `extensions/agentk` (the workflows, `scripts/check-title.sh`,
  `scripts/ci-scope.sh` and Dependabot's config are at the repo root). `scripts/ci-scope.sh` decides
  from a PR's changed paths whether AgentK's jobs run: `extensions/agentk/`, the root `scripts/` and
  `.github/` (which AgentK's tests read) and any root file outside the known components do, root
  Markdown, `.claude/` and `backend/` don't. Nor does Markdown only people read: `README.md`,
  `CONTRIBUTING.md`, the `AGENTS.md`, `CLAUDE.md` and `REVIEW.md` files and `docs/`. Markdown the
  tests read (`test/`) or the release ships (`src/tools/skill/SKILL.md`) runs AgentK's jobs.
  Integration tests run against a real session host and git, with a stub `claude` binary that fakes the TUI and fires the hooks.
  A separate `PR title` workflow checks the title with the repo root's `scripts/check-title.sh` and reruns when
  the PR is edited. Changes reach `main` only through PRs, squash-merged once the `CI gate` check (over
  every component's jobs, run or skipped) and the title check pass.
- **Versioning (on push to main).** Versions are `0.YYYYMMDD.N`: the UTC date, and `N` counting
  from 0 for each release that day. Commit subjects are `<type>(<component>[/<area>])[!]: <summary>`
  with one of nine types and one of the repo's components (`agentk`, `meko`, `presenter`, `repo`:
  `scripts/check-title.sh`), or several comma-separated for a change that ships in more than one
  (`feat(agentk,meko): …`), and PRs are squash-merged with the PR title
  as the subject. A push with any `feat`, `fix`, `perf` or `deps` commit for AgentK since its last
  release runs the tests and cuts the next version, and those subjects become the release notes;
  `ci`, `test`, `docs`, `nit` and `chore` don't release. Releases are scoped by component: Meko
  (`backend/`) releases on its own, from its own tags (`meko-v…`), and subjects naming only `meko`
  never release AgentK. Presenter (`extensions/presenter`) releases on its own too
  (`presenter-release.yml`, tags `presenter-v…`): subjects naming only `presenter` (or `meko` and
  `presenter`) release nothing for AgentK. Every other subject
  (`agentk`, `repo`, lists such as `agentk,meko`, and older ones with no component or a free-form
  one) counts for AgentK, and every subject naming `meko`, alone or in a list, counts for Meko: a PR
  titled `feat(agentk,meko): …` releases both. The repo root's `scripts/release/` holds the scripts both
  products use, with `--product agentk|meko|presenter`: `next-version.sh`, `release-notes.sh` and
  `prune-releases.sh` (`product.sh` names each product's tags and components).
- **Build matrix (after the tests).** Self-contained tarballs are built for `linux-x64` and
  `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`. Each bundles the Node 22 runtime,
  node-pty (built from source: gcc-toolset-13 on Linux, Xcode's clang on macOS) and the web assets.
  Each is published twice: as `agentk-<version>-<platform>-<arch>.tar.gz` (in `agentk-<version>/`,
  with `bin/agentk`), and as `agent-k-local-<version>-…` (in `agent-k-local-<version>/`), the name
  and layout that the updaters and installers from before the rename to AgentK look for. An
  updater from then skips any release without its name, so without the second copy those installs
  would never update again. Both hold `bin/agent-k-local`, a link to `bin/agentk`, which the
  services installed then start. Meko reads only the new name. The copy goes a week after the first release that has it (#238): an install still on a release from before the rename by then needs a reinstall.
  Meko picks the tarball for its platform, and skips releases with no build for it. Releases
  since macOS support always include the macOS build: publishing waits for it. A hand-run rebuild
  takes only tags from after the move to `extensions/agentk`. Each release also includes
  `SHA256SUMS`, a build provenance attestation and `install.sh`, which only says that Meko installs
  AgentK (`meko install_extension agentk`): the Mekos from before Meko installed AgentK ran the
  newest release's `install.sh`, and an older AgentK's own instructions still name it. Release tarballs carry the usage stats collector's URL (`CALL_HOME_URL`, from the repo variable of that name) when it's set ([Usage stats](usage-stats.md)). Afterwards, AgentK's releases beyond the newest 5 in each lane (`stable`, or the
  prerelease identifier such as `beta`, per tag prefix) are deleted, except the newest `v…` one;
  their tags stay, and Meko's are left alone.

> **glibc.** RHEL 8 devservers ship glibc 2.28, and node-pty is a native module. Linux builds run
> in an AlmaLinux 8 container rather than on `ubuntu-latest`, so the binary loads on older hosts.

```sh
# The repo's one-line install (gh release download --pattern install.sh, from its latest release,
# which is Meko's) installs Meko. Then:
meko install_extension agentk                      # or Install on Meko's Extensions page
meko install_extension agentk -- --agent kiro      # options after -- go to `agentk install`

# what it does (Meko's install_extension, backend/docs/install.md)
#  0. downloads the newest agentk-<version>-<platform>-<arch>.tar.gz and SHA256SUMS (with gh's
#     token or GITHUB_TOKEN), checks it, unpacks it owner-only into
#     <Meko's data folder>/extensions/agentk/app/<version>, checks that it runs (`agentk version
#     --json`, `agentk selftest`), points app/current at it and links ~/.local/bin/agentk; an
#     install AgentK already has (one still in ~/.agent-k-local too) is set up again in place
# then `agentk install --meko-url … --meko-key …`, on the terminal it was started from:
#  1. scripts/deps.sh (lib/scripts/deps.sh in a release): checks for git, curl, gh and a coding
#     agent (Claude Code, OpenCode or Kiro CLI; one is enough); offers to install whatever is
#     missing (it shows the commands and asks first): brew on macOS, the package manager with sudo
#     on Linux (apt-get, dnf, yum, zypper, pacman or apk), gh on Linux from its GitHub release
#     (checksum-verified) into ~/.local/bin, and an agent with its own installer (Claude Code's
#     claude.ai/install.sh, which publishes no checksum, OpenCode's or Kiro's; which one: --agent,
#     else it asks, else Claude Code); with GITHUB_TOKEN set, a missing gh is only installed with
#     --install-deps. It checks the tools themselves afterwards: Homebrew exiting with an error
#     once they work (it builds from source on a macOS it no longer supports) is a warning, while
#     a failed Linux package manager stops it; then offers each agent's sign-in and `gh auth
#     login` for whichever isn't signed in. Sessions add ~/.local/bin to PATH after the login
#     profile runs. --install-deps installs without asking, --no-install-deps never installs,
#     and from Meko's Extensions page (--non-interactive) nothing is installed without
#     --install-deps
#  2. asks for your repos folder (default ~/code), and stops when there's no `meko` command: the
#     Meko on this computer serves AgentK to browsers (at /ext/agentk/) and signs people in
#  3. Meko: uses Meko's Meko Cloud link, else the Meko running on this computer, and stores neither
#     (no key is asked for: --meko-key, with --meko-url, links one in Meko; a key AgentK kept before
#     is handed to Meko once; --no-meko is ignored) and offers to run Meko's setup for Claude Code if
#     its plugin, MCP server or key is missing (declining is a warning)
#  4. picks the database location (local disk for an older home still on NFS), writes config.toml
#     (with the Meko data folder the home is in); on an older home, says it moves into
#     Meko's data folder once the server has started
#  5. installs both background services: systemd user units with linger if allowed,
#     or LaunchAgents on macOS (needs a desktop login)
#  6. prints the address Meko serves it at (meko setup_link makes the login for remote access)
```

**Tags.** The repo releases three products. AgentK's releases are tagged `agentk-v<version>`
(`v<version>` before releases were tagged by product); Meko's are `meko-v<version>`, and
Presenter's `presenter-v<version>` (`presenter-release.yml`; Meko installs and updates them). Versions stay
bare (`0.YYYYMMDD.N`): tarball names, `app/<version>` and Meko's `bad-versions` use them, and on the day of
the switch `next-version.sh` counted both prefixes so no version was used twice. AgentKs installed
before the switch read only `v…` tags; the last `v…` release (the bridge) reads both, so they reach
the `agentk-v…` releases through it, and pruning never deletes the newest `v…` release (the old
prefix is a lane of its own that stops growing). Meko's
releases are the repo's "latest", since its installer is the one-line install, so AgentK's are
published with `--latest=false`. Meko finds AgentK's releases by their tags (its catalog lists
both prefixes), and What's new compares two versions by their tags, trying each prefix
(`TAG_PREFIXES` in `updates.ts`), since pruning deletes releases but keeps their tags.

**Updates are Meko's.** Meko checks GitHub for AgentK's releases with its own (one list for Meko
and every extension, at its start and every `[update] interval_hours` of Meko's `config.toml`),
installs them, and rolls back one that keeps failing; AgentK has no updater of its own and ignores
an `[update]` table in its `config.toml`. Meko's design is in `backend/docs/install.md`. What AgentK
sees of it:

1. **The offer.** The page reads AgentK's row from Meko's own routes, on Meko's origin
   (`GET /api/v1/extensions`; `src/web/mekoupdate.ts`): the version installed, the newest one, the
   last check, a job Meko runs on AgentK, and **Update automatically** (on by default; Meko then
   installs an update when its check finds one). When a newer version is there, the rail shows a
   "Version X is available" banner with **Update to the latest version**; Settings › Updates has the
   same button, **Check now** (`POST /api/v1/extensions/agentk/check`) and the switch (`PATCH
   /api/v1/extensions/agentk`). A version that was rolled back is never offered again. When the row isn't `updatable`,
   Settings says why: Meko's `not_updatable` (a build from source, or an AgentK still in
   `~/.agent-k-local`, which Meko updates once it has moved), else, under a Meko too old to say,
   to update Meko first. An old `[update] channel = "off"` in AgentK's `config.toml` keeps Meko's
   **Update automatically** off for AgentK until that switch is set; AgentK keeps that one key when
   it saves its config.
2. **The update.** `POST /api/v1/extensions/agentk/update` starts Meko's job: it downloads the
   tarball and checks it against `SHA256SUMS`, unpacks it next to the current version, checks that
   it runs (`agentk version --json`, `agentk selftest`), writes its record
   (`app/update-pending.json`) and points `app/current` at it, then runs `agentk restart`. That
   restarts only the server's service, and waits up to 100 s for the server to answer as the
   version `app/current` names. Sessions keep running. The job lives in Meko, so it outlasts
   AgentK's restart; the page follows it (every 2 s while it runs) and shows the Installing banner.
3. **The restart.** Stopping, the server sees that `app/current` no longer names the version it
   runs from (`switchedAway()`), and sends every open page a `restarting` event first, so the page
   shows only the Installing banner while it waits, and not the lost-connection banner (which it
   shows after 2 minutes if the server isn't back; any other drop is shown after the usual 2
   seconds). While Meko's job is an update, the page's events socket retries every second (for up
   to 2 minutes, then with the usual backoff), so it reloads into the new version as soon as it's
   up. Closing, the server gives each browser's socket 1 second to answer its close and then
   drops it, so a page that can't answer (a sleeping phone or laptop) doesn't hold the restart for
   the 30 seconds `ws` would wait.
4. **Probation.** The launcher (`packaging/agentk`) hands `serve` and `host` to Meko's launcher
   (`meko _launch agentk <its own path> …`), which runs it again with `MEKO_LAUNCHED` set. For
   `serve`, Meko counts each start of a version on probation, and if it fails to come up three
   times in a row (the service manager restarts it) before it has run 5 minutes, points
   `app/current` back to the version before, marks the new one bad (`bad-versions`) and runs the
   old one; the job then fails, saying so. A deliberate stop (SIGTERM, as for a service restart),
   an exit 0 or a start refused because another server holds the home (exit 75) isn't counted, so
   restarting by hand in that window isn't mistaken for a crash loop. Without a Meko that has
   `_launch` (an older one, or none), the launcher runs the server itself and says a failed
   update won't roll back. A start that fails once it holds the home's lease (a migration, Core's
   start, the web server) writes its error to `server.log`, not only to stderr, which reaches no
   one where the service manager's log isn't kept (a user journal that isn't persistent).
5. **The database.** While a version is on probation, Meko's launcher tells it the version it was
   updated from (`MEKO_UPDATED_FROM`). At its first such start, before its migrations run, the
   server copies the database as the old version left it to `state.db.bak-<old version>`
   (`snapshotBeforeMigrating` in `updates.ts`), for going back by hand: Meko's rollback restores
   only the program.

Once a version has run 5 minutes, Meko finishes the update and deletes the installed versions
under `app/` except the running one and the one it updated from, skipping any version a running
process started from: `meko _launch` holds a lock on its version's folder for as long as the process
runs, so the version the session host runs from (it outlives updates) is never deleted. At each
start the server deletes the database copies of versions no longer in `app/`. Each version is about
180 MB, most of it the bundled Node runtime. The "Updated to" notice shows from the new version's
first start, without waiting for the 5 minutes.

The first release that brings the move of AgentK's home into Meko's data folder (#236) moves an
older home as soon as it has proved healthy, ending every session and resuming it in the new
place: [Moving the home into Meko's data folder](recovery.md#moving-the-home-into-mekos-data-folder).
The launcher finds the home from its own folder (`<home>/app/<version>`), so a rollback after the
move runs the previous version from the new home. An install still in an older home moves at its
first start of a release Meko launches; Meko updates it once it's in Meko's data folder.

The rail shows "Updated to v0.4.1 (from v0.4.0)" with a **What's new** link. Updates never need a
maintenance window, because they never touch the session host.

**What's new.** Releases come out several times a day and each one's notes cover only the commits
since the release before it, and releases beyond the newest 5 are deleted, so one release's notes
don't say what an update brought. The **What's new** dialog (`WhatsNewDialog.tsx`) shows every
change between two versions instead: the server asks GitHub for the commits between their tags
(`GET /api/update/notes`, `Updates.notes()` in `updates.ts`, through `gh` once `gh auth status`
passes, else HTTPS: `repos/yugabyte/meko/compare/<from tag>...<to tag>`, each of either
prefix) and lists the `feat`, `fix`, `perf` and `deps`
subjects by type, as the repo root's `scripts/release/release-notes.sh` does (leaving out the subjects that name only `meko` or `presenter`) (`src/server/releasenotes.ts`), with
**Breaking changes** first and each line linked to its PR. GitHub returns at most 250 commits for a
comparison; past that the dialog says so, and **See every commit on GitHub** opens the comparison
there. A range's notes are kept in memory once fetched; nothing is fetched until the dialog opens.
It opens from the "Updated to" banner (the version updated from → this one), the "Version X is
available" banner and Settings while an update is on offer (this version → the offered one), and
otherwise from Settings for the last update (`update.lastUpdatedFrom` in `kv`, written from
`MEKO_UPDATED_FROM` before the server starts, so a version that crashes while starting still
counts, and kept after probation; no link on an install that has never updated, or after a
rollback, when that version is no longer older than the running one). Opened from Settings, it
takes Settings' place once Settings' saves are in, and the ← before its title goes back to
Settings › Updates. A failed comparison
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
