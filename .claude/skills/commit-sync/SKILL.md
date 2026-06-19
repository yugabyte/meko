---
name: commit-sync
description: >-
  Before staging files for a git commit (or finalizing one) in this repo,
  check whether the change needs companion updates that must travel in the
  **same** commit: (1) `architecture/*.md` docs, (2) tests covering the
  new/changed behavior, (3) inspection/diagnostic tooling under `tools/` and
  `scripts/`, and (4) operational `runbooks/*.md`. For each, decide whether the
  change makes it wrong or missing, and update it in the same commit. Also, on
  any commit that touches Rust, run `cargo clippy` and resolve every warning
  before committing (a Rust commit lands clippy-clean). Use for
  `git commit`, `git add`-then-commit, `/commit-commands:commit`,
  `/commit-commands:commit-push-pr`, or any flow that ends in a commit.
---

# commit-sync

A commit should land **complete**: the code, plus the companions that must stay
true alongside it — the docs people read, the tests that guard it, the tools
that inspect it, and the runbooks operators follow. A commit that changes how
the system is wired but leaves any of these stale ships a half-truth, and
**wrong docs / tools / runbooks are worse than absent ones**: they teach future
readers (and future agents) a false model, and they fail at the worst time
(during an incident, mid-debug).

So: every commit that touches a subsystem covered by one of these companions
must keep that companion accurate, **in the same commit**. Not "follow-up", not
"TODO" — same commit. If the change doesn't affect a given companion, that's
fine; you still have to look.

(Note: the GitHub-issue tracker is handled by the separate
`task-tracker-sync` skill. This skill's Step 5 still reminds you to
reference and close the relevant issue at commit time — they work together.)

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
```

If the only staged files are themselves under `architecture/`, `docs/`,
`runbooks/`, or top-level READMEs, you can skip the doc/runbook arms —
but still ask whether the change needed a test that isn't staged.

## Step 2 — walk the four companions

For each companion below: **does this change touch it? If so, update it in this
commit.** A single change can hit several — do all that apply.

---

### Companion 1 — Architecture docs (`architecture/*.md`)

`architecture/*.md` is the canonical description of how the system is wired
today. Map staged files to docs with this table; a staged path matching the left
column means you must read the doc on the right and decide if it needs updating.

| Code area (staged path matches) | Arch doc(s) to recheck |
|---|---|
| `safekeeper/**`, `libs/safekeeper_api/**`, `libs/walproposer/**` | `architecture/safekeeper.md`, `architecture/safekeeper-lsns.md` |
| `pageserver/**` (excluding `pageserver/pagebench/**`) | `architecture/pageserver.md`, `architecture/pageserver-lsns.md`, `architecture/yb-pageserver.md` |
| `storage_controller/**` | `architecture/storage-controller.md` |
| `endpoint_storage/**` | `architecture/remote-storage.md` (YbStore backend) |
| `libs/remote_storage/**` | `architecture/remote-storage.md` |
| `pgxn/**`, `compute_tools/**`, `compute/**` | `architecture/compute.md`, `architecture/compute_ctl.md` |
| Schema files: `storage_controller/migrations/**/*.sql`, anything creating/altering tables in `yb_storage` or `storage_controller` databases | `architecture/yugabyte.md` + the per-subsystem doc that owns the table |
| Anything that changes a binary's CLI surface (clap `Args` structs in `*/src/bin/*.rs`) | The "Run / configure" block in the matching subsystem doc |
| **HTTP router** (`pageserver/src/http/routes.rs`, `safekeeper/src/http/routes.rs`, `storage_controller/src/http.rs`, `compute_tools/src/http/**`, `endpoint_storage/src/app.rs`) — a route added/removed/renamed, a method/path/auth/body change | The matching `architecture/<service>-apis.md` (`yb-pageserver-apis.md` / `safekeeper-apis.md` / `storage-controller-apis.md` / `compute_ctl-apis.md` / `endpoint-storage-apis.md`) |
| **Metrics module** (`*/src/metrics.rs`, `compute_tools/src/compute_metrics.rs`) — a metric added/removed/renamed, or a label/type change | The matching `architecture/<service>-metrics.md` (`yb-pageserver-metrics.md` / `safekeeper-metrics.md` / `storage-controller-metrics.md` / `compute_ctl-metrics.md` / `endpoint-storage-metrics.md`) |
| Top-level topology (new binary, new wire link, removed component) | `architecture/high-level.md` + `architecture/README.md` |

The doc needs an update if the change invalidates **any** of these:

- A section heading or "Changes in yb-light" entry (e.g. you deleted the module
  §N describes, or removed a thing the doc presents as still-present).
- A code-layout list (`safekeeper/src/foo.rs` — purpose). Module
  deleted/added/renamed → list is wrong.
- A CLI / config example (Run / configure block, TOML snippet, flag list). A
  flag added/removed/renamed makes the example a lie.
- An HTTP route table (`<service>-apis.md`) or a metric table
  (`<service>-metrics.md`) entry — a route/metric you added/removed/renamed,
  or a changed method/path/auth/label/type.
- A schema block (`CREATE TABLE ...`, column list, primary key, **column
  type**) when you altered the live table.
- A mermaid diagram (an arrow, node, or label that no longer corresponds to the
  code).
- A persistence cheat sheet / field table / LSN inventory.
- A claim about a control-flow path ("X calls Y which writes to Z") if you
  changed any node in that path.
- An anchored commit hash, when your commit supersedes that commit's behavior
  (rewrite the section; don't just append).

If you genuinely can't find any statement the doc makes that's now wrong, fine —
but default toward "needs a touch": a section header rename, a one-line
clarification, an obsolete commit anchor swapped for the new one. These are
cheap and they're the things that rot.

---

### Companion 2 — Tests

If the change adds behavior, changes an interface, or fixes a bug that no
existing test would have caught, add or extend a test **in the same commit** —
one that would have failed before your change and passes after.

Where tests live in this repo:

| Kind | Location |
|---|---|
| Unit | `#[cfg(test)]` modules inside the crate (e.g. `libs/ysql_client/src/lib.rs`) |
| Integration (Rust) | `<crate>/tests/` (e.g. `compute_tools/tests/`, `safekeeper/tests/`) |
| End-to-end (cluster) | `test/e2e/storage/fast/` and `test/e2e/storage/slow/` (`*.sh` / `*.sql`, driven by `test/e2e/storage/run.sh`) |
| Python harness | `test_runner/` |

Triggers that usually mean "add/extend a test":
- A new public function, API route, or CLI subcommand.
- A new migration / schema change with a round-trip you can assert (insert →
  read back).
- A bug fix — encode the regression as a test.
- A behavior change in a path that already has tests (update them so they assert
  the new behavior, not the old).

Don't add ceremonial tests for pure refactors that existing coverage already
exercises, or for changes that only an end-to-end run can meaningfully cover —
in that case lean on `test/e2e/storage/` and **say so** (run it, cite the result; see
the `e2e-before-corruption-claim` skill for the corruption-class rule). If a
change genuinely can't be unit-tested, note why rather than forcing a hollow
test.

---

### Companion 3 — Tools

Inspection / diagnostic tooling has to keep working against what you changed:

- `tools/` — `yb-dump-pages`, `yb-dump-wals`, `cluster-ui` (they read the
  YB-backed page store / WAL / control-plane state directly).
- `scripts/` — operational helpers (e.g. `download_basebackup.py`,
  `force_layer_download.py`, `sk_cleanup_tenants`, `yb-light-ship.sh`).

Check a tool when your change touches **what it reads or renders**:
- A YB table's schema / columns / key encoding changed → any dump or UI that
  reads that table (e.g. a column type flip from `TEXT` to `JSONB`, a renamed
  column, a new table that deserves its own dump).
- On-disk layout, file names, or directory structure a tool walks changed.
- A binary's CLI surface a script shells out to changed (flag renamed/removed).
- A wire format / key scheme a tool decodes changed.

If a new table or state store is added that operators will need to inspect,
consider whether a tool (or a flag on an existing one) should cover it — at
minimum, don't leave an existing tool silently dumping the wrong thing.

---

### Companion 4 — Runbooks (`runbooks/*.md`)

`runbooks/` (`perf-investigation.md`, `startup-failures.md`,
`throughput-troubleshooting.md`) are the operational procedures: the
`## Conventions` blocks (hosts, ports, env vars, `psql` path), the
copy-pasteable inspect/diagnose commands, the "where does state live" pointers,
metric names, and step-by-step troubleshooting flows.

Update a runbook when your change makes one of those wrong:
- Where state lives or how to inspect it changed (e.g. "scan
  `.yb_light/endpoints/`" → "query `storage_controller.endpoints`"). A runbook
  step that tells an operator to look in the wrong place is actively harmful
  during an incident.
- A CLI command, flag, port, or env var the runbook hardcodes changed.
- A metric name / HTTP endpoint a troubleshooting step polls changed.
- A startup precondition / failure mode the runbook diagnoses changed (esp. for
  `startup-failures.md`).

## Step 3 — clippy must be clean

If the commit touches Rust (`*.rs`, `Cargo.toml`, build scripts), run
clippy and resolve every warning **before** you commit — a warning-clean
tree is part of "landing complete", same as the companions above. A
warning that lands is one the next person has to wade past to see whether
*their* change added anything.

```
cargo clippy --workspace --all-targets 2>&1 | grep -E "^warning: " | grep -v generated
```

Must print nothing. If it doesn't:

- `cargo clippy --fix --workspace --all-targets --allow-dirty --allow-staged`
  clears the machine-applicable lints; fix the rest by hand.
- Prefer a real fix. Reach for a **targeted** `#[allow(clippy::...)]`
  (with a one-line reason) only for design-choice lints where a refactor
  would be riskier than the lint — e.g. `too_many_arguments` on a wide
  wiring/constructor signature, `large_enum_variant` where boxing would
  ripple through the type. Never blanket-allow at crate level.
- Re-run until the grep above is empty, then `cargo build --workspace` to
  confirm the fixes compile.

Skip this step only when nothing staged is Rust (pure docs/runbook/config
commits). It is **not** optional just because the warnings are
pre-existing: a Rust commit lands clippy-clean.

## Step 4 — update in the same commit

Edit the companions that apply. Do **not** split into a separate "docs/tests
follow-up" commit — if the code lands and a companion still says the old thing,
even for one commit, search/agent context (and operators) pick up the wrong
model.

Re-stage and commit everything together:

```
git add <code files> <architecture/*.md, tests, tools/scripts, runbooks/*.md you edited>
git commit -m "..."
```

### Subject format

The commit subject follows:

```
[#<issue>] <area>: <sub-component>: <title>
```

- **`[#<issue>]`** — *optional* leading tag, present when the commit
  advances a tracked GitHub issue (e.g. `[#42]`). Put it first so
  `git log --oneline` is greppable by issue and GitHub auto-links the
  commit on the issue. Omit it for work with no issue.
- **`<area>`** — *required*. Either a **subsystem** — **`storage`** or
  **`control_plane`**, mirroring the issue area labels (`control_plane`
  underscored in the subject; the label itself is `control plane`) — or
  a **meta token** for work that touches neither subsystem: **`pm`**
  (project management: tracker, issues, planning), **`docs`**, **`ci`**,
  **`build`**, **`tools`**. Pick a subsystem when the commit changes
  product code; pick a meta token otherwise. The authoritative taxonomy
  lives in [`docs/issue-tracking.md`](../../../docs/issue-tracking.md).
- **`<sub-component>:`** — *optional* second segment naming the
  subsystem within the area: `pageserver`, `safekeeper`, `walredo`,
  `materialiser`, `walreceiver`, `page_service`, `e2e` (under
  `storage`); `storage_ctl`, `yb_light_local`, `cluster-ui` (under
  `control_plane`); … Drop it when the area alone is precise enough.
- **`<title>`** — *required*. Concise, imperative description of the
  change.

Examples:

```
[#34] storage: walreceiver: fix torn WAL write on kill -9
[#33] storage: pageserver: skip zero-page writes to YB
storage: materialiser: pace background image compaction
storage: e2e: add E2E_STOP_ON_FAIL to halt at the first failing iteration
control_plane: yb_light_local: retry HTTP bind on EADDRINUSE during recycle
pm: migrate task tracker to GitHub issues
docs: clarify the YB idempotency contract
ci: pin protoc to 3.21
```

For staged work on one issue, note the stage in the title:
`[#42] storage: pageserver: strip multi-timeline machinery (stage C)`.

The subject describes the **code** change; companion updates (docs,
tests, tools, runbooks) ride along silently — don't enumerate them in
the subject. Call out a companion in the body only if it's large or
load-bearing (e.g. a substantial doc rewrite or a new test suite).

## Step 5 — reference and close the tracked issue

If this commit advances or completes a GitHub issue (see the
`task-tracker-sync` skill for the full tracker contract):

- **Reference it** with the `[#<num>]` subject tag (see Subject format
  above) so the work is linked.
- **Close it when the work ships.** Either put `Closes #<num>` /
  `Fixes #<num>` in the PR body (or the final commit message) so merge
  auto-closes it, or close it explicitly once merged:

  ```
  gh issue close <num> --comment "Landed in <commit/PR>. <what shipped>."
  ```

Don't leave a shipped fix's issue open "to close later" — that gap is
exactly when the issue list misleads. If the commit only partially
advances the issue, leave it open and say what's left (a comment, or an
edit to the body), don't close prematurely.

## When you genuinely don't need to update

Skip a companion only when you can name **which** rule applies — not just "feels
minor." If you can't categorize the skip, default to checking.

- **Arch docs:** pure bug fix inside an internal function the doc describes only
  at the subsystem level ("X handles Y"; the bug was in how X computed Z — still
  true); comment/typo/whitespace; dependency bump that doesn't change documented
  behavior; build/CI plumbing that doesn't change deployed architecture.
- **Tests:** pure refactor with unchanged behavior already under test;
  comment/formatting/dep-bump; a change only an e2e run can cover (then run e2e
  instead and cite it).
- **Tools:** the change doesn't alter any schema, on-disk layout, key encoding,
  wire format, or CLI surface a tool reads.
- **Runbooks:** the change doesn't alter any command, port, env var, metric,
  state location, or failure mode a runbook references.

## Anti-patterns

- **"I'll update the doc/test/runbook in a follow-up."** No. The follow-up never
  happens, and the companion lies for the gap. Same commit.
- **Cargo-culting.** Don't add a fresh "Changes in yb-light" §N entry, a
  ceremonial test, or a runbook paragraph for every commit. Many commits just
  edit an existing section / extend an existing test. Volume is not progress.
- **Updating only the commit-hash anchor.** If you swap `c4c7c1350` for
  `840e56aae` but the prose still describes the old design, you've made the doc
  more wrong, not less.
- **Leaving a tool silently dumping the wrong thing** after a schema change
  because "it still runs." Running and being correct are different.
- **Splitting a companion update into a separate PR** when the underlying code
  change is on this branch. Code and its companions travel together.

## Failure mode this prevents

Real example from this repo: §1 of `architecture/safekeeper.md` described the
control file as a 388B bincode blob stored at `yb_storage.blobs` under
`safekeeper-control/<tenant>/<timeline>`, keyed by a
`ControlFileStorage::{File, Yb}` dispatch enum, selected by
`--control-file-backend file|remote-storage`. By the time anyone read it, none
of that was true: #10 (`840e56aae`) had moved the control file to typed
columns in `safekeeper.timeline_state`, deleted the enum, deleted
`control_file_upgrade.rs`, and removed both CLI flags. The commit that did the
move did not touch the architecture doc — so for weeks the canonical description
of the safekeeper was a fabrication. The same rot hits a `yb-dump-*` tool that
keeps decoding a dropped column, a runbook that tells you to grep a directory
that no longer holds the state, and an untested code path that quietly
regresses. This skill exists so that doesn't happen.
