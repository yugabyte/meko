# meko

This repository is organised one directory per project, with shared components at the
top level.

| Path | What |
|---|---|
| [`meko/`](meko/) | The Meko platform — CLI, API server, UI, inference gateway, provisioner, docs. Start at [`meko/README.md`](meko/README.md). |
| [`ycp/`](ycp/) | YCP — the BYOC data plane: pull agent, per-datapack MCP server, and the wire types both sides share. Shared between meko and amp, which is why it sits at the root. See [`ycp/ARCHITECTURE.md`](ycp/ARCHITECTURE.md) and [`ycp/DEPLOY.md`](ycp/DEPLOY.md). |

`amp/` will join as a sibling tree; see
[#393](https://github.com/yugabyte/meko/issues/393) for the migration.

Repo-wide configuration stays at the root: `.github/` (GitHub only runs workflows
from there), `.goreleaser.yml`, `.editorconfig`, `.gitignore` and `.claude/`.
