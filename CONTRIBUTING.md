# Contributing to AgentK Local

## Development setup

Requirements: Linux or macOS, Node 22, git, and Claude
Code. Meko (an API key plus the `meko-agent-skills` Claude Code plugin) is required to create
projects.

```sh
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
use another folder), so it never opens an installed AgentK Local's database, hook socket or session
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
npm run test:integration        # needs git; uses a stub `claude` (tmux only for the move-off-tmux tests)
```

CI runs all of these on Linux and macOS for every push and pull request (`.github/workflows/ci.yml`).
Changes reach `main` only through PRs, squash-merged once CI and the PR title check pass.

## Commits

Commit subjects and PR titles are `<type>[(<component>)][!]: <summary>`, like
[Conventional Commits](https://www.conventionalcommits.org/) but with exactly these types:

- Release: `feat`, `fix`, `perf`, `deps`.
- Don't release: `ci`, `test`, `docs`, `nit` (comment-only changes, typos, formatting, refactors
  with no behavior change), `chore`.

The component is optional and any lowercase name (`fix(web): …`). `!` marks a breaking change,
and only on a releasing type (`feat!: …`, `deps!: …`). PRs are squash-merged with the
PR title as the commit subject, so the title is what the release workflow reads: pick the type
for the PR as a whole. Check a subject or title with `sh scripts/check-title.sh "<title>"`; the
`PR title` workflow (`.github/workflows/pr-title.yml`) runs it on every PR title. See `.claude/skills/commit/SKILL.md` for the full rules, including: no
agent attribution.

## Dependencies

Dependabot (`.github/dependabot.yml`) checks npm packages and GitHub Actions every Monday and
assigns its PRs to the maintainer. Packages that must move together (fastify, react, xterm,
vite, typescript with typescript-eslint, eslint) come in one PR per family, majors included.
Other minor and patch bumps come grouped (runtime, dev, actions), and each other major bump
gets its own PR. `@types/node` stays on the Node major we ship (22). `typescript` stays on 6
until typescript-eslint supports TypeScript 7
([typescript-eslint#10940](https://github.com/typescript-eslint/typescript-eslint/issues/10940)),
so its majors don't come at all; drop that `ignore` rule once it lands. Dependabot's commit subjects (and so its PR
titles) follow the title and release rules: every npm bump is `deps: …` and releases once merged
(the web UI's libraries are devDependencies bundled into the release, so dev-tool bumps release
too), while `ci: …` waits for the next release. `test/unit/dependabot.test.ts` checks both. Mark a
breaking major with `!` when merging it. Node itself (`.nvmrc`, `NODE_VERSION` in
`release.yml`) is bumped by hand.

## Releases

Versions are `0.YYYYMMDD.N`: the UTC date of the release, and `N` counting up from 0 for each
release that day (`0.20260924.0`, `0.20260924.1`, …). They are valid semver, so newer always
sorts higher.

1. Squash-merge a PR into `main`. If there are `feat`, `fix`, `perf` or `deps` commits since the last release tag, the
   Release workflow (`.github/workflows/release.yml`) runs the tests, picks the next version with
   `scripts/next-version.sh`, and writes notes with `scripts/release-notes.sh`. Pushes with only
   `ci`/`test`/`docs`/`nit`/`chore` commits don't release. The notes list features, fixes,
   performance and dependency changes.
2. It builds self-contained tarballs in an AlmaLinux 8 container (glibc 2.28, so they run on
   RHEL 8) for `linux-x64` and `linux-arm64`, and on a `macos-15` runner for `darwin-arm64`
   (node-pty built from source with Xcode's clang). Each bundles Node 22, the compiled server and
   web UI, and production dependencies, and is smoke-tested (`version`, and `selftest`, which
   spawns a real pty).
3. It creates the tag and GitHub release `v<version>` with the tarballs, `SHA256SUMS` and
   `install.sh`, and attests build provenance where the plan allows.
4. It deletes GitHub releases beyond the newest 5 in each lane with `scripts/prune-releases.sh`.
   A lane is the version's prerelease identifier (`-beta.2` → `beta`), or `stable` without one.
   Tags are kept, so release notes and version numbering are unaffected.
5. Running installs see the release on their next update check (every 6 hours) and offer it in
   an "Update to vX" banner; one click installs it without interrupting sessions. Once a version has been healthy for 5 minutes, the
   server deletes every installed version under `~/.agent-k-local/app/` except the running one and
   one previous version (the one it updated from) to roll back to by hand.

**Usage stats.** Release builds carry the collector's report URL (the full URL, ending in
`/v1/report`) from the `CALL_HOME_URL` repo variable (Settings → Secrets and variables → Actions → Variables), written into the tarball as
`CALL_HOME_URL`. Without the variable, releases send nothing. To try the report against a
collector of your own, set `[call_home] url` in the dev home's `config.toml`
(`.dev-home/config.toml`). The collector (`services/ak-local-call-home` in yugabyte/agent-k) has
an identical copy of `docs/call-home-report.schema.json`; change both together.

Run the Release workflow by hand with **force** to release without feat/fix/perf/deps commits, or with
a **tag** to rebuild the assets of an existing release (a tag from before macOS support gets its
Linux tarballs only).

Release tarball layout:

```
agent-k-local-<version>/
  bin/agent-k-local        launcher (packaging/agent-k-local), handles update rollback
  node/bin/node            bundled Node runtime
  lib/                     package.json, dist/, node_modules/, packaging/{systemd,launchd}/
  VERSION
  CALL_HOME_URL            the usage stats collector (only when the repo variable is set)
```

## Installing a release

The repo is internal for now, so downloads go through an authenticated GitHub CLI:

```sh
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh
```

Pass options through `sh -s --`, for example:

```sh
gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - \
  | sh -s -- --version 0.2.0 --non-interactive --repos-root ~/code
```

Before anything else, the installer checks for git, curl, gh and Claude Code and offers to
install the missing ones (and `agent-k-local install` offers Meko's own setup for Claude Code),
showing the commands and asking first (`--install-deps` to skip the question,
`--no-install-deps` to never install, `--deps-only` to stop after that step). Where AgentK Local
is already installed (`app/current` and `config.toml` in its home, `~/.local/bin/agent-k-local`,
and both services enabled or loaded), it says so and stops before any of that, unless given
`--reinstall`, `--deps-only`, a `--version` other than the installed one, or options for
`agent-k-local install` (other than `--non-interactive`). Its tests
(`test/unit/install-script.test.ts`) run the real script against stub tools.

The installer unpacks into `~/.agent-k-local/app/<version>`, points `app/current` at it, links
`~/.local/bin/agent-k-local`, and runs `agent-k-local install`, which writes `config.toml`,
checks Meko, installs the two background services (the `agent-k-local` and `agent-k-local-host`
systemd user units on Linux; the `com.yugabyte.agent-k-local` and `com.yugabyte.agent-k-local.host`
LaunchAgents on macOS), and prints the address, or for remote access the one-time setup link.
