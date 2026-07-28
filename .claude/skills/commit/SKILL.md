---
name: commit
description: >-
  Before staging files for a git commit (or finalizing one) in this repo, check
  whether the change needs companion updates that must travel in the **same**
  commit: (1) docs under `docs/content/docs/**` and the `AGENTS.md` guides,
  (2) tests covering the new/changed behavior, (3) regenerated artifacts
  (Swagger, provisioner CRDs/RBAC/`zz_generated`), and (4) the setup/config
  surface (`LOCAL_DEV_SETUP.md`, `DEVELOPMENT.md`, migrations). For each, decide
  whether the change makes it wrong or missing, and update it in the same
  commit. Also, run the formatter and linter for every language the commit
  touches before committing. Use for `git commit`, `git add`-then-commit,
  `/commit-commands:commit`, `/commit-commands:commit-push-pr`, or any flow that
  ends in a commit.
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
under `docs/`, an `AGENTS.md`, or a top-level `*.md`, you can skip the doc arm —
but still ask whether the change needed a test that isn't staged.

## Step 2 — walk the four companions

For each companion below: **does this change touch it? If so, update it in this
commit.** A single change can hit several — do all that apply.

---

### Companion 1 — Docs (`docs/content/docs/**`) and the `AGENTS.md` guides

Two distinct audiences, both canonical:

- `docs/content/docs/**` — the public Hugo site. What users are told is true.
- `AGENTS.md` (root) and per-component `AGENTS.md` (`api_server/`, `meko_ui/`,
  `meko_admin/`, `meko-provisioner/`) — what agents and new developers are told
  is true about layout, commands, and conventions.

A staged path matching the left column means you must read the doc on the right
and decide if it needs updating:

| Code area (staged path matches) | Doc(s) to recheck |
|---|---|
| MCP tool surface — a tool added / removed / renamed, or its arguments changed | `docs/content/docs/reference/mcp-server.md` |
| `cli_app/**` — a command, subcommand, or flag added / removed / renamed | `docs/content/docs/reference/meko-cli.md` |
| `api_server/internal/handlers/**` — a route added / removed / renamed, or a method / path / auth / body change | The handler's Swagger comments (see Companion 3), plus `docs/content/docs/reference/*.md` and `docs/content/docs/integrations/*.md` if the route is publicly documented |
| Auth / PAT / API-key behavior (`api_server/internal/auth/**`, PAT handlers) | `docs/content/docs/reference/api-keys.md`, `docs/content/docs/integrations/connect-programmatically.md` |
| Memory, knowledge-base, or datapack semantics (extraction, search, scoring, quotas) | `docs/content/docs/guides/working-with-memory.md`, `guides/working-with-knowledge-bases.md`, `architecture/core-concepts/**` |
| A new component, a component renamed, or a cross-component contract changed | Root `AGENTS.md` — the layout table and Architecture Notes; `docs/content/docs/architecture/how-meko-fits.md` |
| Build / run / test / lint commands changed (`build.sh`, `go.mod` toolchain, `package.json` scripts, `.golangci.yml`) | Root `AGENTS.md` (Build & Run / Tests / Lint blocks), `DEVELOPMENT.md` |
| Folder layout or per-component convention changed | That component's own `AGENTS.md` |
| User-visible behavior shipped | `docs/content/docs/changelog/**` |

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
| `meko-provisioner/**/*_types.go`, or any kubebuilder marker (`+kubebuilder:…`) | `(cd meko-provisioner && make manifests generate)` — updates `config/crd/bases/*.yaml`, `config/rbac/role.yaml`, `**/zz_generated.*.go`, `PROJECT` |
| Swagger annotations on an `api_server` handler (`@Summary`, `@Router`, …), or `main.go`'s top-of-file Swagger block | Regenerate `api_server/docs/` (`docs.go`, `swagger.json`, `swagger.yaml`) |
| A DB schema change | Add a paired migration under `api_server/internal/db/migrations/{system,data}/` — **both** `.up.sql` and `.down.sql`, numbered in sequence. Never edit a migration that has already shipped |

Never hand-edit the generated output; change the source and re-run the generator.
Then `git add` the regenerated files so code and artifact land together.

---

### Companion 4 — Setup and config surface

A change that alters how the system is stood up has to update the instructions
someone follows to stand it up:

- `LOCAL_DEV_SETUP.md` and `DEVELOPMENT.md` — a new required env var or flag, a
  changed port, a new prerequisite service, a changed bootstrap step.
- Root `AGENTS.md > Database` — the reset/truncate snippet, if you added a
  registry table it should clear.
- Deployment config (Helm values, k8s manifests, CI workflow env) when a new
  variable must be set for the service to boot.

A new flag that silently degrades when unset still needs documenting: say what
the degraded behavior is (the pattern `AGENTS.md` already uses for
`--cognito-*` and `--langfuse-db-*`).

## Step 3 — format and lint clean

If the commit touches code, it lands **formatted and lint-clean** for every
language it touches — same as the companions above, this is part of "landing
complete". Run only the toolchains for the components in your staged set
(Step 1).

### Go (`api_server`, `cli_app`, `meko-provisioner`, `docs`)

```
gofmt -l <the modules you touched>     # silent == clean
gofmt -w <any files it listed> && git add -u
```

Then lint the module you changed — `.golangci.yml` at the repo root governs
`cli_app` and `api_server`:

```
(cd api_server       && golangci-lint run ./...)
(cd cli_app          && golangci-lint run ./...)
(cd meko-provisioner && GOTOOLCHAIN=go1.25.8 make lint)
```

Note `goimports.local-prefixes: meko` — local imports go in their own group.
Line limit 120 for Go.

### TypeScript (`meko_ui`, `meko_admin`)

```
(cd meko_ui    && npm run check)     # tsc
(cd meko_admin && npm run check)
```

### Python (`system_agent`, `inference_gateway`)

```
(cd system_agent && mypy . && pycodestyle .)    # max-line-length 100 (tox.ini)
(cd inference_gateway && pytest)                # no formatter configured
```

Resolve every warning rather than committing past it — a warning that lands is
one the next person has to wade past to see whether *their* change added
anything. Prefer a real fix over a suppression; when you must suppress, make it
targeted and give a one-line reason.

Format only the code you're landing: if a formatter rewrites files unrelated to
your change, leave those out of this commit.

Skip this step only when nothing staged is code (pure docs/config commits). It is
**not** optional just because the warnings are pre-existing.

## Step 4 — update in the same commit

Edit the companions that apply. Do **not** split into a separate "docs/tests
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
[MEKO-<num>] <component>: <title>
```

- **`[MEKO-<num>]`** — the tracking ticket, first so `git log --oneline` is
  greppable by ticket. Use **`[MEKO-xxx]`** when the work genuinely has no
  ticket; the tag itself isn't optional.
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
[MEKO-xxx] ci: prefix CI job names with "Meko"
docs(mcp-server): datapack_update takes name/description, not connection_string
```

The subject describes the **code** change; companion updates (docs, tests,
generated files) ride along silently — don't enumerate them in the subject. Call
out a companion in the body only if it's large or load-bearing.

### No agent attribution — message *or* authorship

`AGENTS.md > Git Commit Guidelines` is explicit: **do not mention any AI
assistant in commit messages, PR descriptions, or code comments; no
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

## Step 5 — reference and close the tracked ticket

If this commit advances or completes a tracked ticket or GitHub issue:

- **Reference it** with the `[MEKO-<num>]` subject tag (see Subject format above)
  so the work is linked.
- **Close it when the work ships.** Put `Closes <full issue URL>` in the PR body
  (or the final commit message) so merge auto-closes a GitHub issue, or move the
  JIRA ticket once merged. Link with full URLs, never bare `#123` —
  `AGENTS.md > Pull requests`.

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
  prerequisite that `LOCAL_DEV_SETUP.md` / `DEVELOPMENT.md` reference.

## Anti-patterns

- **"I'll update the doc/test in a follow-up."** No. The follow-up never happens,
  and the companion lies for the gap. Same commit.
- **Hand-editing a generated file** (`zz_generated.*.go`, `config/crd/bases/*.yaml`,
  `api_server/docs/swagger.json`) instead of changing the source and re-running
  the generator. The next `make manifests generate` silently reverts you.
- **Editing a migration that already shipped.** Migrations run on every
  `api_server` boot; add a new numbered pair instead.
- **Cargo-culting.** Don't add a ceremonial test or a changelog paragraph for
  every commit. Many commits just edit an existing section / extend an existing
  test. Volume is not progress.
- **Committing with a red linter** because the warnings were already there.
- **Splitting a companion update into a separate PR** when the underlying code
  change is on this branch. Code and its companions travel together.

## Failure mode this prevents

This repo has a visible trail of it. `datapack_update`'s arguments changed to
`name`/`description`, but `docs/content/docs/reference/mcp-server.md` kept
telling users it took a `connection_string` — repaired later by a standalone
docs PR (`docs(mcp-server): datapack_update takes name/description, not
connection_string`, #366). The `scope` attribute was removed from the MCP server
tools; the docs kept documenting it until another cleanup PR removed the
references (#368). A CLI example went stale the same way (#346).

Each of those is a period during which the canonical, user-facing description of
Meko was a fabrication, plus a second PR's worth of work to undo. Every one would
have been a two-line edit riding along in the commit that changed the behavior.
That's what this skill is for.
