# AGENTS.md

Guidance for AI coding agents working in this repository.

Work is organised one directory per project, with shared components at the top level,
so **the guide you need is the one covering the tree you are editing**:

| Editing | Read |
|---|---|
| `amp/**` | [`amp/AGENTS.md`](amp/AGENTS.md) — AMP's own layout, build and test commands |
| `meko/**` | [`meko/AGENTS.md`](meko/AGENTS.md) — layout, build/run/test/lint commands, architecture notes, per-component guides |
| `ycp/**` | this file, plus [`ycp/ARCHITECTURE.md`](ycp/ARCHITECTURE.md) and [`ycp/DEPLOY.md`](ycp/DEPLOY.md) |

## Reading paths in this repo

Paths in root-level files — this one, and everything in `.claude/skills/` — are
relative to the **repository root**, so they carry the `meko/` or `amp/` prefix. Commands
quoted inside `meko/AGENTS.md` or `amp/AGENTS.md` are relative to that file's own
directory. Both are correct; check which altitude you are reading before copying a path.

## Required one-time git config

```bash
git config log.follow true
```

Both trees were moved into subdirectories by rename commits, and `git log` does no
rename detection without this. **Don't conclude a file has no history from a short
`git log`** — check the config first. `git blame` is unaffected. Directory-level log
can't be recovered; use a file, or an old SHA (all pre-move SHAs still resolve).

## `ycp/` — shared, top-level

The BYOC **data plane** for YCP (Yugabyte Control Plane): the pull agent
(`cmd/agent`), the per-datapack MCP server (`cmd/dpmcp`), the wire types both sides
share (`internal/api`), and the kind e2e. Its own `go.mod`. The control plane itself
is `meko/api_server --control-plane-mode`.

It lives at the root rather than under `meko/` because it is shared with amp.

```bash
# unit tests need nothing running
(cd ycp && go test ./...)

# build + deploy end-to-end: builds the api-server and agent images, stands the
# control plane up on a throwaway kind cluster, and asserts
# register -> enroll -> READY -> restart-without-re-enrolling.
# Needs kind + docker or podman.
(cd ycp && ./test/e2e/run.sh)
```

Images publish to the platform ECR registry on merge to `main`
(`.github/workflows/ycp-images.yml`).

## Repo-wide conventions

### Prerequisites

An **authenticated GitHub CLI (`gh`)** is required — this repo is private, so
unauthenticated access silently returns the wrong thing rather than failing. Check
with `gh auth status`; if it is missing, stop and have the user run `gh auth login`
(in Claude Code, `! gh auth login` so the output lands in the conversation). Do not
work around it by fetching GitHub URLs with a web fetcher or asking the user to paste
output by hand.

### Branch naming

Branches are **`<username>/<kebab-topic>`** or **`feature/<kebab-topic>`**.
`.claude/hooks/check-branch-name.sh` blocks a non-conforming `git push`, and the
`branch-names-require-prefix` ruleset enforces the weaker half remotely. Full rules
and the hook's test suite: [`meko/AGENTS.md`](meko/AGENTS.md).

### Tracking references

Commit subjects and PR titles carry `[MEKO-<num>]` for meko work or `[#<issue>]` for
AMP work, and **omit the tag entirely** when there is genuinely no ticket — never
invent a placeholder or guess a number.

#### `[#N]` in commits that predate the monorepo merge means an *amp* number

AMP's 69 open issues were transferred out of `yugabyte/amp` and **renumbered**, so a
`[#N]` tag on any commit from before the merge refers to amp's old sequence, not to
`#N` in this repo. amp's numbering reached **#447** (issues and pull requests share one
sequence), so treat any `[#N]` with **N ≤ 447** on a pre-merge commit as an amp number
and resolve it before assuming it points here.

| amp numbers | where they are now |
|---|---|
| the 69 **open** issues, scattered over `#5`–`#440` | moved here as `#432`–`#500` — [mapping table](https://github.com/yugabyte/meko/issues/393#issuecomment-5139709911) |
| the 175 **closed** issues, up to `#433` | left in `yugabyte/amp`; still resolve there |
| pull requests | left in `yugabyte/amp` |

The ranges overlap, which is the trap: `#432`–`#500` here *are* the migrated amp issues,
but everything below that is meko's own. A live example — `[#244]`, the tag on AMP's
ImageVolume work:

- amp#244 "control_plane: support Postgres extensions via k8s ImageVolume" → now
  [meko#474](https://github.com/yugabyte/meko/issues/474)
- meko#244 is "Add discord link to Meko app and remove Slack link" — an unrelated meko PR

Old `yugabyte/amp` issue URLs redirect to the new location, so prefer pasting a full URL
over a bare `#N`.

### Reviewing

[`meko/REVIEW.md`](meko/REVIEW.md) is the per-component review guide for the meko
tree.

### On-demand CI (`/ci` directives)

Some AMP suites are too heavy for every PR and are opt-in: add a `/ci <suite>` line to
the **PR description**, before opening it — editing the description afterwards fires
nothing.

| Directive | Runs |
|---|---|
| `amp_integration_slow` | slow integration scenario suite. Also automatic when the diff touches `amp/test/integration/compute/slow/` |
| `amp_generate_image` | controller container image build |
| `amp_deploy_gke` | build + push four images to Quay, deploy to the itest-dev GKE cluster, smoke test. Also nightly on `main` |

All three are `amp_`-prefixed because they belong to AMP; there is nothing equivalent for
meko yet. Full contract, including what makes a change correctness-sensitive:
[`amp/docs/ci-directives.md`](amp/docs/ci-directives.md).

### Agent skills (`.claude/skills/`)

Pointers only; each skill is the authority on its own subject.

| Skill | Read it before |
|---|---|
| `commit` | any commit — it gates the companions (docs, tests, generated files, setup) that must travel in the same commit |
| `git-push` | pushing a branch, or refreshing an existing PR |
| `open-pr` | opening a new PR |
| `merge-pr` | merging |
| `create-amp-issue` | filing or updating a GitHub issue for AMP work |
