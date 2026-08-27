---
name: amp-report-service
description: >-
  Use when working with the amp report portal (the QA results service for amp):
  triaging failed runs and tests, reading result logs/attachments, checking perf
  history and regressions, managing known issues, launching test runs, cutting
  release builds, or deploying/updating amp portals through the Fleet page. Covers
  the three interfaces — the `yb` CLI, the portal's MCP server, and the web UI —
  how to set each up, and the discovery-first workflow the fast-moving tooling
  requires.
---

# amp report service — how developers use it

The amp report portal at **http://10.150.1.93** is where every ampqa run lands:
functional/stress/perf results, their logs and attachments, perf history with
regression gates, known-issue tracking against GitHub, and the buttons that
drive Jenkins — launch a test run, cut a release build, deploy or update an amp
portal. If you want to know "did my change break anything", "why is this test
red", or "put my branch build on a cluster", this is the service you talk to.

## Rule zero — discover, don't recall

**The tooling changes weekly. Never act from a memorized flag, tool name, or
endpoint — including anything written in this skill.** Concepts here are
stable; invocations are not. Before doing anything:

```bash
yb amp --help              # the command map
yb amp <cmd> --help        # ALWAYS, before first use in a session
yb manifest                # full machine-readable command tree (for agents)
```

For the MCP server, list the tools at runtime (`tools/list`) instead of trusting
any tool table you have seen before — including the one in this skill.

## The three interfaces

### 1. `yb` CLI — the default for humans and agents

One static binary wrapping the portal's whole API under `yb amp …`.

**Install** (no Go toolchain needed; drops the binary at `~/.local/bin/yb`):

```bash
curl -fsSL https://raw.githubusercontent.com/yugabyte/qa-services/main/yb-cli/install.sh | bash
```

From a `qa-services` checkout: `cd yb-cli && ./install.sh` (or
`YB_FROM_SOURCE=1 ./install.sh` to build). Update = re-run the installer.

Command families (`yb amp --help` for the live list): `run`, `result`, `files`,
`perf`, `issue`, `report`, `fleet`, `build`, `schedule`, `catalog`, `status`,
`stats`.

Conventions that hold across all of them:

- Commands accept **bare UUIDs or full portal URLs** interchangeably — paste a
  `http://10.150.1.93/runs/<id>` link directly.
- `--json` on anything returns a stable envelope (`kind`, `data`, `truncated`)
  for scripting.
- **Write operations refuse to run** without `--yes` (interactive) or `--json`
  (programmatic). If a command warns about a write gate, that is by design.
- Long output is truncated per line with an explicit marker; `--max-line -1`
  disables it.

### 2. The portal MCP server — for AI agents

A stateless proxy over the same API, exposing ~45 tools (list/get runs,
results, perf compare, known issues, file search, trigger run/build/deploy, …).

**Setup** — point your MCP client at the running portal:

| Transport | URL |
|---|---|
| streamable HTTP (Cursor, Windsurf, …) | `http://10.150.1.93:8989/mcp` |
| SSE (Claude Code) | `http://10.150.1.93:8989/sse` |

```bash
# Claude Code
claude mcp add amp-report --transport sse http://10.150.1.93:8989/sse
# health check
curl http://10.150.1.93:8989/health
```

No auth, no local install. For a locally-run portal the same paths hang off
`localhost:8989`. First action in any session: `tools/list` — the tool set
tracks the portal release, not this document.

### 3. The web UI

http://10.150.1.93 — runs list, per-result pages (steps, error, attachments,
perf tables and charts), perf trends, Fleet (standing portals + deploy/update
dialogs), known issues, schedules, and a per-release product status board. The
UI and the tools read the same API: anything you see on a page, the CLI/MCP can
fetch — a result page maps to `yb amp result get <url>`.

## The data model in one screen

```
run  (one ampqa invocation: branch, commit, image tag, report bucket)
└── result   (one test: status, error, steps tree, team, suite)
    ├── steps         (nested, timed, [OK]/[FAILED])
    ├── attachments   (logs, phases.txt, report.html — stored in the files service)
    ├── perf data     (named datapoints; gates against baselines or intra-run ratios)
    └── known issues  (links to GitHub issues that EXPLAIN the failure)
```

- **Reports** group runs into buckets (`regression`, `nightly`, …) — attach or
  detach a run by patching its `report` key.
- **Known issues** are GitHub issues (this repo) registered in the portal and
  attached to failed results; a triaged failure is one with a KI chip. Keep the
  GitHub side authoritative — the portal syncs title/state from it.
- **Fleet** rows are standing amp deployments; deploy/update/delete actions
  trigger the Jenkins jobs in `meko-devops` (`jenkins/amp/…`), which report
  back as runs of kind `deploy`.
- Runs, builds and deploys triggered from the portal all carry
  `triggered_by`/`build_tag`, so a result is always traceable to the exact
  image and commit — `Config` on the run shows both.

## Canonical workflows

Sketches, not scripts — verify each command's current flags first (`--help`).

**Triage a failed run** (a portal link in hand):

```bash
yb amp run get <url>                  # statuses + failed test list
yb amp result get <result-id>         # error, steps, attachments, KI chips
yb amp files read <source>            # read an attachment (log, phases.txt)
yb amp files search-in <source> "..." # grep inside one attachment
yb amp result history <result-id>     # flaky, or new with this build?
```

Then decide whether the failure is the PRODUCT's or OURS, because that decides
what happens next and they are not symmetrical.

**Product defect** — amp or the backing behaved wrongly. Attach the existing
known issue, or file a GitHub issue (follow the `create-amp-issue` skill: area
label + AMP milestone) and register it:

```bash
yb amp issue create --url <github-url> --result <id> --notes "..." --yes
```

**Test or framework defect** — the test asserted the wrong thing, the harness
lost a port-forward, a chaos action injected nothing, a client pod died, a
budget was too short. **Do not file a GitHub issue. Fix the code.** GitHub
issues are the product's defect log, and filling it with our own harness bugs
buries the real ones and wastes the reader's time. Fix it on a branch, and if
the fix has to wait, say so in the test's own doc comment where the next person
to run it will see it.

The tell: if the answer to "would a customer ever hit this?" is no, it is ours.
A `needs-area-label` issue with no product area to assign is usually the same
signal — nothing in the product owns it because the bug is not in the product.

Either way the portal still needs the failure explained, so a framework defect
you are fixing gets a note on the result rather than a fabricated issue link.

**Perf question** ("did X regress?"):

```bash
yb amp perf history <test> <datapoint>    # value across runs, newest first
yb amp perf compare <result-id>           # this result vs trailing baseline
```

Cross-build comparisons live on the result page's Performance section (history
averages per version) and the perf-trends page.

**Launch / rebuild / deploy:**

```bash
yb amp run start --help      # test runs (tag or single test, branch builds)
yb amp build --help          # cut a release (versioning is tag-driven)
yb amp fleet --help          # standing portals: deploy / update / delete
```

All three are thin triggers over Jenkins jobs — the portal records the build
and links it; deep failure diagnosis continues in the Jenkins console.

**Triggering a run — two rules before any flags:**

1. **The YugabyteDB version comes from the portal settings.** `yb amp settings`
   → `default_yb_version` is the version the rest of the stack is known to
   carry; pass it as `--yb-version` on every trigger. Never hand-pick one and
   never omit the flag — an omitted version falls through to the Jenkins job's
   own baked default, which can be a whole train behind.
2. **A branch that only changes test code does not rebuild amp.** `--branch`
   names the TEST FRAMEWORK checkout; the amp images are a separate axis.
   Pass `--image-version` with the newest release (first non-custom row of
   `yb amp builds`) and the run deploys those prebuilt images while the tests
   come from the branch head. `--commit` is the build-from-source switch: it
   implies BUILD_FROM_SOURCE, ignores `--image-version`, and costs ~20 min of
   image build per new sha — reach for it only when the branch changes amp/
   product code.

```bash
# test-only branch: framework from the branch head, images from a release
yb amp run trigger --tag perf --branch <user>/<topic>     --image-version <release-tag> --yb-version <default_yb_version> ...

# product branch: build and test the images from this exact sha
yb amp run trigger --tag regression --branch <user>/<topic> --commit <sha>     --yb-version <default_yb_version> ...
```

After triggering, `yb amp run get <id>` shows the Jenkins params: a release run
carries `IMAGE_VERSION` and no `BUILD_FROM_SOURCE`, and `YB_VERSION` must be
present either way. Repeat triggers at the SAME sha reuse the already-built
`custom-<sha>` images, so amending commits is what makes source builds
expensive.

## Anti-patterns

- **Guessing flags from memory or from this file** instead of `--help` /
  `tools/list`. The single most common failure mode; the tools genuinely
  change under you.
- **Hand-rolling `curl` against the portal or Jenkins** for something the CLI
  or MCP already does — you lose the write gates, URL parsing, and JSON
  envelopes, and your incantation breaks on the next release.
- **Marking a failure triaged without a known issue.** The KI link is what
  makes tomorrow's identical failure arrive pre-explained.
- **Filing a GitHub issue for our own test or framework bug.** The issue
  tracker is the product's defect log. A harness bug goes in a branch, not in
  it.
- **Editing portal state to "fix" a result.** Results are records of what ran;
  if the outcome is wrong, fix the test or the product and re-run.
- **Triggering without `--yb-version`, or with a hand-picked one.** The portal
  default (`yb amp settings`) is the only version the deployed stack is known
  to carry.
- **`--commit` on a test-only branch.** It flips the run to build-from-source
  and rebuilds identical amp images for every sha; use `--branch` +
  `--image-version` (see Triggering above).
