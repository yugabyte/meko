# ampqa — Framework Implementation Plan

**Scope:** the Go test framework only — the `ampqa` binary: core contracts,
scheduler + lease manager, environment providers, Apps/Tasks, chaos, reporters,
CLI. Companion to [`QA_FRAMEWORK_ARCHITECTURE.md`](QA_FRAMEWORK_ARCHITECTURE.md)
(the "what and why"); this document is the "how and in what order."

**Out of scope here:** the report portal (API/UI/files service/Victoria
Metrics), the MCP server, Jenkins/GH Actions pipeline definitions, and the
promotion machinery. The framework is built so it is **useful before the portal
exists** (stdout + JSON sinks first; the portal sink is one more implementation
of an interface that is stable from day one).

> **Implementation status:** this plan predates a lot of what has actually
> shipped in `test/framework/`, and some concrete choices below (package
> layout in §2, CLI surface in §4) no longer match the real binary. Where they
> differ, a note in-line points at the built behavior — run `ampqa --help` /
> `ampqa manifest` for ground truth. Built and exercised today: the engine
> (`core`/`sched`/`exec`/`report`), env providers (fake/docker/sharedk8s/
> exclusivek8s/vm/universe), the apps layer (controller, YBA/yugabyted, pg,
> sampleapp/benchbase/pgbench/workloadgen, cloud AWS+GCP — **no Azure**), the
> chaos engine, the perf tier with baselines, and a self-contained HTML report
> sink. Not built: the report portal, the MCP server, and CI pipeline
> definitions (all still M5+/§7/§8 roadmap as described below).

---

## 1. Ground rules

- **Language / toolchain:** Go, matching `controller/go.mod` (currently
  `go 1.25`). Lint = `golangci-lint` with the controller's config as the base.
  Every milestone ends green on `go vet`, lint, and the framework's own tests.
- **Standalone module — no cross-imports, ever.** `test/framework` is its own
  Go module (`github.com/yugabyte/yb-amp/test/framework`) and is **fully
  separate from all product code in this repo**: it imports nothing from
  `controller/` or any other product module, and no product code imports it.
  It talks to the product only **over the wire** — REST, psql, SSH,
  kubectl/docker — never by linking product source. `go build ./...` inside
  `test/framework` must succeed with the rest of the repo absent. (A `go.work`
  may exist for IDE convenience but must never be a build requirement.) This is
  what keeps the framework independent, evolvable at its own pace, and
  liftable into its own repo whenever wanted.
- **Controller client:** the generated client in `controller/internal/api` is
  `internal` — **not importable** from another module. The framework generates its
  own client from `controller/internal/api/openapi_spec.yaml` with the same
  pinned `oapi-codegen` (a `make framework-generate` target mirroring
  `controller-generate`), into `test/framework/apps/controller/gen`. A CI check
  regenerates and diffs, so spec drift fails fast.
- **No heavy work in-process — and nothing runs on the operator's machine.**
  The framework only coordinates (architecture §1), and **environments are
  always remote**: the dev laptop / CI agent issues CLI and API calls to remote
  instances, pods, and clusters — it never hosts an environment itself, not
  even for the dev loop. Any package that would buffer logs/metrics unbounded
  in memory is a review reject; everything streams.
- **The framework provisions infrastructure itself, not only consumes it.**
  Like the stress `SetupPortalTask` / `SetupK8SYugawareTask`: it can **start
  cloud VMs** (create → tag → wait for SSH → bootstrap software over SSH) via
  `apps/cloud`, and **create custom k8s namespaces, pods, secrets, and
  services from specs** — arbitrary objects, not just the amp Helm chart. Env
  providers and tasks compose these primitives.
- **KISS — the stress-framework layering, nothing more.** A small agnostic
  engine (`core`, `sched`, `exec`, `report` — knows nothing about what's being
  tested) and then **apps and helper classes for everything else**: all
  communication with the product or any of its parts is just different apps,
  atomic, flat, importable from any test, task, or check. No adapter layers,
  no dependency-injection ceremony, no registries beyond a plain map where one
  is genuinely needed (test/task lookup by name). Prefer plain structs and
  functions; add an interface only where two real implementations exist
  (sinks, env providers, exec transports, chaos actions). The engine staying
  agnostic is kept by common sense and review, not tooling. This simplicity is
  the versatility — it's why the stress framework could absorb scylladb, tidb,
  jepsen, and taqo as just more apps.
- **Parallel by design, from the first line.** Easy parallel execution is a
  fundamental property, not a milestone: every test is atomic and independent
  (its own env/project, no shared mutable state, no ordering assumptions), the
  lifecycle and all core contracts assume concurrent execution, and nothing in
  the authoring API makes it possible to write a test that only works
  sequentially. `--parallel N` is a first-class flag; running sequentially is
  just the worker pool at size 1, never a different code path. Reviews reject
  anything that would break under `-race` or concurrent tests (package-level
  mutable state, shared temp paths, fixed ports).
- **Every milestone ships something runnable.** No milestone is "internal
  refactoring only."

---

## 2. Package layout (target state)

The stress layout (`src/main` / `src/apps` / `src/tasks` / `src/suites`),
translated to Go. A small agnostic engine; everything that talks to the
product or any external system is an app; flat, atomic, no extra layers.

```
test/framework/                     module github.com/yugabyte/yb-amp/test/framework
  cmd/ampqa/                      main: CLI wiring only (cobra), no logic
  core/
    test.go                       Test, Suite, EnvSpec, registration (name → Test map)
    step.go                       Step spans, properties, attachments
    lifecycle.go                  per-test lifecycle driver (acquire→…→release)
    result.go                     Result + status taxonomy
    seed.go                       seed derivation (run seed → per-test → per-app)
    options.go                    typed options, -o overrides, resolved-config capture
    timeouts.go                   deadline hierarchy (run > test > step > op)
  sched/
    scheduler.go                  packing scheduler (SHARED bins + EXCLUSIVE queue)
    lease.go                      Resource Lease Manager (weights, capacity, pinning)
  exec/
    exec.go                       RemoteExecutor interface
    ssh/                          SSH transport (VMs; key auth, sessions, scp)
    k8s/                          kubectl exec / logs / port-forward / apply
    docker/                       docker exec / logs / cp
  artifact/
    artifact.go                   declarative artifacts (the stress artifacts.conf
                                  model): name + glob_path/url with {version}
                                  substitution from the versions{} block
    fetch/                        fetchers: github_release, s3, http, local
    deploy.go                     delivery to hosts/pods via exec transports:
                                  on-host download vs upload, fetch-once + copy
                                  host-to-host, unpack, target scope; local cache
  env/
    provider.go                   EnvProvider + Environment interfaces
    docker/                       single-container amp (+ YB) on a REMOTE docker
                                  host (ssh / DOCKER_HOST) — never the operator's box
    vm/                           cloud-VM environments: create instance via
                                  apps/cloud → wait ssh → bootstrap (the stress
                                  SetupPortalTask pattern)
    sharedk8s/                    project-per-test on a shared control plane
    exclusivek8s/                 namespace-per-test dedicated stack (up.sh path)
    remote/                       attach to an existing stack by URL
    registry/                     standing-env registry (file store now, portal later)
  apps/                           ← everything that talks to anything, side by side
    app.go                        App interface + base lifecycle (collect-before-kill)
    controller/                   amp controller REST wrapper (the stress
                                  yugaware_app equivalent): typed calls, async-task
                                  poll helpers; gen/ = oapi-codegen output
    cloud/                        AWS / GCP / Azure API wrappers (the stress
                                  apps/cloud equivalent) — ambient-auth first, §3
    pg/                           libpq client app (pgx): queries, asserts, pools
    pgbench/                      pgbench driver
    benchbase/                    benchbase/TPC-C driver (client pod or host)
    sampleapps/                   yb-stress-sample-apps driver
    workloadgen/                  small Go workload runner (schema+load, random ops)
  chaos/
    action.go                     ChaosAction interface
    nemesis.go                    seed-driven nemesis loop
    actions/                      podkill, prockill, ybnodekill, zoneout, netem, …
  check/
    checker.go                    Checker interface (oracles)
    invariants/                   always-on: log corruption scan, leak check, health
  task/
    task.go                       Task interface (name → Task map; ampqa task <name>)
    tasks/                        provision, teardown, load-data, collect, reap,
                                  run-workload
  report/
    reporter.go                   Reporter fan-out + Sink interface
    stdout/  jsonfile/  junit/    sinks
    portal/                       portal REST sink (implements the §7.1 contract)
test/suites/                      the actual tests
  functional/…  stress/…  perf/…
test/tasks/                       repo-level task definitions composed from Apps
```

**As actually built**, this target layout differs in a few places:
- `exec/` has `ssh/` and `k8s/` only — there is no `exec/docker/` subpackage
  (docker access lives in `apps/dockerhost` and `env/docker` directly).
- `env/` has `docker/`, `vm/`, `sharedk8s/`, `exclusivek8s/`, `universe/`,
  `fake/`, `reap/`, and `registry/` — there is no separate `env/remote/`
  package; attach-to-existing-stack is handled inside each provider. `env/`
  also gained `universe/` (the §5.4a Universe target — a YBA-managed or
  yugabyted cluster as the environment itself) and `reap/` (age/tag-based
  cloud-infrastructure reaping, exposed as `ampqa env reap`), neither of
  which is in the diagram above.
- `apps/cloud/` wraps **AWS and GCP only** — no Azure wrapper exists. `apps/`
  also has several real packages not listed above: `ampstack` (stands up a
  whole amp control plane), `ybastack`/`yba` (YBA-managed universes),
  `yugabyted`, `ybbacking`, `ybk8s`, `clientpod`, `dockerhost`, `yugabyte`.
  The workload driver is `apps/sampleapp` (singular), not `apps/sampleapps`.
- `chaos/` actions live as flat files (`actions.go`, `chaos.go`, `registry.go`,
  `runner.go`, `config.go`) rather than one file per action under
  `chaos/actions/`. The actions that exist today: pod delete, process
  kill/restart, node restart/stop, network partition/slowdown, packet loss —
  targeted at a pod or a host, so a YB-node kill is the same `process_kill`/
  `node_restart` action pointed at a YB target, not a dedicated
  `ybnodekill` action. There is no `zoneout` or `netem` action yet.
- `check/` (a standalone `Checker` interface + `invariants/` package) **does
  not exist** — oracle/invariant logic is embedded ad hoc in `core`, `env`,
  and `chaos` rather than factored into its own package. This is the one
  piece of the M4 plan (below) that has not landed as designed.
- `report/` has a real `html/` sink (`ampqa report --from <out> -o out.html`)
  that is not in this diagram at all, in addition to `stdout/`, `jsonfile/`,
  and `junit/`. There is no `report/portal/` sink yet — the portal itself
  hasn't been built (§7 remains roadmap).
- There's also a `baseline/` package (perf baseline handling for the perf
  tier's regression comparisons) not shown above.
- `test/framework/suites/` holds the actual tests (not `test/suites/` as
  drawn) — `canary/`, `functional/`, `perf/`, `presets/`, and `stress/`.
  There is no `examples/` suite.

That's the whole structure. Another system to talk to → another directory under
`apps/`. Another fault → another file under `chaos/actions/`. Another ops
routine → another task. The engine never changes for any of these.

---

## 3. Core contracts (the API tests are written against)

These interfaces are the stability boundary — milestones may rework internals
freely, but changes here after M1 need a deliberate call.

```go
// A test. Registered in an init() via core.Register(...).
type Test struct {
    Name     string            // "functional/branch_cow_isolation"
    Suite    string
    Category Category          // Functional | Stress | Perf | LongRunning
    Env      EnvSpec           // sharing + shape + weight (+ YB image/version)
    Tags     []string          // smoke, nightly, feature:branching, owner:...
    Versions VersionSpec       // min/max product & YB version bounds; outside → auto-skip
    Timeout  time.Duration     // 0 → category default
    Run      func(ctx *T) error
}

// EnvSpec — architecture §4.2.
type EnvSpec struct {
    Sharing Sharing   // Shared | Exclusive | Persistent
    Shape   Shape     // SingleContainer | SharedControlPlane | DedicatedStack | Remote
    Weight  int       // lease units; 0 → shape default
}

// T is the per-test context handed to Run.
type T struct {
    Env      Environment       // provisioned per EnvSpec (or attached)
    Reporter StepReporter      // Step(), attachments, metrics
    Seed     *Seed             // deterministic sub-seeds: t.Seed.Fork("nemesis")
    Options  Options           // typed, resolved config for this run
    // Fail/Skip/Deadline helpers; context.Context embedded for cancellation.
}

// Environment — a live target (architecture §4.2, §5).
type Environment interface {
    ControllerURL() string            // "" for shapes without a controller
    Versions() DetectedVersions       // product build + YB version, read live at setup
    Connect(db string) (*pgx.Conn, error)
    Exec() RemoteExecutor             // ssh / kubectl / docker for this env
    Handles() EnvHandles              // ids: namespace, containers, project, endpoint
    Collect(ctx context.Context, into ArtifactSink) error // logs, cores, configs
}

type EnvProvider interface {
    Provision(ctx context.Context, spec EnvSpec, seed *Seed) (Environment, error)
    Teardown(ctx context.Context, env Environment) error
    Attach(ctx context.Context, ref string) (Environment, error) // --env <id>, remote URL
}

// App — the atomic reuse unit (architecture §4.2). One deployable/runnable
// thing with a uniform lifecycle; composable in tests, tasks, nemesis, manual envs.
type App interface {
    Start(ctx context.Context, env Environment, opts AppOptions) error
    WaitReady(ctx context.Context) error
    Signal(ctx context.Context, sig AppSignal) error   // stop, pause, kill…
    Collect(ctx context.Context, into ArtifactSink) error // ALWAYS collect before kill
    Close(ctx context.Context) error
}

// ChaosAction — one fault, seed-parameterized.
type ChaosAction interface {
    Name() string
    Targets(env Environment) ([]Target, error)
    Inject(ctx context.Context, t Target) (Revert, error) // Revert undoes (or no-op)
}

// Checker — an oracle. Verifies a property of the env/data, not "no error happened".
type Checker interface {
    Name() string
    Check(ctx context.Context, env Environment, rep StepReporter) error
}

// Sink — a reporter backend. The Reporter fans out to N sinks; sinks never
// block the test (bounded queues, drop-with-warning on overflow for non-portal sinks).
type Sink interface {
    RunStarted(RunInfo) error
    ResultEvent(ResultEvent) error   // test start/end, step start/end, attachment, metric
    RunFinished(RunSummary) error
}
```

**Result status taxonomy** (portal schema already has the column; the framework
sets it honestly by *where* the failure happened):

| Status | Meaning | Set when |
|---|---|---|
| `passed` | assertions + invariants green | — |
| `failed` | product defect signal | assertion/checker/invariant failed during RUN |
| `env_failed` | environment could not be provisioned/attached | failure before `setup` completes |
| `broken` | framework bug | panic in framework code, sink crash, impossible state |
| `timed_out` | deadline exceeded | test/step watchdog fired |
| `skipped` | not run | filtered out, dependency failed, `--resume` skip |

**Seed derivation:** one run seed (`--seed`, else random, always logged +
reported) → per-test seed = `H(runSeed, testName)` → per-component sub-seeds via
`Seed.Fork("nemesis")`, `Seed.Fork("workload")`. Same run seed ⇒ same everything,
including the nemesis schedule (architecture §4.3, §5.6).

**Timeout hierarchy:** run deadline > test timeout (category default:
functional 10 m, stress per-test, perf per-test) > step timeout (optional) >
operation timeouts in exec/clients (always set; nothing calls out without a
deadline). Watchdog fires → status `timed_out`, teardown still runs under its
own independent deadline.

### Configuration — the stress-framework model, verbatim

The stress config system is one of its best features; this framework copies its
semantics exactly:

- **Layered config files:** `-c/--config` accepts **multiple files, deep-merged
  in order** (later wins), starting from a checked-in `configs/default.yaml`
  that can `include` others (versions, artifacts, per-env profiles). Splitting
  concerns across files is the intended usage — e.g.
  `-c configs/default.yaml -c ~/secrets.yaml -c my-changes.yaml`: base profile,
  private credentials, and this run's deltas each live in their own file and
  merge into one config. `-jc` accepts JSON config files merged on top.
- **`-o key=value` overrides** on top of everything, with the same typed
  prefixes: `int:`, `float:`, `bool:`, `list:` (comma-separated),
  `json_file:` (value read from a file), `json_text:` (inline JSON), and
  dotted keys nesting deep (`-o task_options.portal_region=us-west-2`).
- **One resolved config** comes out the other end — a typed struct the whole
  framework reads from (the stress `Config` object) — and the fully resolved
  set is **recorded into every result** so any run is inspectable and
  replayable.
- Everything about how the framework behaves — versions, env shapes, cloud
  regions, timeouts, workload params, reporter targets — flows through this
  one mechanism, so Jenkins, GH Actions, a laptop, and an agent all manipulate
  the framework the same way: files for the stable shape, `-o` for the deltas.
- **Format: YAML by default, HOCON also accepted.** Files are parsed by
  extension (`.yaml`/`.yml` → YAML with an explicit `include:` key;
  `.conf`/`.hocon` → HOCON via a Go parser, e.g. `gurkankaymak/hocon`) and both
  land in the same merged tree — so a stress-style HOCON profile and a YAML
  secrets file mix freely in one `-c` chain. The shipped defaults, the
  `artifacts {}` declarations, and the `versions {}` block are all YAML.

### Artifacts — declarative, versioned, cached

The stress `artifacts.conf` model, in Go. An `artifacts {}` config section
declares every external thing tests work with — workload apps, utilities,
datasets, binaries:

- `glob_path` + `url` carry `{version}` placeholders substituted from the
  central `versions {}` block — one bump re-points everything.
- `type` picks the fetcher: `github_release` (resolved via the GH API, token
  from secrets), `s3`, `http`, `local`. A new source = a new fetcher, nothing
  else changes.
- Delivery flags per artifact: `remote_download` (fetch directly on the target
  host/pod — curl / aws-cli there) vs local fetch + upload;
  `internal_download` (fetch/upload **once**, then copy host-to-host for the
  rest — one egress, N hosts); `remote_unzip`; `target` scope (client /
  server / all).
- Local artifact cache keyed by name+version, size-compared to skip
  re-transfer on repeated runs.

Apps consume artifacts by name (`artifact.Deploy(ctx, env, "benchbase")`) and
never hand-roll downloads.

### Cloud & k8s credentials — ambient-auth first

The framework talks to **AWS and GCP APIs and k8s** directly (cloud apps
under `apps/cloud/{aws,gcp}`, k8s via `exec/k8s` and the env providers — there
is no Azure wrapper). Credential rule, copied from the stress cloud apps:
**use what the machine already has — zero extra parameters on an authed
box.**

- **AWS:** SDK default credential chain (env vars → shared config/SSO profile →
  instance role) — an authed `aws` CLI just works (stress `aws.py` is bare
  boto3 for the same reason).
- **GCP:** Application Default Credentials (`gcloud auth application-default`,
  service-account env, metadata server).
- **k8s:** standard kubeconfig resolution (`KUBECONFIG`, `~/.kube/config`,
  current context; `--kube-context` to pick another).

Explicit credentials (key files, service principals) remain available **through
the same config system** (secrets section / `-o`) and take precedence when set —
for CI boxes that aren't interactively authed. But the default path is: if
`aws` and `gcloud`/`kubectl` work in your shell, `ampqa` works with no
credential setup at all.

### Version compatibility — one framework, every product version

No per-version framework branches: master runs against **any** yb-amp build
and any YB backing version (architecture §4.9). Mechanics, mirrored from the
stress framework:

- `Environment.Versions()` detects the product build + YB version **live at
  setup** and records them into the result.
- `Test.Versions` (a `VersionSpec` with min/max bounds, per release line when
  needed — the `YugabyteRequiredVersions` pattern) → the lifecycle **auto-skips**
  out-of-range tests with status `skipped` and the reason recorded. Hand-rolled
  `if version < X { return }` early-returns in test bodies are a review reject.
- `core` ships version helpers (`ParseVersion`, `Compare`,
  `FeatureEnabled(env, feature)`) — the `is_feature_enabled_for_release`
  equivalent — for tests that run everywhere but gate a step.
- Apps own cross-version API differences: an app checks the live version and
  takes the other route (moved endpoint, renamed field, legacy flow), so tests
  stay version-agnostic and version logic lives once, in the app.

### Extending it

Adding anything is adding a file or a directory, never editing the engine: a
new system to talk to = a new app under `apps/`; a new fault = a new action in
`chaos/actions.go` (flat file today, not yet a `chaos/actions/` directory); a
new report destination = a `Sink` under `report/`; a new ops routine = a
`Task` composed from existing apps (`task/`, listed with `ampqa task`); a new
environment kind = an `EnvProvider` under `env/`. (A pluggable `Checker`/oracle
abstraction is still on the roadmap — see the `check/` note in §2.) That's the
stress-framework growth path (it absorbed scylladb, tidb, jepsen, taqo as just
more apps), replayed in Go.

---

## 4. CLI surface

**This section described the target CLI as planned; the command tree that
actually shipped differs in shape** — there is no `provision`/`check`/
`teardown` split at the top level, no `--dry-run`/`--resume`/`--branch`/
`--commit`/`--report` flags, and `--env` is `--env-provider` (which env
*kind*, not an environment id to attach to — there's no attach-by-id flag on
`run` yet). The real, current surface (verify with `ampqa <cmd> --help` or
`ampqa manifest`):

```
ampqa run        [--suite <re>] [--test <re>] [--tag <t>]... [--parallel N]
                 [--per-env-parallelism N] [--seed S]
                 [--env-provider fake|docker|sharedk8s|exclusivek8s|universe|auto]
                 [--fake-env] [-c file]... [--jc file]... [-o key=value]...
                 [--out-dir dir] [--junit path] [--no-default-config]
ampqa setup      -t <test-re> [--name label] [--ttl dur] [same config/env flags as run]
                 # runs ONLY setup, leaves the env standing & registered; see `env ls`/`env rm`
ampqa collect    [--suite <re>] [--test <re>] [--tag <t>]...   # list registered tests
ampqa task       [name] [same config flags as run]             # run one Task, or list built-ins bare
ampqa env        ls                                            # list standing environments
ampqa env        rm <id> [--skip-teardown] [--force] [--env-provider p]
ampqa env        reap --tag <runTag> | --stale <dur> [--cloud aws|gcp]
ampqa report     --from <run-out-dir|results.json> -o <report.html>
ampqa manifest                                # full command tree + tests + config keys as JSON
ampqa version
```

- Global `--json`: every command emits the stable envelope
  `{kind, version, command, data, truncated}`; exit codes on `run`: `0` all
  selected tests passed/passed+skipped, `1` a test failed/broke/env-failed/
  timed-out, `2` usage, `3` selection matched no tests, `4` tests were
  selected but none executed (e.g. no provider available).
- Options: `-o key=value` with type prefixes (`int:`, `bool:`, `float:`,
  `list:`, `json_file:`, `json_text:`) — the stress framework convention,
  dotted keys nest. The fully resolved option set is recorded into every
  result.
- `ampqa setup` is today's stand-in for the planned `provision` +
  detach-and-leave-running behavior; `ampqa env rm <id>` reconstructs the
  real teardown from the registry record (the planned `teardown` verb).
  There is **no standalone `ampqa check` health-assert command** — Mode-2
  long-running (§2, §5.5) periodic health-checking is not built.
- Portal read/write subcommands (`ampqa test get/results/steps`, `files`,
  `comment`, …) mentioned elsewhere in this plan **do not exist** — there is
  no portal yet, so nothing to stub them against. `ampqa report` (HTML from a
  completed run's local JSON output) is the one reporting capability that
  exists today and needs no portal.

---

## 5. Milestones

Ordering follows the architecture's phasing but is resequenced so that **every
milestone is demoable** and the riskiest contracts freeze earliest. Sizes are
rough engineer-weeks for one person who knows the codebase.

> **Status:** M0–M4's scope (scheduler/lease manager, k8s + VM env providers,
> apps/workload drivers, the chaos engine, canary/functional/perf/stress
> suites) has substantially landed in `test/framework/` — with the concrete
> differences noted inline below and in §2/§4. M5 (portal sink) and the MCP
> server have not been started; the env registry remains file-backed.

### M0 — Skeleton + single-container env + first green tests (~2 w)
**Goal:** `ampqa run --suite functional` passes against a single-container amp
+ a YugabyteDB container **on a remote docker host** (an EC2 test instance per
the test-instances convention, reached over SSH / `DOCKER_HOST=ssh://…`) — the
operator's machine only coordinates, from day one. Execution runs through the
worker-pool path from day one (pool size 1 in M0 — the packing/lease logic
arrives in M1, but there is never a separate sequential code path, and the
first tests are already written parallel-safe).

- Module scaffolding, `go.work`, Makefile targets (`framework-build`,
  `framework-test`, `framework-generate`, lint).
- `core`: Test/registry, Step, the **full config system** (layered `-c` files
  deep-merged in order + `-jc` JSON + `-o` typed overrides with dotted nesting —
  the stress semantics, see §3; resolved set recorded per result), result
  model, seed plumbing (recorded, not yet consumed by chaos), basic lifecycle
  driver (provision → setup → run → collect → teardown; sequential).
- `exec`: docker transport (exec/logs/cp) — enough for the docker provider.
- `env/docker`: single-container provider — start the `neon-endpoint` image
  with `DEV_PROVISION_SCHEMA=true` + a `yugabytedb/yugabyte` container,
  wait-ready, psql reachable, teardown. Options: shared YB across tests vs
  per-test YB.
- `apps/pg`: pgx-based client app (connect, exec, assert helpers).
- `report/stdout` sink; Reporter fan-out shell.
- 3 real functional tests (e.g. DDL/DML round-trip, restart-compute WAL replay
  via container restart, branch smoke if reachable without controller).
- **Framework self-tests:** unit tests for registry/options/seed/lifecycle with
  a fake provider.

**Accept:** fresh checkout → `make framework-build && ampqa run --suite functional`
green from the operator's machine against a **remote docker host** (nothing
runs locally); `ampqa manifest` and `--json` envelope work.

### M1 — Scheduler, lease manager, taxonomy, JSON sink, canary (~2–3 w)
**Goal:** parallelism is real and provably correct; the framework is honest
about failure kinds.

- `sched`: packing scheduler — SHARED bin-packing with `per_env_parallelism`,
  EXCLUSIVE queue; lease manager with weights, capacity, blocking acquisition,
  pinned leases (for later PERSISTENT). Pure logic, no k8s.
- Status taxonomy wired through the lifecycle (env_failed vs failed vs broken
  vs timed_out); panic recovery per test; teardown-always with independent
  deadline.
- Version compatibility plumbing (§3): `Environment.Versions()` live
  detection, `Test.Versions` bounds → auto-skip with recorded reason, version
  helpers (`ParseVersion`/`Compare`/`FeatureEnabled`).
- Timeout hierarchy + watchdogs.
- `report/jsonfile` + `report/junit` sinks; `--fake-env` (fake provider, real
  scheduler+reporting — this shipped instead of a separate `--dry-run` flag).
- `ampqa collect` (test enumeration as JSON).
- **Canary suite** (`test/suites/canary`): tests that deliberately pass, fail,
  panic, hang, and env-fail — asserts each lands with the right status, teardown
  always ran, leases always released. Runs in the framework's own CI on the
  fake provider + docker provider.
- **Kill-the-framework drill:** SIGKILL mid-run test (scripted) → next `ampqa env ls`
  shows orphans; `ampqa env rm` / reaper cleans. (Automated resume comes in M6.)

**Accept:** a mixed run (20 SHARED + 4 EXCLUSIVE docker envs, capacity 3) shows
packing + blocking behavior in logs; canary suite green; scheduler/lease unit
tests deterministic (fake clock, race detector on).

### M2 — Remote-exec toolkit + k8s providers + attach (~3 w)
**Goal:** the same tests run against real k8s stacks; envs can be reused.

- `exec/ssh` (VM targets: key auth, command, scp, long sessions) and `exec/k8s`
  (exec/logs/port-forward/apply via client-go, standard kubeconfig resolution).
- `apps/cloud`: AWS / GCP wrappers (Azure was planned here but never built)
  with **ambient-auth-first** credential resolution (§3) — **VM lifecycle
  included**: create/tag/terminate
  instances, wait-for-SSH, plus list/describe (instances, zones). Feeds the
  `env/vm` provider (create → bootstrap over SSH — the stress SetupPortalTask
  flow) and, later, the chaos VM/zone actions in M4.
- `exec/k8s` also covers **custom namespaces/pods/secrets/services from
  specs** (apply/create/delete arbitrary objects) — the SetupK8SYugawareTask
  pattern — so env providers and tasks can stand up anything, not only the amp
  Helm chart.
- `apps/controller`: the controller app — generated client + typed wrapper,
  async-task poll helpers (202 → poll `GET /tasks/{id}` with backoff +
  deadline), idempotent delete, error taxonomy mapping.
- `env/sharedk8s`: project-per-test on a standing control plane
  (createProject → createEndpoint → connect; deleteProject cascade on
  teardown). In-env project slots = `per_env_parallelism`.
- `env/exclusivek8s`: namespace-per-test dedicated stack driving the
  parameterized `deploy/up.sh` path (or its extracted library equivalent);
  labels every object for the reaper.
- `env/remote`: attach by controller URL (+ kubeconfig context) — `--env`.
- `env/registry`: file-backed standing-env registry (`~/.ampqa/envs.json` or
  repo-local state dir), `provision`/`teardown`/`env ls` wired; `--skip-setup`.
- Keep-on-failure flag; reaper task (`ampqa task reap --older-than 24h`) sweeping
  by label/age.
- Collect: component logs (pageserver/safekeeper/compute/controller pods),
  configs, cores → ArtifactSink (JSON sink stores to a local dir tree).

**Accept:** the M0 functional suite runs unchanged on (a) shared k8s control
plane, (b) exclusive namespaces, (c) `--env` attach to a pre-provisioned stack —
selected purely by EnvSpec/flags; provision-once + 3 re-runs with `--skip-setup`
works (the TPCC-style loop from architecture §4.3).

### M3 — Apps, workload drivers, Tasks (~3 w)
**Goal:** sustained load through client pods; primitives reusable outside tests.

- `artifact/`: the declarative artifact system (§3) — fetchers
  (github_release, s3, http, local), version substitution, local cache, and
  delivery via the exec transports (on-host download, fetch-once + host-to-host
  copy, unpack, target scope). Lands first in this milestone because every
  driver below consumes it.
- `apps` base: uniform lifecycle, **collect-before-kill enforced in the base**
  (the stress `report_kill` lesson — losing workload logs is not an option).
- Client-pod mechanics: launch a workload image in the env's namespace (or a
  container next to a docker env), command it, stream progress.
- Drivers: `pgbench`; `workloadgen` (Go: schema setup + data load with
  verifiable patterns — sequence-numbered rows, checksummed payloads; random
  ops driven by the test seed); `benchbase` (TPC-C image + config templates,
  load/execute phases split so M2's env-reuse gives cheap re-execute);
  `sampleapps` (existing jar/image, start → wait-for-load → monitor → collect →
  kill).
- `task`: Task interface + `ampqa task` runner; builtins: `provision`,
  `teardown`, `load-data`, `run-workload`, `collect`, `reap`. Tests refactored
  to compose the same Apps (tests = tasks + assertions).

**Accept:** a stress-shaped test — provision exclusive env, 30-min workloadgen
load via client pod, verify counts/checksums, collect all logs — green; the
same load runnable standalone via `ampqa task run-workload` against a
registry env.

### M4 — Chaos engine + checkers + always-on invariants (~3 w)
**Goal:** nemesis runs are reproducible and verified, not vibes.

- `chaos`: action registry + seed-driven nemesis loop (which action, which
  target, when — all from `Seed.Fork("nemesis")`; schedule logged as a step
  timeline so any run's fault sequence is readable and replayable).
- Actions (each a plugin): endpoint pod kill/restart; in-pod process kill
  (pageserver/safekeeper/compute via exec); controller kill;
  **YB node kill/restart** (tserver + master pods) and **zone-outage
  simulation** (kill/cordon everything in one zone label); network delay/loss
  (netem via privileged helper) — network actions are k8s-first, docker-later.
- Revert discipline: every action returns a Revert; nemesis reverts on loop
  exit; teardown reverts anything left.
- `check`: Checker interface; first oracles — data checksum/rowcount vs
  workloadgen's expected state; branch COW divergence check; read-after-restart.
- **Always-on invariants** (run for every test, every category): component-log
  corruption/FATAL scan (reuse the marker list from `test/e2e/storage`), leaked
  k8s objects / projects / endpoints check, env health at teardown.
- Chaos allowed only when `EnvSpec.Sharing == Exclusive` — enforced by the
  framework, not by convention.

**Accept:** a nemesis test (workload + 5 random faults incl. one YB-node kill)
passes with data verified; re-run with the same `--seed` produces the identical
fault schedule; a deliberately-injected corruption marker in a log flips the
run to `failed` via the invariant, not via the test body.

### M5 — Portal sink + long-running entrypoints (~2 w, portal-API-contract dependent)
**Goal:** results stream to the portal contract; Mode-2 long-running works
end-to-end against the registry.

- `report/portal`: implements architecture §7.1 (`POST /runs`, `/results`,
  steps live-PATCH, attachments via files-service upload, metrics, perf
  datapoints, final PATCH). Built against a **contract-test stub server**
  checked into the framework — the portal team implements the same contract;
  either side can change only by changing the shared contract tests.
- Streaming discipline: attachments upload as files close (not end-of-run);
  step events batched with small flush intervals; bounded queues.
- `provision`/`check`/`teardown` full flow for PERSISTENT envs: pinned lease
  accounting, registry state transitions, `check` = attach + invariants +
  data-oracle + report health result. Manual-category provision (spec file,
  TTL, no checks) rides the same path.
- `--resume <run-id>` groundwork: run manifest written incrementally by the
  JSON sink (which tests completed), consumed to skip on resume.

**Accept:** full run streams into the stub portal with correct entity
lifecycle; `provision → 3 × check (one after a container restart) → teardown`
green with health results recorded; resume skips completed tests after a
framework kill.

### M6 — Hardening + perf primitives + docs (~2 w)
**Goal:** trustworthy under failure; perf runs statistically sane; devs onboarded.

- Retry policy: env provisioning retries (bounded, new env each attempt,
  `env_failed` only after exhaustion); assertion failures never auto-retry.
- Perf run primitives: N repetitions, warmup discard, median/trimmed-mean
  aggregation (never raw mean), variance gate (refuse to compare when spread
  exceeds threshold), perf datapoint emission per §7.1.
- `--resume` finished; flaky-quarantine tag honored by selection
  (`--exclude-tag quarantine` default in CI profiles).
- Docs: `test/framework/README.md` (authoring guide: write a test in ≤30 lines,
  write an App, write a chaos action, run locally, debug with keep-on-failure),
  godoc pass on the core package.
- CI for the framework itself (GH Actions job in-repo): lint + unit + canary
  on fake provider + docker-provider smoke.

**Accept:** canary + kill-drill + resume all green in CI; a new dev follows the
README and lands a functional test without touching framework internals; a perf
test produces stable numbers across 5 repetitions or refuses with a clear error.

**Total: ~17–18 engineer-weeks** for one person; M2/M3 parallelize well across
two people (exec+providers vs apps+tasks) which compresses the calendar to
~10–12 weeks.

---

## 6. Testing the framework itself (standing, not a phase)

- **Unit:** scheduler, lease manager, seed derivation, options parser, status
  taxonomy — fake provider + fake clock, `-race` always on.
- **Canary suite (M1):** deliberate pass/fail/panic/hang/env-fail tests
  asserting statuses, teardown-always, lease release. Gates every framework PR.
- **Contract tests (M5):** portal sink vs stub server — the shared contract.
- **Kill-the-framework drill:** scripted SIGKILL mid-run; orphan detection +
  reaper + (M5+) resume verified.
- **Dry-run mode:** `--fake-env` exercises selection/scheduling/reporting with
  zero infrastructure (the fake provider stood in for a dedicated `--dry-run`
  flag) — runs on every PR.
- **Docker-provider smoke:** the M0 suite in CI — the one real-integration
  gate that needs only a docker daemon.

---

## 7. Decisions locked for implementation (and one deliberately open)

| Decision | Choice | Why |
|---|---|---|
| Layering | small agnostic engine + everything else is apps/helpers (KISS, no adapter layers or DI ceremony) | the stress-framework ideology: atomic apps made it absorb whole other systems without core changes |
| Runner | custom runner, tests registered in Go (`core.Register`) | SHARED bin-packing + lease semantics don't map onto `go test -parallel`; CLI/report control end-to-end |
| `go test` bridge | **deferred, kept cheap** | `T` mirrors `testing.T` semantics (Fail/Skip/Deadline) so a thin adapter can come later without rewriting tests |
| Controller client | generate own from spec via pinned oapi-codegen | `controller/internal/…` is unimportable; drift caught by CI regen-diff |
| Config | stress semantics verbatim: layered `-c` files (deep-merge, include, split by concern — base/secrets/deltas) + `-jc` JSON + typed `-o` overrides with dotted nesting; **YAML default, HOCON also parsed** (by extension, same merged tree) | the ergonomics devs/agents already know; one mechanism manipulates everything; resolved set recorded per result |
| Cloud/k8s creds | ambient-auth first (AWS default chain, GCP ADC, kubeconfig — no Azure); explicit keys via config override | an authed laptop (`aws`/`gcloud`/`kubectl`) runs `ampqa` with zero credential setup — the stress apps/cloud behavior |
| Env registry | file-backed now, portal-backed later behind one interface | framework must work before the portal exists |
| Chaos on shared envs | hard-blocked by the framework | architecture §5.1/§5.6; convention is not enforcement |
| Workload logs | collect-before-kill enforced in the App base | the `report_kill` lesson — this is where debugging evidence dies |

**Deliberately open until M2 experience:** whether `env/exclusivek8s` keeps
shelling to a parameterized `deploy/up.sh` or extracts it into Go (client-go
applies). Start by shelling (zero drift with the ops path), revisit when the
provider's error handling needs more fidelity than exit codes.

---

## 8. Risks

- **Endpoint cold-start dominates short functional tests** (image pull +
  supervisor start order). Mitigations already in the design: single-container
  provider for the bulk (M0), shared control plane with pre-warmed endpoints as
  a later provider option; measure in M2 before optimizing.
- **Portal contract churn** while the portal is built in parallel → the stub
  contract tests (M5) are the fence; the JSON sink means the framework never
  blocks on the portal.
- **netem/zone-outage actions need privileged pods** — cluster policy work
  outside the framework; sequenced last inside M4 so the milestone doesn't hang
  on it.
- **Spec drift** between controller API and generated client — CI regen-diff
  (ground rules) makes it a failing check, not a runtime surprise.

---

## 9. Execution environment — what exists today (inventory, 2026-07-10)

Where the framework will actually be developed and run. Point-in-time facts,
verified from the dev machine; revisit when infra changes.

### Local dev machine — coordinator only, never a host
Nothing runs on this machine — it is weak on purpose and only issues CLI/API
calls to remote targets. What it has is exactly what a coordinator needs:
- **CLI clients:** docker 28.3.3 (as a *client* for remote docker hosts via
  `ssh://`), Go 1.26.5 toolchain (repo requires ≥1.25) for building `ampqa`.
- **All four CLIs ambient-authed** — exactly the credential model §3 assumes:
  `aws` (SSO role in account **454529406029**), `az` (Pay-As-You-Go Dev/Test
  subscription), `gcloud` (project `yugabyte`, user + build service account),
  `kubectl` (six GKE contexts). `ampqa` needs zero credential parameters here.
- Environments live remotely: docker envs on EC2 test instances, everything
  else on the GKE clusters below.

### k8s clusters (the Integration category)
Six GKE contexts, project `yugabyte`, region `us-west1`:

| Context | Role | Notes |
|---|---|---|
| `itest-dev` | **first target for framework dev runs** | reachable now; GKE v1.34, default-pool nodes 16 vCPU / 64 GB (5+ nodes) |
| `itest-system`, `itest-release` | itest/stress production clusters | coordinate before scheduling framework load there |
| `itest-mcs`, `yb-gke-portal`, `k8s-portal-yb` | other existing infra | not framework targets for now |

- **No `amp` namespaces exist anywhere today** — clean slate. `deploy/up.sh`
  defaults to `NS=amp` and is GKE-flavored (GKE LB annotations), so the shared
  control plane and exclusive-namespace providers land naturally on these
  clusters.

### Images (ECR)
- Registry `454529406029.dkr.ecr.us-west-2.amazonaws.com` — **the same AWS
  account the local SSO role lives in**, so image listing/pulling works with
  personal dev credentials; the `dshubin_bot` + pull-secret flow from
  `deploy/README.md` is only needed *inside* clusters (12 h token expiry
  caveat applies to long-lived namespaces).
- `amp/endpoint` and `amp/controller` both have fresh tags (endpoint
  `phase1-20260703-…`, controller `tailnet-…` from Jul 8) — prebuilt images
  exist, so early milestones never block on building the slow endpoint image.

### AWS EC2 test instances (manual VMs / SSH-transport testing)
The established `test-instances` convention (see `AWS_TEST_INSTANCES.md` in
yb-stress-test / the `test-instances` skill) is the ready-made recipe for any
VM the framework needs — e.g. exercising the `exec/ssh` transport or a
OneBox-on-VM run:
- us-west-2, subnet `subnet-11798e59`, keypair `yb-dev-aws-2`
  (`~/.ssh/yb-dev-aws-2.pem`), user `ec2-user`, AlmaLinux 9, public IP
  required.
- Prefer ARM `m7g.large` / `m7g.xlarge` (matches production, cheaper).
- Mandatory tags (`Name=spilshchikov-*`, `yb_task=stress-dev`,
  `yb_owner=spilshchikov`, `yb_dept=qa`, `autoclean=true`) — untagged
  instances get cleaned up.
- Track and terminate — instances are never left running idle.
