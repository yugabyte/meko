# Contributing to AgentK

## Development setup

Requirements: Linux or macOS, Node 22, git, and Claude
Code. Meko (an API key plus the `meko-agent-skills` Claude Code plugin) is required to create
projects.

AgentK lives in `extensions/agentk` of the meko-local repo: run everything below from there.
Paths are relative to it, except `.github/`, `.claude/`, `scripts/check-title.sh` and
`scripts/ci-scope.sh`, which are at the repo root.

```sh
cd extensions/agentk
nvm use 22                      # .nvmrc pins the major version

# node-pty is a native module that needs a C++20 compiler. On RHEL 8 the system g++ (8.5)
# is too old; use gcc-toolset-13:
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc
export CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++
# On macOS, skip the exports: `xcode-select --install` and `brew install gh` are enough.
npm ci                          # also makes node-pty's macOS spawn-helper executable (scripts/postinstall.mjs)

npm run dev                     # API server on 127.0.0.1:7777 + Vite on 127.0.0.1:5173
```

Open http://127.0.0.1:5173. The dev server listens on loopback, so it opens without a login (local
access, see `localAccess()` in `src/server/auth.ts`). A dev server keeps its state in `./.dev-home` (set `AK_DEV_HOME` to
use another folder), so it never opens an installed AgentK's database, hook socket or session
host: it starts a session host of its own for that folder, which keeps running when `npm run dev`
restarts (stop `npm run dev`, then `AK_HOME=.dev-home npx tsx src/server/cli.ts host stop` ends it). Port 7777 is taken if
an installed server is running there.

Local state uses Node's built-in `node:sqlite`, which prints an experimental warning on Node 22.
The release wrapper runs Node with `--disable-warning=ExperimentalWarning`.

## Checks

```sh
npm run typecheck
npm run lint
npm test                        # unit + integration
npm run test:unit
npm run test:integration        # needs git; uses a stub `claude`
```

CI runs all of these on Linux and macOS for every push to `main` and every pull request that changes
AgentK (`.github/workflows/ci.yml`, in `extensions/agentk`; `scripts/ci-scope.sh` at the repo root
says which paths count, including the root files the tests read). Changes reach `main` only through
PRs, squash-merged once the `CI gate` and `PR title` checks pass.

## Commits

Commit subjects and PR titles are `<type>(<component>[/<area>])[!]: <summary>`, like
[Conventional Commits](https://www.conventionalcommits.org/) but with exactly these types:

- Release: `feat`, `fix`, `perf`, `deps`.
- Don't release: `ci`, `test`, `docs`, `nit` (comment-only changes, typos, formatting, refactors
  with no behavior change), `chore`.

The component names the part of the repo: `agentk` for AgentK, `meko` for Meko
(`backend/`), `repo` for the shared root (CI, `scripts/`, root docs). An optional area follows a
slash (`fix(agentk/web): …`). `!` marks a breaking change, and only on a releasing type
(`feat(agentk)!: …`, `deps(agentk)!: …`). PRs are squash-merged with the
PR title as the commit subject, so the title is what the release workflow reads: pick the type
for the PR as a whole. Check a subject or title with `sh scripts/check-title.sh "<title>"` from the
repo root; the
`PR title` workflow (`.github/workflows/pr-title.yml`) runs it on every PR title. See `.claude/skills/commit/SKILL.md` for the full rules, including: no
agent attribution.

## Dependencies

Dependabot (`.github/dependabot.yml`) checks AgentK's npm packages and GitHub Actions every Monday
(and Meko's Go modules and memory engine packages, as `chore(meko): …` and `chore(meko/memory-engine): …`, which
never release; see `backend/AGENTS.md`) and assigns its PRs to the maintainer. Packages that must move together (fastify, react, xterm,
vite, typescript with typescript-eslint, eslint) come in one PR per family, majors included.
Other minor and patch bumps come grouped (runtime, dev, actions), and each other major bump
gets its own PR. `@types/node` stays on the Node major we ship (22). `typescript` stays on 6
until typescript-eslint supports TypeScript 7
([typescript-eslint#10940](https://github.com/typescript-eslint/typescript-eslint/issues/10940)),
so its majors don't come at all; drop that `ignore` rule once it lands. Dependabot's commit subjects (and so its PR
titles) follow the title and release rules: every npm bump is `deps(agentk): …` and releases once merged
(the web UI's libraries are devDependencies bundled into the release, so dev-tool bumps release
too), while `ci(repo): …` waits for the next release. `test/unit/dependabot.test.ts` checks both. Mark a
breaking major with `!` when merging it. Node itself (`.nvmrc`, `NODE_VERSION` in
`agentk-release.yml`) is bumped by hand.

## Releases

Versions are `0.YYYYMMDD.N`: the UTC date of the release, and `N` counting up from 0 for each
release that day (`0.20260924.0`, `0.20260924.1`, …). They are valid semver, so newer always
sorts higher.

1. Squash-merge a PR into `main`. If there are `feat`, `fix`, `perf` or `deps` commits since the last release tag, the
   AgentK release workflow (`.github/workflows/agentk-release.yml`) runs the tests, picks the next version with
   the repo root's `scripts/release/next-version.sh --product agentk`, and writes notes with
   `scripts/release/release-notes.sh --product agentk`. Only AgentK's subjects count: Meko's
   (`feat(meko/…)` and the like) release Meko instead, from its own workflow. Pushes with only
   `ci`/`test`/`docs`/`nit`/`chore` commits don't release. The notes list features, fixes,
   performance and dependency changes.
2. It builds self-contained tarballs in an AlmaLinux 8 container (glibc 2.28, so they run on
   RHEL 8) for `linux-x64` and `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`
   (node-pty built from source with Xcode's clang). Each bundles Node 22, the compiled server and
   web UI, and production dependencies, and is smoke-tested (`version`, and `selftest`, which
   spawns a real pty).
3. It creates the tag and GitHub release `agentk-v<version>` (`v<version>` before releases were
   tagged by product; Meko's are `meko-v<version>`) with the tarballs, `SHA256SUMS` and
   `install.sh`, not marked "latest" (that's Meko's), and attests build provenance where the
   plan allows.
4. It deletes GitHub releases beyond the newest 5 in each lane with `scripts/release/prune-releases.sh --product agentk`, which leaves Meko's releases alone.
   A lane is the tag's prefix and the version's prerelease identifier (`-beta.2` → `beta`), or
   `stable` without one. The newest `v<version>` release is never deleted: installs from before the
   switch to `agentk-v` tags update through it.
   Tags are kept, so release notes, version numbering and the app's **What's new** (the commits
   between two tags) are unaffected.
5. Running installs see the release on their next update check (every 6 hours) and offer it in
   a "Version X is available" banner. Its **Update to the latest version** button checks again and
   installs the newest release then (which may be newer than X), without interrupting sessions. Once a version has been healthy for 5 minutes, the
   server deletes every installed version under `~/.agent-k-local/app/` except the running one and
   one previous version (the one it updated from) to roll back to by hand.

**Usage stats.** Release builds carry the collector's report URL (the full URL, ending in
`/v1/report`) from the `CALL_HOME_URL` repo variable (Settings → Secrets and variables → Actions → Variables), written into the tarball as
`CALL_HOME_URL`. Without the variable, releases send nothing. To try the report against a
collector of your own, set `[call_home] url` in the dev home's `config.toml`
(`.dev-home/config.toml`). The collector (`services/ak-local-call-home` in yugabyte/agent-k) has
an identical copy of `docs/call-home-report.schema.json`; change both together.

Run the AgentK release workflow by hand with **force** to release without feat/fix/perf/deps commits, or with
a **tag** (`agentk-v…` or `v…`) to rebuild the assets of an existing release. Only tags from after the move to
`extensions/agentk` can be rebuilt; the workflow refuses older ones.

Release tarball layout (`agentk-<version>-<linux|darwin>-<arch>.tar.gz`):

```
agentk-<version>/
  bin/agentk               launcher (packaging/agentk), handles update rollback
  bin/agent-k-local        a link to it, by its name before the rename to AgentK
  node/bin/node            bundled Node runtime
  lib/                     package.json, dist/, node_modules/, packaging/{systemd,launchd}/
  VERSION
  CALL_HOME_URL            the usage stats collector (only when the repo variable is set)
```

Each release also carries the same files as `agent-k-local-<version>-<linux|darwin>-<arch>.tar.gz`,
in `agent-k-local-<version>/`: the name and layout the updaters and installers from before the
rename look for, so those installs still update. The release workflow smoke-tests both.

## Installing a release

The repo is internal for now, so downloads go through an authenticated GitHub CLI. The repo's
latest release is Meko's: its one-line install (`gh release download --repo
yugabyte/agent-k-local --pattern install.sh --output - | sh`) installs Meko, and then
`meko install_extension agentk` installs AgentK pointed at that Meko. AgentK alone comes
from its own installer in the newest AgentK release:

```sh
gh release download "$(gh release list --repo yugabyte/agent-k-local --exclude-drafts \
  --exclude-pre-releases --limit 50 --json tagName \
  --jq '[.[].tagName | select(test("^(agentk-v|v)[0-9]"))][0]')" \
  --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
```

Pass options through `sh -s --` (`--version 0.2.0 --non-interactive --repos-root ~/code`, …).

Before anything else, the installer checks for git, curl, gh and a coding agent (Claude Code or
Kiro CLI: either one is enough) and offers to install the missing ones, showing the commands and
asking first. With neither agent there it asks which to install (`--agent claude|kiro` picks one
without asking; Kiro CLI comes from its own installer at cli.kiro.dev), and it stops when no agent
ends up installed. `agentk install` then offers Meko's own setup for each agent it finds
(Claude Code, Kiro), asking first. `--install-deps` skips the questions (and installs Claude Code
when no agent is there and `--agent` doesn't say), `--no-install-deps` never installs, and
`--deps-only` stops after that step. Where AgentK
is already installed (`app/current` and `config.toml` in its home, `~/.local/bin/agentk`,
and both services enabled or loaded), it says so and stops before any of that, unless given
`--reinstall`, `--deps-only`, a `--version` other than the installed one, or options for
`agentk install` (other than `--non-interactive`). Its tests
(`test/unit/install-script.test.ts`) run the real script against stub tools.

The installer unpacks into `~/.agent-k-local/app/<version>`, points `app/current` at it, links
`~/.local/bin/agentk` (and `~/.local/bin/agent-k-local`, the command's name before the rename,
which older services, scripts and Meko run), and runs `agentk install`, which writes `config.toml`,
checks Meko, installs the two background services (the `agentk` and `agentk-host`
systemd user units on Linux; the `com.yugabyte.agentk` and `com.yugabyte.agentk.host`
LaunchAgents on macOS), and prints the address, or for remote access the one-time setup link.
Services installed before the rename keep their old names (`agent-k-local…`) until `agentk
install` moves them; see [recovery](docs/design/recovery.md).
