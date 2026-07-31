# meko

This repository is organised one directory per project, with shared components at the
top level.

| Path | What |
|---|---|
| [`amp/`](amp/) | AMP — Postgres-compatible storage and its control plane: pageserver, safekeeper, compute extensions, controller, UI. Start at [`amp/README.md`](amp/README.md). |
| [`meko/`](meko/) | The Meko platform — CLI, API server, UI, inference gateway, provisioner, docs. Start at [`meko/README.md`](meko/README.md). |
| [`ycp/`](ycp/) | YCP — the BYOC data plane: pull agent, per-datapack MCP server, and the wire types both sides share. Shared between meko and amp, which is why it sits at the root. See [`ycp/ARCHITECTURE.md`](ycp/ARCHITECTURE.md) and [`ycp/DEPLOY.md`](ycp/DEPLOY.md). |

Repo-wide configuration stays at the root: `.github/` (GitHub only runs workflows
from there), `.gitmodules`, `.goreleaser.yml`, `.editorconfig`, `.gitignore` and
`.claude/`.

## After cloning

```bash
git submodule update --init --recursive   # amp's vendored PostgreSQL
git config log.follow true                # git log <file> spans the tree moves
```

Both trees were moved into subdirectories by rename commits, so without `log.follow`
a `git log` on a file stops at the move and shows one commit. `git blame` is
unaffected. Directory-level log (`git log -- amp/controller`) can't be recovered —
`--follow` only takes a single file.
