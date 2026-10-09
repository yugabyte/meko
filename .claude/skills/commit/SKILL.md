---
name: commit
description: >-
  Before staging files for a git commit (or finalizing one) in this repo, check
  whether the change needs companion updates that must travel in the **same**
  commit: (1) docs under `meko/docs/content/docs/**` and the `meko/AGENTS.md` guides,
  (2) tests covering the new/changed behavior, (3) regenerated artifacts
  (Swagger, provisioner CRDs/RBAC/`zz_generated`), (4) the setup/config
  surface (`meko/LOCAL_DEV_SETUP.md`, `meko/DEVELOPMENT.md`, migrations), and
  (5) for `amp/**`, the `amp/*/architecture/*.md` docs and `amp/runbooks/*.md`.
  For each, decide whether the change makes it wrong or missing, and update it in
  the same commit. Then cut the editorial prose the change added — comments that
  restate the code, doc and runbook paragraphs that say the same thing twice —
  because reviewers spend their review budget deleting it. Also, run
  `.github/scripts/format.sh` and the linter for every language the commit
  touches before committing — including `cargo clippy` for Rust, which CI fails
  on but nothing runs for you. Use for `git commit`, `git add`-then-commit, `/commit-commands:commit`,
  `/commit-commands:commit-push-pr`, or any flow that ends in a commit.
---

# commit

A commit should land **complete**: the code, plus the companions that must stay
true alongside it — the docs people read, the tests that guard it, the generated
files that mirror it, and the setup instructions a new developer follows. A
commit that changes how the system is wired but leaves any of these stale ships a
half-truth, and **wrong docs are worse than absent ones**: they teach future
readers (and future agents) a false model, and they fail at the worst time.

So: every commit that touches a subsystem covered by one of these companions must
keep that companion accurate, **in the same commit**. Not "follow-up", not
"TODO" — same commit. If the change doesn't affect a given companion, that's
fine; you still have to look.

Accurate is not the same as long. The commit is also where you cut the prose the
change added (Step 3) — a companion nobody will read because it's padded fails the
same way a stale one does.

## When to fire

Run this check before you finalize a commit message and call `git commit`. That
means:
- Direct `git commit` invocations.
- The `/commit-commands:commit` and `/commit-commands:commit-push-pr` flows.
- Any agent step that ends with "now commit this."

If you've already `git add`-ed and you're about to write the message, that's the
moment.

## Step 1 — figure out what's staged

```
git diff --cached --name-only
git diff --cached --name-only | cut -d/ -f1 | sort -u   # which components?
```

Meko is a polyglot monorepo — the component in column 1 determines which
companions and which toolchain apply. If the only staged files are themselves
under `meko/docs/`, an `AGENTS.md`, or a top-level `*.md`, you can skip the doc arm —
but still ask whether the change needed a test that isn't staged.

## Step 2 — walk the five companions

For each companion below: **does this change touch it? If so, update it in this
commit.** A single change can hit several — do all that apply.

---

### Companion 1 — Docs (`meko/docs/content/docs/**`) and the `meko/AGENTS.md` guides

Two distinct audiences, both canonical:

- `meko/docs/content/docs/**` — the public Hugo site. What users are told is true.
- `meko/AGENTS.md` and per-component `AGENTS.md` (`meko/api_server/`, `meko/meko_ui/`,
  `meko/meko_admin/`, `meko/meko-provisioner/`) — what agents and new developers are told
  is true about layout, commands, and conventions.

A staged path matching the left column means you must read the doc on the right
and decide if it needs updating:

| Code area (staged path matches) | Doc(s) to recheck |
|---|---|
| MCP tool surface — a tool added / removed / renamed, or its arguments changed | `meko/docs/content/docs/reference/mcp-server.md` |
| `meko/cli_app/**` — a command, subcommand, or flag added / removed / renamed | `meko/docs/content/docs/reference/meko-cli.md` |
| `meko/api_server/internal/handlers/**` — a route added / removed / renamed, or a method / path / auth / body change | The handler's Swagger comments (see Companion 3), plus `meko/docs/content/docs/reference/*.md` and `meko/docs/content/docs/integrations/*.md` if the route is publicly documented |
| Auth / PAT / API-key behavior (`meko/api_server/internal/auth/**`, PAT handlers) | `meko/docs/content/docs/reference/api-keys.md`, `meko/docs/content/docs/integrations/connect-programmatically.md` |
| Memory, knowledge-base, or datapack semantics (extraction, search, scoring, quotas) | `meko/docs/content/docs/guides/working-with-memory.md`, `guides/working-with-knowledge-bases.md`, `architecture/core-concepts/**` |
| A new component, a component renamed, or a cross-component contract changed | `meko/AGENTS.md` — the layout table and Architecture Notes; `meko/docs/content/docs/architecture/how-meko-fits.md` |
| Build / run / test / lint commands changed (`meko/build.sh`, `go.mod` toolchain, `package.json` scripts, `meko/.golangci.yml`) | Root `AGENTS.md` (Build & Run / Tests / Lint blocks), `meko/DEVELOPMENT.md` |
| Folder layout or per-component convention changed | That component's own `AGENTS.md` |
| User-visible behavior shipped | `meko/docs/content/docs/changelog/**` |

The doc needs an update if the change invalidates **any** of these:

- A tool / command / route / flag list — something you added, removed, or renamed.
- A copy-pasteable example (a CLI invocation, a `curl`, a SQL snippet, a config
  block). A renamed flag or argument makes the example a lie.
- An argument or response-shape description (e.g. a tool that used to take
  `connection_string` and now takes `name`/`description`).
- A schema block or table description when you altered the live table.
- A claim about a control-flow path ("the UI's Playground talks to
  `inference_gateway`, which forwards via MCP") if you changed any node in it.
- A port, env var, or default the doc hardcodes.

If you genuinely can't find any statement the doc makes that's now wrong, fine —
but default toward "needs a touch". These are cheap and they're the things that
rot.

---

### Companion 2 — Tests

If the change adds behavior, changes an interface, or fixes a bug that no
existing test would have caught, add or extend a test **in the same commit** —
one that would have failed before your change and passes after.

Where tests live in this repo:

| Component | Location / command |
|---|---|
| `api_server` | `*_test.go` beside the code. Unit tests are mocked; integration tests need YugabyteDB on `localhost:5433`. `go test ./...` |
| `cli_app` | `*_test.go`. `go test ./...` |
| `meko-provisioner` | envtest (bundles its own apiserver + etcd). `make test` |
| `inference_gateway` | `tests/` — all upstream calls mocked, no creds needed. `pytest` |
| `meko_ui` | vitest — `npm test`; typecheck `npm run check` |

Triggers that usually mean "add/extend a test":
- A new public function, API route, MCP tool, or CLI subcommand.
- A new migration / schema change with a round-trip you can assert (insert →
  read back).
- A bug fix — encode the regression as a test.
- A behavior change in a path that already has tests (update them so they assert
  the new behavior, not the old).

Don't add ceremonial tests for pure refactors that existing coverage already
exercises. If a change genuinely can't be unit-tested (it needs a live
YugabyteDB, a real Cognito, or a cluster), say so and cite what you ran manually
instead of forcing a hollow test.

---

### Companion 3 — Regenerated artifacts

`AGENTS.md > Auto-Generated Files` lists files you must never hand-edit — but
that cuts both ways: when you change their source, you must **regenerate and
stage the output in the same commit**, or the checked-in artifact contradicts the
code.

| You changed | Regenerate |
|---|---|
| `meko/meko-provisioner/**/*_types.go`, or any kubebuilder marker (`+kubebuilder:…`) | `(cd meko/meko-provisioner && make manifests generate)` — updates `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT` |
| Swagger annotations on an `api_server` handler (`@Summary`, `@Router`, …), or `main.go`'s top-of-file Swagger block | Regenerate `meko/api_server/docs/` (`docs.go`, `swagger.json`, `swagger.yaml`) |
| A DB schema change | Add a paired migration under `meko/api_server/internal/db/migrations/{system,data}/` — **both** `.up.sql` and `.down.sql`, numbered in sequence. Never edit a migration that has already shipped |

Never hand-edit the generated output; change the source and re-run the generator.
Then `git add` the regenerated files so code and artifact land together.

---

### Companion 4 — Setup and config surface

A change that alters how the system is stood up has to update the instructions
someone follows to stand it up:

- `meko/LOCAL_DEV_SETUP.md` and `meko/DEVELOPMENT.md` — a new required env var or flag, a
  changed port, a new prerequisite service, a changed bootstrap step.
- Root `AGENTS.md > Database` — the reset/truncate snippet, if you added a
  registry table it should clear.
- Deployment config (Helm values, k8s manifests, CI workflow env) when a new
  variable must be set for the service to boot.

A new flag that silently degrades when unset still needs documenting: say what
the degraded behavior is (the pattern `AGENTS.md` already uses for
`--cognito-*` and `--langfuse-db-*`).

### Companion 5 — amp architecture docs and runbooks (`amp/**` only)

The `amp/*/architecture/*.md` docs are the canonical description of how the
storage/compute engine and the Go controller are wired **today**. A staged path
matching the left column means you must read the doc on the right and decide
whether it needs updating in this commit:

| Staged path matches | Arch doc(s) to recheck |
|---|---|
| `amp/compute/safekeeper/**`, `amp/compute/libs/safekeeper_api/**`, `amp/compute/libs/walproposer/**` | `amp/compute/architecture/safekeeper.md`, `safekeeper-lsns.md` |
| `amp/compute/pageserver/**` | `amp/compute/architecture/pageserver.md`, `pageserver-lsns.md`, `pageserver-gc.md` |
| `amp/controller/internal/registry/**`, `amp/controller/internal/core/**` | `amp/controller/architecture/sc-integration.md` |
| `amp/compute/libs/remote_storage/**` | `amp/compute/architecture/remote-storage.md` |
| `amp/compute/pgxn/**`, `amp/compute/compute_tools/**`, `amp/compute/image/**` | `amp/compute/architecture/compute.md`, `compute_ctl.md` |
| `amp/controller/migrations/**/*.sql`, anything creating/altering `yb_storage` tables | `amp/compute/architecture/yugabyte.md` + the per-subsystem doc owning the table |
| A binary's CLI surface (clap `Args` structs in `amp/*/src/bin/*.rs`) | the "Run / configure" block in the matching subsystem doc |
| **HTTP router** — a route added/removed/renamed, or a method/path/auth/body change | the matching `*-apis.md` |
| **Metrics module** (`amp/*/src/metrics.rs`) — a metric added/removed/renamed, or a label/type change | the matching `*-metrics.md` |
| Top-level topology (new binary, new wire link, removed component) | `amp/compute/architecture/high-level.md` + `README.md` |

`amp/runbooks/*.md` are the operational procedures. Update one when your change
makes it wrong — where state lives or how to inspect it, a hardcoded CLI
command/flag/port/env var, a metric name or HTTP endpoint a troubleshooting step
polls, or a startup precondition it diagnoses. **A runbook step that sends an
operator to the wrong place is actively harmful during an incident**, which is why
this is a commit-time gate and not a follow-up.

## Step 3 — cut the prose the change added

You have just written or touched comments, docs, arch docs and runbooks. Re-read
that **text** in the staged diff and delete what a reader doesn't need. Editorial
prose sends reviewers into deleting text instead of checking the change — to the
point where architecture-doc diffs stop getting reviewed at all — and each spare
sentence is one more claim that has to stay true later. The rule is
[`AGENTS.md > Prose discipline`](../../../AGENTS.md#prose-discipline--write-for-the-reader-not-for-volume);
it's a gate here because it's easy to hold at the start of a task and gone by the
end of one, and this is the last point where cutting is free.

```
git diff --cached -- '*.md'    # the doc/runbook prose; also skim added comments
```

Cut:

- Comments that restate the line below them, caption an obvious block, or narrate
  the change ("now we also handle the empty case") — the diff and the commit
  message already say that.
- Doc or runbook filler: "In other words…", a recap of context the section already
  gave, the history of what the code used to do.
- Any sentence a reader who knows this repo could skip without doing or believing
  anything differently.

Keep: invariants, ordering constraints, a workaround and its cause, units,
anything that stops the next reader from making a wrong change. **Motivation is
not filler** — why a design is the way it is is exactly what a reader can't
recover from the code, in a doc or a PR description alike. What's being cut is
text that says the same thing twice, not text that explains.

Don't over-correct. The goal is *fewer, load-bearing* words — not stripped docs.
Deleting a section that describes live behavior is a worse outcome than leaving it
wordy, and this applies to **your own diff**, not a cleanup tour of files you
didn't touch.

## Step 4 — format and lint clean

If the commit touches code, it lands **formatted and lint-clean** for every
language it touches — same as the companions above, this is part of "landing
complete". Run only the toolchains for the components in your staged set
(Step 1).

### Formatting (Go, Rust, Python, shell, TypeScript/JS/CSS)

One script formats them all, with the pinned tool versions CI's required
`Format` check uses (it runs the same script with `--check`):

```
.github/scripts/format.sh go ts    # the languages you touched (go rust python shell ts); no args = all
git add -u
```

Go's gofmt comes from the release in `meko/api_server/go.mod`, fetched once
through `GOTOOLCHAIN` if your `go` is a different one, so a local run and CI
format the same way. `main` is kept formatted by that check, so the script only
rewrites what you changed. It exits 2 and says what to install when a tool is
missing (`go`, which also builds shfmt; `cargo` with rustfmt; ruff 0.11.2 via
uv or pip; `npx`, for prettier).

### Go (`api_server`, `cli_app`, `meko-provisioner`, `docs`)

Lint the module you changed — `meko/.golangci.yml` at the repo root governs
`cli_app` and `api_server`:

```
(cd meko/api_server       && golangci-lint run ./...)
(cd meko/cli_app          && golangci-lint run ./...)
(cd meko/meko-provisioner && GOTOOLCHAIN=go1.25.8 make lint)
```

Note `goimports.local-prefixes: meko` — local imports go in their own group.
Line limit 120 for Go.

### TypeScript (`meko_ui`, `meko_admin`)

```
(cd meko/meko_ui    && npm run check)     # tsc
(cd meko/meko_admin && npm run check)
```

### Python (any `*.py`; `system_agent`, `inference_gateway`)

```
(cd meko/system_agent && mypy . && pycodestyle .)    # max-line-length 100 (tox.ini)
(cd meko/inference_gateway && pytest)
```

ruff picks `amp/pyproject.toml` for files under `amp/` and the root `ruff.toml`
for everything else. Both set `force-exclude`, so even a vendored file named
explicitly (`amp/deploy/lru/amp-time/vendor/`) is left alone.

### Rust (`amp/**` — `*.rs`, `Cargo.toml`, build scripts)

**Nothing runs clippy for you.** The `Amp Compute` CI job fails on any clippy
warning, but `cargo build` only compiles, so if you skip this you find out from
a red PR. Format with `.github/scripts/format.sh rust` (above), then run clippy,
which must print nothing:

```
(cd amp && cargo clippy --workspace --all-targets) 2>&1 | grep -E "^warning: " | grep -v generated
```

If it doesn't: `cargo clippy --fix --workspace --all-targets --allow-dirty
--allow-staged` clears the machine-applicable lints; fix the rest by hand, then
`cargo build --workspace` to confirm the fixes compile. Reach for a **targeted**
`#[allow(clippy::...)]` with a one-line reason only for design-choice lints where
a refactor would be riskier than the lint — `too_many_arguments` on a wide wiring
signature, `large_enum_variant` where boxing would ripple through the type. Never
blanket-allow at crate level.

Resolve every warning rather than committing past it — a warning that lands is
one the next person has to wade past to see whether *their* change added
anything. Prefer a real fix over a suppression; when you must suppress, make it
targeted and give a one-line reason.

Format only the code you're landing: if a formatter rewrites files unrelated to
your change, leave those out of this commit.

Skip this step only when nothing staged is code (pure meko/docs/config commits). It is
**not** optional just because the warnings are pre-existing.

## Step 5 — update in the same commit

Edit the companions that apply. Do **not** split into a separate "meko/docs/tests
follow-up" commit — if the code lands and a companion still says the old thing,
even for one commit, search/agent context (and users) pick up the wrong model.

Re-stage and commit everything together:

```
git add <code files> <docs, AGENTS.md, tests, regenerated artifacts you edited>
git commit -m "..."
```

### Subject format

The commit subject follows:

```
[<KEY>-<num>] <component>: <title>              # with a Jira ticket
[#<issue>] <component>: <title>                 # with a GitHub issue in this repo
[<KEY>-<num>][#<issue>] <component>: <title>    # tracked in several places
```

- **`[<KEY>-<num>]`** — the tracking ticket, first so `git log --oneline` is
  greppable by ticket. Any Jira project's key works (`MEKO-`, `AMP-`, `CLOUDGA-`,
  `PLAT-`, …): use the ticket that actually tracks the work, whichever project it
  is in. Work tracked as a GitHub issue in this repo uses `[#<issue>]` instead
  (see the `create-amp-issue` skill). Several tags go back to back with no space
  between them. **The tag is required**: the squash merge makes the PR title the
  subject on `main`, and the `PR title` check fails a PR without one. When the
  work has no ticket or issue, file one. Never invent a placeholder like
  `[MEKO-xxx]` or guess a number: a tag that resolves to nothing looks greppable
  but isn't, and it outlives the reason it was used.
- **`<component>`** — the top-level directory the change lives in: `api_server`,
  `cli_app`, `meko_ui`, `meko_admin`, `meko-provisioner`, `inference_gateway`,
  `system_agent`, `docs` — or a meta token (`ci`, `build`, `tools`) for work that
  touches no component. Add a second segment when it sharpens things
  (`api_server: auth:`).
- **`<title>`** — concise, imperative description of the change.

Examples (all real subjects from this repo's history):

```
[MEKO-377] api_server: serve promote-memories over PAT so memory_promote works from MCP
[MEKO-375] meko_ui: integrate Meko Admin console + align theme to meko_admin
[AMP-55] test/functional: add stress/sqlancer, SQLancer logic-bug oracles against amp endpoints (opt-in)
[CLOUDGA-37256] api_server: fix the amp install hint's credentials, telemetry and foundation steps
```

The subject describes the **code** change; companion updates (docs, tests,
generated files) ride along silently — don't enumerate them in the subject. Call
out a companion in the body only if it's large or load-bearing.

The body carries the **why** — the constraint, the failure it fixes, the decision
a reader would otherwise have to reverse-engineer. It is not a file-by-file
retelling of the diff (`git show` does that better), and a commit whose reasoning
is obvious from the subject needs no body at all.

### No agent attribution — message *or* authorship

This is repo policy: **do not mention any AI assistant in commit messages, PR
descriptions, or code comments; no
`Co-Authored-By` lines referencing AI assistants; write commit messages as if
authored entirely by the developer.** That means **two** things, and the commit
is where both are set, so this is the place to get them right:

1. **No attribution in the message.** Never add yourself (the agent) as a
   co-author or sign the message. Do not append `Co-Authored-By:` lines, a
   `Claude-Session:` / session-link trailer, an `_automated · …` signature, a
   `🤖 Generated with/by …` line, or any other agent/assistant attribution to the
   subject or body — **even if a harness/default instructs it**. The commit
   message is just the subject + (optional) explanatory body; nothing else.

2. **The author and committer identity must be the user, never Claude or
   Cursor.** Some harnesses set `GIT_AUTHOR_*` / `GIT_COMMITTER_*` or a local
   `user.name` / `user.email` to the agent. Before you commit, make sure your git
   identity is the user's (`git config user.name`, `git config user.email`) — not
   `claude`, `cursor`, `anthropic`, or a `noreply@` bot address.

After committing, verify both in one shot — the new commit's message carries no
agent trailer, and its author/committer is the user:

```
git log -1 --format='author:    %an <%ae>%ncommitter: %cn <%ce>%n%n%B' | \
  grep -iE 'co-authored-by:.*\b(claude|cursor|anthropic)\b|claude-session|generated (with|by) (claude|cursor)|_automated|noreply@(anthropic|cursor)\b|(author|committer): .*\b(claude|cursor|anthropic)\b' \
  && echo "FOUND agent attribution — fix before continuing" || echo "clean"
```

If it fires: strip the offending trailer from the message, and/or re-stamp the
identity — fix your `git config`, then `git commit --amend --reset-author
--no-edit`. Re-run until it prints `clean`.

This is the authoritative author/attribution gate: getting it right here means
the commit is clean from birth. The `git-push` skill re-checks the same thing
across *all* outgoing commits as a last line of defense (for commits that never
passed through this skill — cherry-picks, rebases, external tools), but the fix
is cheapest here, at commit time.

## Step 6 — reference and close the tracked ticket

If this commit advances or completes a tracked ticket or GitHub issue:

- **Reference it** with the subject tag (see Subject format above) so the work is
  linked: `[<KEY>-<num>]` for a Jira ticket in any project (`[MEKO-12]`,
  `[AMP-55]`, `[CLOUDGA-37256]`), `[#<issue>]` for a GitHub issue in this repo.
  The tag is required — file a ticket when there is none; never invent a
  placeholder or guess a number.
- **Close it when the work ships.** Put `Closes <full ticket or issue URL>` in the
  PR body so the merge closes it: GitHub closes the issue, and the
  `jira-on-merge` workflow moves an `AMP` or `MEKO` ticket (see the `open-pr`
  skill); move a ticket in another Jira project yourself once the PR merges.
  Link with full URLs, never bare `#123`.

Don't leave a shipped fix's ticket open "to close later" — that gap is exactly
when the tracker misleads. If the commit only partially advances the ticket,
leave it open and say what's left.

## When you genuinely don't need to update

Skip a companion only when you can name **which** rule applies — not just "feels
minor." If you can't categorize the skip, default to checking.

- **Docs:** pure bug fix inside an internal function the docs describe only at the
  component level; comment/typo/whitespace; dependency bump that doesn't change
  documented behavior; build/CI plumbing that doesn't change how a user or
  developer interacts with the system.
- **Tests:** pure refactor with unchanged behavior already under test;
  comment/formatting/dep-bump; a change only a live-infra run can cover (then run
  it and cite the result).
- **Regenerated artifacts:** you didn't touch a `*_types.go`, a kubebuilder
  marker, a Swagger annotation, or the schema.
- **Setup/config:** the change doesn't alter any command, port, env var, flag, or
  prerequisite that `meko/LOCAL_DEV_SETUP.md` / `meko/DEVELOPMENT.md` reference.

## Anti-patterns

- **"I'll update the doc/test in a follow-up."** No. The follow-up never happens,
  and the companion lies for the gap. Same commit.
- **Hand-editing a generated file** (`zz_generated.*.go`, `config/crd/bases/*.yaml`,
  `meko/api_server/docs/swagger.json`) instead of changing the source and re-running
  the generator. The next `make manifests generate` silently reverts you.
- **Editing a migration that already shipped.** Migrations run on every
  `api_server` boot; add a new numbered pair instead.
- **Cargo-culting.** Don't add a ceremonial test or a changelog paragraph for
  every commit. Many commits just edit an existing section / extend an existing
  test. Volume is not progress.
- **Padding the companion instead of updating it.** A doc or runbook edit that
  adds three paragraphs of framing around the one sentence that actually changed
  makes the reviewer's job harder, not easier — and it is why architecture-doc
  diffs go unreviewed. Same for comments that narrate the diff.
- **Committing with a red linter** because the warnings were already there.
- **Splitting a companion update into a separate PR** when the underlying code
  change is on this branch. Code and its companions travel together.

## Failure mode this prevents

This repo has a visible trail of it. `datapack_update`'s arguments changed to
`name`/`description`, but `meko/docs/content/docs/reference/mcp-server.md` kept
telling users it took a `connection_string` — repaired later by a standalone
docs PR (`docs(mcp-server): datapack_update takes name/description, not
connection_string`, #366). The `scope` attribute was removed from the MCP server
tools; the docs kept documenting it until another cleanup PR removed the
references (#368). A CLI example went stale the same way (#346).

Each of those is a period during which the canonical, user-facing description of
Meko was a fabrication, plus a second PR's worth of work to undo. Every one would
have been a two-line edit riding along in the commit that changed the behavior.
That's what this skill is for.
