# Contributing to AgentK Local

## Development setup

Requirements: Linux, Node 22, tmux ≥ 2.7, git, and Claude Code. Meko (an API key plus the
`meko-agent-skills` Claude Code plugin) is required to create projects.

```sh
nvm use 22                      # .nvmrc pins the major version

# node-pty is a native module that needs a C++20 compiler. On RHEL 8 the system g++ (8.5)
# is too old; use gcc-toolset-13:
export CC=/opt/rh/gcc-toolset-13/root/usr/bin/gcc
export CXX=/opt/rh/gcc-toolset-13/root/usr/bin/g++
npm ci

npm run dev                     # API server on 127.0.0.1:7777 + Vite on 127.0.0.1:5173
```

Open http://127.0.0.1:5173. A dev server keeps its state in `~/.agent-k-local` unless you set
`AK_HOME`, e.g. `AK_HOME=/tmp/ak-dev npm run dev`.

Local state uses Node's built-in `node:sqlite`, which prints an experimental warning on Node 22.
The release wrapper runs Node with `--disable-warning=ExperimentalWarning`.

## Checks

```sh
npm run typecheck
npm run lint
npm test                        # unit + integration
npm run test:unit
npm run test:integration        # needs tmux and git; uses a stub `claude`
```

CI runs all of these on every push and pull request (`.github/workflows/ci.yml`).

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `perf:`,
`docs:`, …). The release workflow reads them to decide whether to release and to write the notes.
See `.claude/skills/commit/SKILL.md` for the full rules, including: no agent attribution.

## Releases

Versions are `0.YYYYMMDD.N`: the UTC date of the release, and `N` counting up from 0 for each
release that day (`0.20260924.0`, `0.20260924.1`, …). They are valid semver, so newer always
sorts higher.

1. Push to `main`. If there are `feat`, `fix` or `perf` commits since the last release tag, the
   Release workflow (`.github/workflows/release.yml`) runs the tests, picks the next version with
   `scripts/next-version.sh`, and writes notes with `scripts/release-notes.sh`. Pushes with only
   `docs`/`chore`/`ci`/`test`/`refactor` commits don't release.
2. It builds self-contained tarballs in an AlmaLinux 8 container (glibc 2.28, so they run on
   RHEL 8) for `linux-x64` and `linux-arm64`, each bundling Node 22, the compiled server and web
   UI, and production dependencies. Each tarball is smoke-tested (`version` and `selftest`).
3. It creates the tag and GitHub release `v<version>` with the tarballs, `SHA256SUMS` and
   `install.sh`, and attests build provenance where the plan allows.
4. Running installs pick the release up on their next update check (every 6 hours) and update
   themselves without interrupting sessions.

Run the Release workflow by hand with **force** to release without feat/fix/perf commits, or with
a **tag** to rebuild the assets of an existing release.

Release tarball layout:

```
agent-k-local-<version>/
  bin/agent-k-local        launcher (packaging/agent-k-local), handles update rollback
  node/bin/node            bundled Node runtime
  lib/                     package.json, dist/, node_modules/, packaging/systemd/
  VERSION
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

The installer unpacks into `~/.agent-k-local/app/<version>`, points `app/current` at it, links
`~/.local/bin/agent-k-local`, and runs `agent-k-local install`, which writes `config.toml`,
checks Meko, installs the `agent-k-local` and `agent-k-local-tmux` systemd user units, and prints
the one-time setup link.
