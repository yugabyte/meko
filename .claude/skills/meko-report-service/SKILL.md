---
name: meko-report-service
description: >-
  Use when working with the meko report portal (the QA results service for the
  mekoqa framework): triaging failed runs and tests, reading result logs and
  attachments, checking perf history, managing known issues, launching or
  rerunning mekoqa test runs, reading or changing the Config page defaults, or
  looking up catalog branches, tests and image tags. Covers the three interfaces
  (the `yb meko` CLI, the portal's MCP server, the web UI) and the
  discovery-first workflow the fast-moving tooling requires.
---

# meko report service, how developers use it

The meko report portal is at **https://meko-qa.dev.yugabyte.com** (API under
`/api/v1`). The bare `http://10.150.5.246` still answers, so older links keep
working. It is a copy of the amp report portal, so the routes and JSON shapes
match amp's. Every mekoqa run lands here: results, steps, logs and attachments,
perf history, known issues, schedules, and the buttons that launch the
`meko-run-tests` Jenkins job. For "did my change break anything" or "why is this
test red", this is the service to ask.

For amp's portal use the `amp-report-service` skill instead.

## Rule zero, discover, don't recall

The tooling changes often. Do not act from a memorized flag or tool name,
including anything in this file.

```bash
yb meko --help             # the command map
yb meko <cmd> --help       # always, before first use in a session
yb manifest                # machine-readable command tree
```

For the MCP server, run `tools/list` and trust that over any table you have seen.

## The three interfaces

### 1. `yb meko` CLI

The same binary as `yb amp`. Install or update:

```bash
curl -fsSL https://raw.githubusercontent.com/yugabyte/qa-services/main/yb-cli/install.sh | bash
```

The base URL defaults to `https://meko-qa.dev.yugabyte.com`. Override it with `--meko-url`,
`YB_MEKO_URL`, or `YB_MEKO_API_URL`.

Command families: `run` (list, get, status, wait, trigger, restart, rerun, stop,
set-report), `result`, `files`, `perf`, `issue`, `report`, `status`, `stats`,
`catalog`, `releases`, `schedule`, `settings`.

```bash
yb meko run list --status failed
yb meko run get <uuid-or-url>
yb meko result get <result-id>
yb meko files search-in <source> "panic"
yb meko catalog tests --key main
yb meko releases --limit 5
yb meko settings
```

Conventions:

- Ids can be a bare UUID, a portal URL, or the 8-character id the list tables print.
- `--json` returns an envelope (`kind` is `meko.*`, `data`, `truncated`).
- Anything that launches or writes needs `--yes` (or `--json`). Launch commands
  take `--dry-run`, which prints the request and sends nothing.
- amp's fleet, lru and build groups are not exposed. meko has no job behind them.

### 2. The MCP server

A stateless proxy over the same API. Streamable HTTP at
**https://meko-qa.dev.yugabyte.com/mcp**, SSE at `/sse`. No auth. Add it to Claude Code:

```bash
claude mcp add --transport http --scope user meko-qa https://meko-qa.dev.yugabyte.com/mcp
```

Cursor: `{ "mcpServers": { "meko-qa": { "url": "https://meko-qa.dev.yugabyte.com/mcp" } } }`.

`curl https://meko-qa.dev.yugabyte.com/mcp` hangs by design. A GET opens the event stream, and
a POST needs `Accept: application/json, text/event-stream` and a JSON-RPC body.
Check health with `curl https://meko-qa.dev.yugabyte.com/api/v1/health` or `http://10.150.5.246:8989/health`.

Tool families: runs and results, metrics, perf, files (`read_file_lines`,
`search_in_file`), known issues, schedules, `list_catalog`, `list_releases`,
`get_settings` and `update_settings`, `trigger_run`, `restart_run`,
`rerun_results`, `trigger_build`. Write tools run only on an explicit request.

### 3. The web UI

https://meko-qa.dev.yugabyte.com has the runs list, result pages, perf trends, known issues,
schedules, the per-release status board, and the Config page. The sidebar's
"Connect AI agents" dialog shows the same MCP and CLI setup.

## Triggering a run

Fields of `POST /trigger/run`, as `yb meko run trigger` flags and `trigger_run`
arguments:

| Field | Meaning |
|---|---|
| `tags`, `test`, `testr` | selector. One is required. `testr` is a regex over test names |
| `branch` | meko branch. Omit it and the portal uses its default branch |
| `commit` | pin a sha, implies build from source |
| `build_from_source` | build the images from the branch |
| `image_version`, `versions` | one quay tag for all images, and per-image overrides (`api_server`, `mcp_server`, `ui`, `inference_gateway`, `rag_agent`, `langfuse`) |
| `parallel` | concurrent test bodies |
| `report`, `owner` | report bucket, who launched it |
| `k8s_cluster` | cluster, default from Config |
| `yb_backing` | `bundled`, `yba_universe`, or a framework value |
| `database_image` | `meko-yugabytedb` tag or a full reference |
| `keep_env`, `keep_env_failed` | keep the environment after the run |
| `options` | `key=value` mekoqa `-o` overrides |

`yb_version` is stored on the run and not forwarded, because the job has no such
parameter. Pick the database with `database_image`.

Leave optional fields unset unless the request names them. The portal fills in
branch, cluster and parallelism from the Config page.

mekoqa has three suites, and each has one tag that selects all of it and files the run
under its Status card: `functional` is `--tag regression`, `stress` is `--tag stress`,
`perf` is `--tag perf`.

```bash
yb meko run trigger --tag regression --dry-run
yb meko run trigger --tag regression --yes --wait
yb meko run rerun <run-id> --result <result-id> --yes
```

A rerun is its own Jenkins build, but its results land on the original run as
further attempts. Watch the original run.

## Config page defaults

`yb meko settings` prints them, `yb meko settings set ... --yes` changes them.

| Setting | Current value |
|---|---|
| `default_branch` | `main` |
| `default_k8s_cluster` | `amp-dev` |
| `default_parallel` | unset, the portal falls back to 4 |
| `default_database_image` | unset, the framework's image |
| `default_options` | `-o` overrides merged under every launch's own options |

Check the live values before relying on this table. Changing them affects every
later launch, so do it only on request.

## Jenkins jobs

All under `users/shubin/meko-devops/MEKO/`:

- `meko-run-tests` runs a mekoqa run. This is what Launch and `trigger_run` start.
- `meko-catalog` collects branches, tests per branch and image tags into the portal catalog.
- `meko-reap` deletes leftover test environments.

## Data model

```
run  (one mekoqa invocation: branch, commit, image tag, report bucket)
└── result   (one test: status, error, steps tree)
    ├── steps         (nested, timed)
    ├── attachments   (logs, report.html, stored in the files service)
    ├── perf data     (named datapoints compared to a trailing baseline)
    └── known issues  (GitHub issues that explain the failure)
```

## Workflows

Triage a failed run:

```bash
yb meko run get <url>
yb meko result get <result-id>
yb meko files read <source>
yb meko result history <result-id>
```

Decide whether the product or the test is at fault. A product defect gets a known
issue (`yb meko issue create --url <github-url> --result <id> --yes`). A test or
framework defect gets a fix in the test code, not a GitHub issue.

Pick an image to run against: `yb meko releases` lists complete release tags, and
`yb meko catalog image_tags --key meko-yugabytedb` lists database image tags.

## Anti-patterns

- Guessing flags or tool names instead of `--help` and `tools/list`.
- Hand-rolled `curl` against the portal for something the CLI or MCP does. You lose
  the write gates and the id parsing.
- Setting `--branch`, `--cluster` or `--parallel` "to be explicit". Unset means the
  Config page default, which is usually what you want.
- Changing settings or triggering runs as a test of the tooling. Use `--dry-run`.
- Editing portal state to fix a result. Fix the test or the product and rerun.
