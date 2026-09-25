# Contributing to agent-k-local

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
`docs:`, …). release-please reads them to pick the next version and write the changelog.
See `.claude/skills/commit/SKILL.md` for the full rules, including: no agent attribution.

## Releases

1. Merge commits to `main`. release-please keeps a release PR open with the next version and the
   changelog.
2. Merge the release PR. release-please tags `vX.Y.Z` and creates the GitHub release.
3. The same workflow (`.github/workflows/release-please.yml`) builds self-contained tarballs in an
   AlmaLinux 8 container (glibc 2.28, so they run on RHEL 8) for `linux-x64` and `linux-arm64`,
   each bundling Node 22, the compiled server and web UI, and production dependencies. It uploads
   them with `SHA256SUMS` and `install.sh`, and attests build provenance where the plan allows.
4. Running installs pick the release up on their next update check (every 6 hours) and update
   themselves without interrupting sessions.

To rebuild the assets for an existing tag, run the Release workflow manually with that tag.

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
