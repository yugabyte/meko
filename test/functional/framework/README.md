# ampqa — yb-amp QA framework

`ampqa` is the system-test framework for **yb-amp / yb-amp** — a Neon-style
fork that separates compute from storage: a controller manages
projects / branches / endpoints (stateless Postgres compute) over a real
YugabyteDB cluster that holds the durable state. `ampqa` stands those systems
up end to end — on **k8s (GKE primarily)**, plus GCE and AWS VM backings — runs
workloads and chaos through them, and reports honestly what happened.

It is a single Go binary and its own Go module: a small product-agnostic engine
(`core` / `sched` / `exec` / `report`) + environment providers (`env/`) + apps
that talk to the product over the wire (`apps/`) + a chaos engine (`chaos/`) +
a CLI (`cmd/ampqa`). The authoritative design lives in
`../../QA_FRAMEWORK_ARCHITECTURE.md` and `../../QA_FRAMEWORK_IMPLEMENTATION.md`;
`ampqa manifest` and `ampqa collect` are ground truth for what exists today, and
this README is kept in sync with them.

## Core principles

These are load-bearing — the reasons the framework stays versatile, safe, and
maintainable. Every change (human or agent) must uphold them; the authoritative
statement with rationale is `AGENTS.md` (read it before changing framework
code). In brief:

1. **KISS — small agnostic engine, atomic apps.** The engine knows nothing about
   what is tested. Every interaction with the product or an external system is an
   atomic, reusable helper under `apps/`. A new capability is a new
   app / action / checker / sink / task / env provider — **never an edit to the
   engine**. Add an interface only where two real implementations exist.
2. **Standalone module — no product coupling.** `test/functional` imports no
   product code and none imports it; `go build ./...` works with the rest of the
   repo absent. The product is reached only over the wire (REST / psql / SSH /
   kubectl / docker); the controller client is generated from the OpenAPI spec.
3. **Parallel by design.** Tests are atomic and independent — own env, no shared
   mutable state, no ordering. `--parallel N` is first-class; **sequential is
   just pool size 1**, never a separate path. No fixed ports or shared temp
   paths; everything passes `go test -race`.
4. **Nothing runs on the operator's machine.** The framework only coordinates —
   CLI/API calls to remote pods, instances, clusters. Logs and metrics are
   **streamed**, never buffered unbounded, so a CI agent never OOMs.
5. **Resources outlive `Provision`.** The provision `ctx` bounds provisioning
   work only; anything that must outlive it (port-forwards, SSH sessions,
   background processes) is rooted in `context.Background()` and released in
   `Teardown`/`Close`. **Teardown always runs** once provisioning succeeded, on
   its own deadline, even on failure/panic — and **logs are collected before
   anything is killed** (`apps.StopWithCollect`).
6. **One config mechanism, one home for defaults.** Precedence: embedded
   `configs/default.yaml` → `-c` files (deep-merged) → `-jc` JSON → typed `-o`
   overrides. **Every default lives in `configs/default.yaml`**, documented; a
   defaults test guards it. The fully resolved config is recorded into every
   result.
7. **Secrets — named, masked, never leaked.** Credentials are declared by name
   in the `secrets:` section (token / file / env variants), resolved by name,
   **masked in logs and never recorded into results**.
8. **Infrastructure hygiene — reuse, don't pollute.** Create no orphan/redundant
   resources, reuse shared ones, clean up what you create. Only ever touch infra
   the framework created (labeled `ampqa*` / tagged `ampqa-run`). Tag and reap.
9. **Result honesty & reproducibility.** Status is set by *where* the failure
   happened (see taxonomy below); report faithfully. Every run has a **seed**
   driving all randomness; `--seed` replays it exactly.
10. **One framework, every product version.** No per-version branches. A test
    declares `VersionSpec` bounds and is auto-skipped out of range; cross-version
    differences are handled inside apps (check the live version), never with
    hand-rolled `if version < X` in test bodies.
11. **Dependencies — minimal and proven.** Prefer stdlib; add only mainstream,
    proven libraries. `kubectl` / `helm` / `docker` are shelled ambient-authed
    binaries, not linked SDKs.
12. **Every change is verified.** Before done: `go build ./...`, `go vet ./...`,
    `gofmt -l .` (empty), `go test -race ./...` (green). Unit tests use
    in-process doubles only — nothing real runs. The **canary suite** is the
    engine's self-test and must stay green. **No AI attribution anywhere.**

## Quickstart

```
make build
go run ./cmd/ampqa run --suite canary --fake-env
```

The canary suite is the framework's own self-test against the in-process fake
provider (no real infra). Its members (`pass` / `fail` / `panic` / `hang` /
`provision_env_fail` / `skip_version`) fail in deliberately different ways so
every status shows up in the summary table.

List the live test registry (always check, never assume): `go run ./cmd/ampqa
collect`. Run a real test — it needs an environment, from `-c` config,
`--env-provider`, or **nothing at all if you have a kube-context**:

```
# self-building k8s (default): stands up its own amp control plane, runs, tears down
go run ./cmd/ampqa run --tag regression

# explicit env from a config file
go run ./cmd/ampqa run --test features/pg_roundtrip -c my-env.yaml
```

With only a `kubectl` current-context in scope and no `-c`, `--env-provider
auto` self-builds a run-scoped amp control plane on k8s (`sharedk8s` deploy
mode), runs the selected tests project-per-test against it, and tears the whole
thing down when the run finishes. Only when **neither** a config-driven provider
**nor** a kube-context is available does a real-env test **skip cleanly** (exit
4, `no environment provider configured`) rather than fail.

Results land under `--out-dir <dir>/<runid>/`: `run.json`, one
`<test-name>.json` per result, `manifest.jsonl` (event log), and
`artifacts/<test-name>/<blob>` for collected logs/cores/diagnostics. Add
`--junit path.xml` and/or `--html path.html` for reports written at run end.
For agents, `go run ./cmd/ampqa manifest` emits the full command tree +
registered tests + config keys as JSON.

## Mental model

**Tests & suites.** A test is a Go function registered via `core.Register` with
a suite name, category, sharing mode, env shape, tags, and a one-line
description. Suites group them; `ampqa collect` / `ampqa describe` list the live
registry.

**Status taxonomy** (`core/result.go`) — the outcome is set by *where* a failure
occurred, and the distinction drives what to do next:

| Status | Meaning | Product signal? |
|---|---|---|
| `passed` | assertions + invariants green | — |
| `failed` | a product defect signal during the run | **yes** |
| `timed_out` | a watchdog deadline was exceeded | **yes** |
| `broken` | a framework bug (panic, impossible state) | no (fix the harness) |
| `env_failed` | the environment could not be provisioned/attached | no |
| `skipped` | not run (filter, version bound, missing provider/dependency) | no |

`env_failed` / `skipped` mean the test never got a fair run — never read them as
a pass.

**Seed determinism.** Every run has a seed (`--seed`, random if unset). Workload
data shape, chaos action selection/timing, and every other randomized choice
derive from it, so a failure reproduces by re-running with the same `--seed`.

## Environments, backings & lifecycle

**Provider** (`env/`) = *how* an environment is stood up. **Backing** = *what
YugabyteDB deployment* sits underneath. They are orthogonal: the same test runs
against a different provider/backing without changing test code.

| Provider | What it does |
|---|---|
| `fake` | in-process, no real infra — canary tests, dev loops |
| `docker` | containers on a remote docker host (over SSH) |
| `sharedk8s` | one amp control plane, a project + endpoint carved **per test** out of it (co-tenant `shared` tests packed, bounded by `--per-env-parallelism`). Two modes — see lifecycle below |
| `exclusivek8s` | a dedicated namespace + stack per `exclusive` test |
| `universe` | attaches to / provisions a YugabyteDB **universe** directly (not amp) as the target |

Backings: YB-in-k8s (`apps/ybk8s`), `yugabyted` (bare node), raw YB (VM), or full
YBA (`apps/yba` / `apps/ybastack`) — on k8s, GCE, or AWS
(`apps/cloud/{aws,gcp}`), with a co-location model (amp control plane + backing
provisioned together) to keep runs cheap.

**Provider selection.** `--env-provider auto` (the default) picks the first
*configured* provider in precedence `universe > sharedk8s > exclusivek8s >
docker`, and falls back to **self-building `sharedk8s`** when none are configured
but a kube-context is discoverable. "Everything other than k8s is a parameter":
`--env-provider docker|universe|exclusivek8s|fake` targets the same tests
elsewhere.

**Lifecycle — `--env-mode build|attach|keep`.** Every `run` provisions an
environment, runs the selected tests, and (by default) tears it down. The mode
changes whether teardown happens:

- **`build`** (default) — provision, run, tear down. A bare `run` leaves nothing
  standing.
- **`keep`** — provision, run, then **record** the environment in the standing
  registry (`~/.ampqa/envs.json`) instead of tearing it down, and print an
  `--env <id>` to reuse it. Supported by any provider that exposes a recordable
  standing environment (`sharedk8s` deploy mode today); others warn and tear
  down.
- **`attach`** — selected automatically by `--env <id>`: the registry entry is
  resolved to its provider + connection info, the tests run against that
  **standing** environment, and it is left running (tearing an attached env down
  is `env rm`'s job, not a `run`'s).

**`--setup-only`** — provision the environment a test needs, run its setup
fixtures, then **stop**: the body never runs and the env is left standing (forces
`keep`). This is "stand up an env from a test for manual/dev use." `ampqa setup`
is a thin **deprecated** alias for `run --setup-only --test <name>` (same engine,
prints a deprecation note).

**Self-building k8s (the default path).** When `auto` falls through to
`sharedk8s` with only a kube-context (no `controller_url`, no attach namespace),
the provider **deploys its own run-scoped amp control plane** — the same
`ampstack.App` the `exclusivek8s` provider and the `deploy-amp` task build — into
a freshly-named owned namespace (`ampqa-cp-<random>`), carves a project +
endpoint per test out of it, and tears the whole control plane down in `Close()`
at run end. So `ampqa run --tag regression` with nothing but a kube-context
stands up its own amp stack, runs project-per-test, and cleans up — no `setup`
step, no standing control plane needed. `--env-mode keep` / `--setup-only`
records that self-built control plane (`Provider.StandingEnv()`) so a later `run
--env <id>` attaches to it instead of rebuilding.

**Execution & packing.** `--parallel N` bounds concurrent test bodies (`N=1` is
sequential — same code path). `--per-env-parallelism` (with `sched.capacity`,
the total lease budget = max concurrent environments) bounds how many co-tenant
`shared` tests pack onto one environment at a time. A test's own
`EnvSpec.Sharing` (`shared` / `exclusive` / `persistent`) decides whether it may
share at all — `exclusive` and `persistent` always get a dedicated env.

## Suites

Per the live `ampqa collect` output (34 tests: `canary` 6, `features` 19,
`perf` 2, `presets` 5, `stress` 2).

**Four tier tags** — `nightly` is a **superset** of the other three:
`regression` (the fast PR gate: basic functions + the main use case + "the PR
didn't break anything important"), `stress` (the two `suites/stress` scenarios —
sustained load and combined load under chaos), `perf` (the baseline-gated
benchmarks in `suites/perf`), and `nightly` (re-runs regression against real
backings *plus* the extended Jenkins sweep across real k8s/GCP/YBA/yugabyted/raw-YB
backings, chaos, and perf). Every tagged test also carries `nightly`; the
extended-only tests carry just `nightly`. `canary` and `presets` are untagged
(selected by `--suite` / name). See `docs/testing-tiers.md`.

| Suite | Purpose |
|---|---|
| `canary` | Framework self-test (`pass`/`fail`/`panic`/`hang`/`provision_env_fail`/`skip_version`) — no real infra. Sanity-checks the engine itself. |
| `features` | End-to-end amp-on-backing: provision a backing, run a YB-native workload, deploy amp, run a workload through amp compute, cross-check, tear down. Holds the **`regression`** tier (11 fast, env-portable core-guarantee tests: roundtrip, restart durability, workloadgen, parallel isolation, mutations, transaction, index, cold cache, DDL-survives-restart, the multi-db main case) plus **`nightly`** tests: the k8s foundation test (`amp_on_yb_k8s`) and the amp-feature tests (`amp_branch_isolation`, `amp_endpoint_lifecycle`, `amp_compute_restart`). The cross-cloud/VM/YBA foundation tests (`amp_on_yugabyted_aws`, `amp_on_raw_yugabyte_aws`, `amp_on_yba_universe`, `amp_on_yba_provisioned`) are **untagged** — run on demand by name when that cloud infra is available. |
| `presets` | Setup-only menus for standing dev environments (`raw_aws`, `yb_k8s`, `yba_k8s`, `yugabyted_gce`, `pgbench_client`), sharing mode `persistent`, meant for `ampqa run --setup-only`. |
| `stress` | `combined_load` (parallel workloads + chaos against an exclusive universe) and `load_sustain` (workloadgen-driven soak). |
| `perf` | `tpcc` (BenchBase TPC-C) and `pgbench` (YB's `ysql_bench` fork) — both gate tps + latency against a stored baseline (`suites/perf/baseline/`). |

## CLI surface

Verified against `--help` on each command.

```
ampqa run        --suite <re> --test <exact-name> --testr <re> --tag <t>
                 [--parallel N] [--per-env-parallelism N] [--seed S]
                 [--env-provider fake|docker|sharedk8s|exclusivek8s|universe|auto]
                 [--env-mode build|attach|keep] [--env <id>] [--setup-only]
                 [--env-name label] [--ttl duration]
                 [-c file.yaml] [--jc file.json] [-o key=value]
                 [--out-dir dir] [--junit path.xml] [--html path.html]
                 [--fake-env] [--no-default-config]
ampqa setup      -t <exact-name> [--name label] [--ttl duration] [config flags]
                 # DEPRECATED alias of `run --setup-only --test <name>`
ampqa collect    [--suite <re>] [--test <exact-name>] [--testr <re>] [--tag <t>]
ampqa describe   [test]         # tests grouped by suite, or one test's full parameter block
ampqa manifest                  # full command tree + tests + config keys as JSON
ampqa task       [name]         # run one Task, or list built-ins with no argument
ampqa env        ls | rm <id> | reap --tag <t>|--stale <dur> | refresh-secret <id>
ampqa report     --from <run-out-dir|results.json> -o report.html
ampqa version
```

- **`run`** — `--test` is an exact name (`perf/pgbench`), `--testr` a regex
  (mutually exclusive). `--fake-env` uses the in-process provider
  (self-provisioning tests still stand up real infra in their body).
  `--parallel` / `--per-env-parallelism` / `--out-dir` / `--junit` / `--html`
  default from config (`sched.*` / `report.*`) unless passed. Exit codes: `0`
  all selected passed (or passed+skipped); `1` a `failed`/`broken`/`env_failed`/
  `timed_out`; `2` usage error; `3` selection matched no tests; `4` tests were
  selected but **none executed** (all skipped) — distinct from all-pass.
- **`setup`** — DEPRECATED; delegates to `run --setup-only --test <name>`.
  Select one setup-capable test (`presets` has ready-made menus, e.g. `ampqa
  setup -t presets/yb_k8s`). `--ttl` auto-reaps after a duration (default:
  never).
- **`collect`** — lists registered tests by suite/test/tag filter.
- **`describe`** — no argument: every test grouped by suite with description +
  attributes (category, sharing, shape, tags, phases). With a test name: that
  test's full block — description, the `run` flags, and its config-key prefixes
  resolved against `configs/default.yaml` (default + doc comment per key).
- **`manifest`** — command tree + tests + config keys as JSON; the agent
  self-discovery entry point.
- **`task`** — runs one named built-in Task, or lists them.
- **`env ls`** — standing environments from the registry.
- **`env rm <id>`** — reconstructs the env's real teardown from its registry
  record and runs it, then removes the record. `--skip-teardown` drops only the
  record; `--force` removes the record even if teardown errors.
- **`env reap`** — recovers leaked cloud infra by ownership tag: `--tag
  <runTag>` terminates everything tagged `ampqa-run=<runTag>`; `--stale <dur>`
  terminates managed autoclean instances older than `<dur>` with no known run
  tag. One is required; `--cloud aws|gcp` selects the driver.
- **`env refresh-secret <id>`** — re-mints the ECR image-pull secret for a
  standing k8s (ampstack) env and rolls the controller to re-pull. ECR tokens
  expire (~12h), so a long-lived standing env falls into `ImagePullBackOff` once
  a pod reschedules; this heals it without recreating the env. Needs `-c` with
  the creating config; `--no-roll` only refreshes the secret.
- **`report --from <dir-or-file>`** — renders a self-contained HTML report from a
  completed run's output, offline. Accepts a jsonfile run-out-dir (`run.json` +
  per-test JSON) or a plain JSON array of results.

Every command accepts `--json` for the stable envelope `{kind, version,
command, data, truncated}`; prefer it over parsing human output.

## Reporting, diagnostics & secrets

**Result / Sink model.** A `core.Result` accumulates status, steps, metrics, and
attachments as a test runs; each registered `report.Sink` receives lifecycle
events live. Sinks (`report/`):

- **`stdout`** — TTY-aware live progress (respects `NO_COLOR`): start/finish
  lines, indented steps, a final summary table.
- **`jsonfile`** — `<out-dir>/<runid>/`: `manifest.jsonl` (event log),
  `run.json` (run summary), one `<test>.json` per result, and attachment blobs
  under `artifacts/<test>/`.
- **`junit`** — JUnit XML for CI.
- **`html`** — self-contained report, live (`run --html`) or after the fact
  (`report --from`). Each test card shows its one-line **description**
  (`core.Result.Description`, set from the test at run time). `report --from`
  **backfills** the description from the live registry for older persisted
  results that predate the field, so a report built from a past run's JSON still
  shows what each test checks.

**Secret masking.** `core.Masker` replaces resolved secret values with a fixed
mask everywhere they could surface — config snapshots, step output, error
summaries — so no artifact leaks a credential. `NewMaskerFromSecrets` builds one
from every `secrets:` entry; a nil Masker is a no-op.

**Failure diagnostics.** `diag` collects best-effort k8s evidence *before a
namespace is torn down* — the event stream (`kubectl get events`), every pod's
current and `--previous` (crash-loop) logs, and a `describe` of every pod not
Running-and-Ready. It is **wired into the failure paths**: the `features` amp
flow (`suites/features/diagnostics.go`) and the `stress` combined-load
scenario (`suites/stress/combined_load.go`) call it via the `runLifecycle`
`onBeforeTeardown` hook — after the phases, before any teardown, on its own
budget — and persist via `T.AttachBytes`. The policy is `report.
collect_diagnostics` = `on_failure` (default) / `always` / `never`
(`diag.ShouldCollect`). Known gap: the core-level generic `env.Collect`
(`core/lifecycle.go`) currently discards, and the features *external-backing*
YB namespace isn't yet threaded to the hook — the amp namespace
(controller/endpoint/safekeeper) is fully covered.

## Standing environments: state + teardown

`ampqa run --setup-only` (or `--env-mode keep`) leaves an environment running,
registered in `~/.ampqa/envs.json` — the persisted source of truth for what's
still up.

```
ampqa run --setup-only --test presets/yb_k8s -c my.yaml   # stand up, leave running
ampqa run --tag regression --env-mode keep -c my.yaml     # keep the whole self-built control plane
ampqa env ls                                              # what's currently registered
ampqa env rm <id> -c my.yaml                              # tear down (re-resolves context from -c)
ampqa env reap --stale 24h                                # recover cloud leaks with no run tag
ampqa env refresh-secret <id> -c my.yaml                  # re-mint an expired ECR pull secret
```

`env rm` reconstructs the real teardown (cloud / universe / YBA / VM reap) from
the registry record using the **same** config that created it — always pass `-c`
with matching values, or override just the provider with `--env-provider`.

## Configuration

`configs/default.yaml` is the embedded base layer (`configs/configs.go`), always
loaded first — every framework default the code reads lives there with a comment
(`core/defaults_test.go` asserts it). Top-level sections: `sched` (capacity /
parallelism / lease weights), `env` (`ampstack` / `sharedk8s` / `universe` /
`vm` / `awsvm` / `exclusivek8s` — `env.sharedk8s.deploy` /
`env.sharedk8s.deploy_timeout` control the self-building control plane),
`functional` (amp-on-backing shape), `apps` (`cloud.aws` / `cloud.gcp` / `yba` /
…), `report` (`out_dir` / `junit_path` / `html_path` / retention caps /
`collect_diagnostics`), `chaos` (actions / ports / severity).

Precedence, later wins:

```
embedded configs/default.yaml  →  -c files (deep-merged, in order)  →
-jc JSON files  →  -o typed key=value overrides
```

Typed `-o` prefixes: `int:` / `bool:` / `float:` / `list:` / `json_file:` /
`json_text:`; dotted keys nest. Flags (`--parallel` / `--out-dir` / …) follow the
same rule against their config keys — an explicit flag wins, else the merged
value. `--no-default-config` skips the embedded layer (debugging only; each knob
still has a Go-level fallback).

```
ampqa run -c my.yaml -o sched.capacity=int:4
```

## For AI agents

- **Self-discover first** — `ampqa manifest` (command tree + tests + config keys)
  and `ampqa collect` (tests). Never assume a test/suite/flag exists.
- **Prefer `--json`** — every command emits the stable envelope; parse that, not
  human output.
- **Read exit codes precisely** — `4` (all selected skipped) is **not** a pass;
  `env_failed` / `broken` are not product signals. Report status faithfully.
- **Reproduce with `--seed <N>`** — replays workload/chaos randomness exactly.
- **Operational conventions (non-negotiable against real infra):**
  - Leave nothing standing unless asked. A bare `run` (`--env-mode build`) tears
    its own env down; only `keep`, `--setup-only` (and the deprecated `setup`),
    `attach` (`--env <id>`, which never owns the standing env), or a killed run
    leaves something up.
  - Touch only ampqa-owned infra — namespaces/instances carry `ampqa*` /
    `ampqa-managed-by` labels or `ampqa-run=<tag>` tags. Never act on anything
    without that marker.
  - Always tear down via the tracked path (`env rm <id>`, or `env reap
    --tag`/`--stale`), never manual `kubectl delete` / `aws ec2 terminate`.
  - After any live (non-`--fake-env`) run, leak-check: `ampqa env ls` plus a
    cloud-side `env reap --stale` for anything the registry lost track of.
