# yb-amp QA Framework — Architecture Proposal

**Status:** draft for discussion · **Scope:** a developer-owned, extensible Go test
framework + report portal for **yb-amp / yb-light** (Yugabyte's Neon fork:
serverless Postgres — pageserver, safekeeper, compute, storage_controller,
controller — backed by YugabyteDB).

This document describes *how the system is structured and how it runs*. No code.
It borrows the proven shape of the YugabyteDB stress framework
(`yb-stress-test`) and its report portal (`qa-services/stress-report-service`),
but adapts it to yb-amp's reality: the product is **itself a multi-tenant
control plane**, so we lean on the product's own API to create test
environments instead of provisioning cloud VMs over SSH.

> **Implementation status** (`test/framework/`, as tracked in git): the engine,
> env providers (fake, docker, shared/exclusive k8s, cloud VM, standalone
> universe), the apps layer, the chaos engine, the perf tier, workload drivers
> (sample-app/benchbase/pgbench/workloadgen), and a self-contained HTML report
> sink are all built and exercised by real suites (`canary`, `functional`,
> `perf`, `presets`, `stress` — see `ampqa collect`). The **report portal
> (§7), MCP server, and CI pipelines (§8) described below remain roadmap** —
> `ampqa run`/`setup`/`env` write only to a local file-backed registry today,
> and there is no Azure support anywhere in the codebase (cloud backings are
> AWS and GCP only). Where this document states a capability in the present
> tense, check `ampqa --help` / `ampqa manifest` / the `test/framework` source
> before assuming it is live — sections below were written as a proposal and
> have not all been reconciled with what shipped.

---

## 1. Goals, principles, non-goals

### Goals
- **One place developers own.** The framework, the tests, and the report portal
  live in (or next to) the `yb-amp` repo. Devs read, extend, and run everything
  without leaving their world.
- **Three test tiers on one base:** functional (small, fast, correctness),
  stress (chaos / load / volume / soak), performance (latency, throughput,
  cold-start, regression-vs-baseline).
- **Extensible by construction.** New tests, new environment providers, new
  workloads, new chaos actions, new reporters plug in behind interfaces.
- **Parallel at the core.** Parallelism and a bounded resource pool are
  first-class in the framework, not bolted on by Jenkins.
- **One system, not six.** YugabyteDB accumulated its quality systems after the
  fact — phabricator, jenkins, csi, taqo, itest, stress, perf, release tooling —
  and a feature owner has to learn most of them to ship a feature. yb-amp starts
  clean: one framework and one portal cover functional, stress, perf,
  long-running, and release qualification, so there is a single place to add a
  test and a single place to read results.
- **Agent-friendly, MCP-first.** Every capability — run tests, read results and
  logs, triage a failure, re-run, file/link an issue — is exposed as an API and
  as MCP tools from day one, so AI agents can test, investigate failures, and
  drive fixes end-to-end, not only humans clicking a UI.
- **Executor-agnostic, CI-driven.** The framework is one binary runnable from a
  dev's local machine, a Jenkins agent, or a GitHub Actions runner; CI triggers
  (pre-commit / post-commit / nightly / on-demand) → run → report, with a live
  portal and a Slack summary.

### Guiding principles
- **Nothing heavy runs on the orchestrator.** The framework process only
  *coordinates* — but, exactly like the stress framework, it does so over
  **whatever transport reaches the target**: the controller REST API, **SSH into
  VMs and pods**, `kubectl`/`docker exec`, libpq, and running arbitrary
  apps/commands remotely (starting/stopping/inspecting sample apps, workload
  generators, `yb` tools, log & core collection). It never runs the load itself;
  all load, all DB work, all workloads run **inside the target stack and in
  separate client pods** — never on the machine running the framework, and
  **environments are never hosted locally either**: the operator's machine (or
  CI agent) only sends CLI/API calls to remote instances, pods, and clusters,
  including for the dev loop. (This is the single most important
  non-functional rule; it is what keeps a Jenkins agent from OOMing the way
  the stress agents historically did.)
- **Use the product's own multi-tenancy.** A deployed yb-light control plane
  hosts many projects / branches / endpoints. A "test environment" is usually
  *a project on a shared control plane*, not a freshly-provisioned cluster.
  This makes environments cheap and parallelism natural.
- **Atomic tests.** Each test acquires an environment, runs, collects, and
  releases — self-contained, no shared mutable state between parallel tests.
- **Custom on purpose.** A bespoke framework (like the stress one) can be bent
  to any need; we accept the maintenance cost in exchange for total control.
- **A versatile foundation, kept simple (KISS).** Same ideology as the stress
  framework, which is exactly two things: a small **agnostic engine**
  (`src/main`: config, runners, reporting, connections — assumes nothing about
  what is being tested), and **apps** — atomic wrappers, one per system or
  tool, usable anywhere (`src/apps` holds the YugabyteDB and YugaWare wrappers
  right next to benchbase, jepsen, scylladb, tidb, taqo, terraform…). Tasks
  run ops ansible-style from the same apps; periodic LRU checks ride the same
  machinery. That's the whole architecture — and its simplicity is what made
  it that versatile. This framework does the same in Go: the test engine is
  separate and agnostic; **all communication with the product or any of its
  parts is just different apps and helper classes**, each atomic, each usable
  from any test, task, or check. No extra layers, no ceremony — new needs are
  met by adding an app or a task, not by reworking the core.

### Non-goals (for now)
- Not replacing Rust unit tests (`cargo test`), the Rust compute integration
  framework (`test/integration/compute`), or the Go controller tests
  (`controller/…_test.go`).
  Those stay where they are and keep running in CI. This framework is the
  **system-level** tier above them (functional-against-a-real-stack, stress,
  perf) and the **single reporting destination** they can all publish into.
- Not a general-purpose cloud VM orchestrator. yb-amp doesn't need one.

---

## 2. Where this sits in the overall test pyramid

The YugabyteDB coverage process classifies tests by framework and by a quality
rating (correctness × failure-injection × concurrency × cross-feature ×
sanitizer). yb-amp already has the lower tiers; this framework fills the top and
gives every tier one portal.

The pyramid, top to bottom:

| Layer | Status | Covers |
|---|---|---|
| **System tier** (Go framework, against real stacks) | **this framework** | functional · stress/chaos · perf |
| **Integration** | already exists | Rust integration suite (`test/integration/*`), controller Go integration, pg_regress |
| **Unit** | already exists | `cargo test` (Rust) · `go test` (Go) |

All tiers publish results into **one report portal**.

### Test categories this framework owns

| Tier | What it does | Example targets |
|---|---|---|
| **Functional** | Small, deterministic, fast. Drives the control-plane API and SQL data path; asserts correctness. | project/branch/endpoint lifecycle; branch copy-on-write isolation; WAL replay after restart; GetPage correctness; extension sanity; DDL/DML round-trips; pg_regress against a live stack. |
| **Stress** | Chaos + load + volume + soak. Kills components, partitions the network, degrades the backing store, drives sustained workloads, runs for hours. | pageserver/safekeeper/compute pod kills during load; controller crash mid-task; YB backing-store degradation; large-data ingest + compaction; many-branch fan-out; multi-hour soak with a nemesis loop. |
| **Performance** | Latency / throughput / serverless-specific KPIs; compare to a baseline, flag regressions. | benchbase TPC-C tps + p50/p95; pgbench; pageserver GetPage latency (pagebench); **cold-start time** (endpoint create → first query); **branch-create time**; WAL ingest lag; LFC hit-rate. Historical trend across builds/branches. |
| **Long-running** | Hours-to-days duration. Either a single long soak, **or a detached environment that outlives the run** and is health-checked on a schedule. Reports continuously. | multi-day soak with huge data + a background nemesis loop; a persistent loaded environment left up for days while Jenkins re-invokes the framework hourly/daily to assert it's still healthy (no corruption, no leaks, data still consistent, WAL/GC/compaction still progressing). |

### Where tests run — three placement categories

Orthogonal to the tiers above is *where* a test runs. Three broad categories,
each mapping to environment shapes (§5) and executors (§8):

| Category | What runs there | Environment shapes | Executor |
|---|---|---|---|
| **1. OneBox** — a single machine | Lots of fast, short tests: unit tests (unit = a file), integration tests of one full component, e2e tests of interactions between components. | single-container amp (§5.3), `yb_light_local` | GitHub Actions / Jenkins |
| **2. Integration** — real k8s clusters | Fewer, slower tests that need many machines and external infra (e.g. an NLB): e2e on the full stack, perf, long-running (LRU-style), nemesis/chaos. | `SHARED` / `EXCLUSIVE` / `PERSISTENT` (§5.1, §5.2, §5.5) | Jenkins |
| **3. Manual** — spec → standing system | Start the system with a given spec and hand it over to a human or agent — PoC and evaluation cases. On-demand, rare. | provision-only standing env (no scheduled checks, TTL/reaper) | local / Jenkins |

Quality-wise all three are treated the same: tests are watched, and fixed as
they break — a flaky OneBox test is as much a defect as a failing k8s e2e.

#### Long-running tests — two modes

These are worth separating because the second one breaks the normal
"one invocation owns the environment for its lifetime" assumption.

- **Mode 1 — long soak (single invocation).** The framework holds the
  environment for the whole duration (hours+), drives a large workload with a
  background nemesis loop, and streams steps/metrics/logs to the portal the
  entire time. Same lifecycle as a stress test, just long. The environment is
  `EXCLUSIVE` and torn down at the end. The main concern is that the framework
  process must stay lightweight over a multi-hour run (no unbounded log/metric
  buffering — a lesson from the stress framework's agent-OOM history).

- **Mode 2 — detached environment + periodic health assert (the key pattern).**
  The framework runs in phases across *separate invocations*:
  1. **`provision` invocation** — stand up an environment, load it with data, and
     start a **persistent background load** (a workload runner pod that keeps
     driving traffic on its own). The framework then **records the environment in
     a persistent registry and exits** — it does **not** hold the environment
     open, and the environment's lease is a **long-lived / pinned lease** (it is
     *not* returned to the general pool, so the scheduler never reclaims it).
  2. **`check` invocations (scheduled)** — Jenkins runs a **cron pipeline
     (hourly / daily)** that re-invokes the framework in a lightweight
     **check/assert mode** against the registered environment: verify the stack
     is healthy (all components up), the persistent load is still running, data
     is still consistent (checksums / row counts / no corruption markers in
     component logs), and background maintenance (WAL trim, GC, compaction) is
     progressing. Each check **posts a health result to the portal** ("long-run
     env X: OK at T", or a failure with evidence) and exits.
  3. **`teardown` invocation** — on demand or after a max lifetime, destroy the
     environment and release the pinned lease.

  This is exactly the stress portal's **LRU (long-running-universe) monitoring**
  concept (`lru_setups` / `lru_actions` tables, periodic actions, a dedicated
  monitoring view) — reused here for yb-light. It's the right tool for catching
  slow leaks, unbounded growth, GC/compaction stalls, and durability drift that
  only appear after days of continuous load.

  **Architectural implications** (reflected in the sections below):
  - **Env model (§5):** a third environment disposition beyond `SHARED` and
    `EXCLUSIVE` — a **`PERSISTENT`** environment with a pinned lease that the
    Resource Lease Manager excludes from reclamation and counts against capacity
    for as long as it lives.
  - **Framework (§4):** a **`check` entrypoint** (assert-only, no provisioning)
    and a persistent **environment registry** (a table the portal also reads) so
    a later invocation can find and attach to an env a previous one created.
  - **CI (§8):** a **scheduled cron pipeline** distinct from the run pipelines,
    whose only job is to fire `check` invocations against every registered
    long-running environment.
  - **Portal (§7):** a **long-running / monitoring view** listing live persistent
    environments and their rolling health-check history — separate from the
    per-run test list.

---

## 3. High-level architecture

The flow, top to bottom:

1. **Triggers** — pre-/post-commit (GitHub Actions) · nightly cron · on-demand
   (params / portal UI) — kick off the CI job.
2. **CI orchestrator job (Jenkins or GitHub Actions)** — builds/resolves the
   image tag under test, registers a `test_run` in the portal, and launches
   agents that run the framework, e.g.
   `ampqa run --suite … --parallel N --report <url>`.
3. **Go test framework (the `ampqa` binary)** — core pipeline:
   `test registry → scheduler → bounded worker pool → per-test lifecycle`. Its
   components:
   - **Resource Lease Manager** — semaphore + capacity model.
   - **Env Provider** — shared co-tenant | exclusive | remote.
   - **Workload / Client driver** — drives client pods (benchbase, pgbench, custom SQL…).
   - **Chaos engine** — pod kill, partition, store degrade.
   - **Reporter** — streams steps/logs/metrics to the portal.
4. **The framework talks to three things** (and does nothing heavy itself):
   - **yb-light stack(s)** — controller + storage_controller + pageserver/safekeeper
     + compute endpoints, backed by YugabyteDB — over REST (controller API) + libpq.
     *(the product under test)*
   - **Client pod(s)** — the workload runners — over kubectl/docker.
   - **Report portal** — Go API + Postgres, Go files service, Victoria Metrics,
     Web UI (list / detail / perf tab) — over HTTP.

The framework is a thin, I/O-bound coordinator; everything below it is where the
work actually happens.

**How it drives targets — like the stress framework.** The framework is a **full
remote-execution toolkit, not just an API client**. The controller REST API is
simply the cleanest way to *create* environments; everything else about running
a test uses the same machinery the stress framework relies on — **SSH into VMs
and pods**, `kubectl`/`docker exec`, libpq, and **starting / stopping /
inspecting any app or command** on a node or pod (sample apps, workload
generators, `yb` tools, log & core collection). Any transport that reaches the
target is fair game.

---

## 4. The Go test framework

### 4.1 Why Go, and where it lives
Go is **already a first-class language in yb-amp**: the entire `controller/` is
Go, with an OpenAPI spec (`controller/internal/api/openapi_spec.yaml`) and an
`oapi-codegen`-generated REST client. The framework **reuses that generated client
verbatim** to drive environments — no hand-rolled HTTP, and it stays in sync
with the API by construction. Building a Go framework introduces nothing foreign.

**Proposed repo layout (in `yb-amp`):**
- **`test/framework/`** — the framework, the `ampqa` module/binary, mirroring the
  stress layout (`src/main` / `src/apps` / `src/tasks` / `src/suites`):
  - `core/` — Suite/Test/Task interfaces, lifecycle, options, seeds, results
  - `sched/` — packing scheduler, worker pool, Resource Lease Manager
  - `exec/` — remote-execution transports: SSH, kubectl/k8s, docker
  - `env/` — environment providers: single-container amp (docker), shared
    control plane (project-per-test), exclusive namespace stack, remote attach;
    plus the standing-env registry
  - `apps/` — the atomic primitives, everything that talks to the product or
    any external system, side by side, each usable anywhere: the controller
    REST wrapper, the **amp control-plane app** (`ampstack` — stands up the
    whole yb-light stack), the **YBA app** (create/manage universes through
    YugabyteDB Anywhere — the stress `yugaware_app` equivalent), the
    **yugabyted app** (start/stop a yugabyted cluster over SSH/pods), pg
    client, pgbench, benchbase, sample apps, workload generator. Starting a
    YugabyteDB cluster — a YBA-managed universe or a yugabyted cluster — is an
    app like any other, reusable both as the backing store for a yb-light stack
    and as a **test target in its own right** (§5)
  - `chaos/` — action interface, seed-driven nemesis loop, and the actions
    (pod kill, process kill, YB node kill, zone outage, netem) in one place
  - `check/` — checkers/oracles + always-on invariants (log-marker scan, leak
    checks)
  - `task/` — task runner + tasks (provision, teardown, load-data, collect,
    reap, run-workload)
  - `report/` — Reporter fan-out + sinks (stdout, JSON, JUnit, portal)
- **`test/suites/`** — the actual tests, one package per feature area (`functional/`, `stress/`, `perf/`)
- **`test/tasks/`** — runnable non-test units (provision an env, load a dataset, run a workload standalone, collect) built from the same Apps — the stress framework's `src/tasks/` equivalent
- **`test/portal/`** — report service (see §7) — or a sibling repo; see §7 note

The engine (`core`/`sched`/`exec`/`report`) stays agnostic — it doesn't know
what an app talks to; kept that way by common sense and review, not by
machinery. Everything else is apps and helpers: atomic, flat, importable from
any test, task, or check.

**The framework is fully separate from the product's source code.** It is its
own Go module with **no cross-imports in either direction** — it never links
`controller/` or any other product code (the controller client is generated
from the OpenAPI spec file, not imported), and talks to the product only over
the wire: REST, psql, SSH, kubectl/docker. It builds standalone without the
rest of the repo, evolves at its own pace, and can be lifted into its own repo
at any point.

Tests and framework in one tree = the "one place devs work" requirement. The
existing `test/integration/` (Rust) and `controller/…_test.go` are untouched neighbors.

### 4.2 Core abstractions (conceptual)
- **Test** — the atomic unit. Declares: its **category** (functional/stress/perf),
  its **EnvSpec** (see below), its **tags** (smoke, nightly, feature area, owner),
  and a body that runs against a handed-in environment + reporter.
- **EnvSpec** — the environment a test needs, and *how it's allowed to be shared*.
  Two orthogonal fields:
  - **sharing policy** — `SHARED` (this test may co-tenant on one environment
    alongside other `SHARED` tests) or `EXCLUSIVE` (this test needs an
    environment to itself and runs alone on it).
  - **shape** — what kind of stack + weight: a project on a shared control plane,
    a single-container amp, or a whole dedicated stack with pod-kill access
    (see §5).
  This is the crux of the parallelism model: **not every test uses
  multitenancy.** A batch of functional tests declares `SHARED` and packs onto
  one environment; a destructive or perf-sensitive test declares `EXCLUSIVE` and
  gets its own environment. Both kinds run **concurrently within a single run**.
- **App** — the atomic reuse unit, mirroring the stress framework's `src/apps/`
  layer (the reason that framework is so versatile). An App wraps one
  deployable/runnable thing behind a uniform lifecycle — a workload runner
  (benchbase, pgbench, sample apps), a chaos action, a client, a collector, a
  deploy driver. Apps know how to start, drive, observe, and clean themselves
  up, and are **reused across every context**: the same App works inside a
  functional test, a stress nemesis run, a perf measurement, a standalone task,
  and a manually provisioned environment. New capability = new App, composed
  from existing primitives — nothing is welded to one test.
- **Task** — a runnable unit that *does* something rather than *asserts*
  something, built from the same Apps and primitives (the stress framework's
  tests-and-tasks split). `ampqa task <name>` runs one directly: provision an
  environment, load a dataset, run a workload standalone, collect logs, deploy
  a portal. Tasks make the primitives usable outside tests — for manual envs
  (§2), for CI plumbing, for one-off ops — and tests are then just tasks plus
  assertions and reporting.
- **Suite** — a group of tests sharing setup helpers (mirrors the stress
  framework's `AbstractStressSuite`). Suites are just organizational; the
  scheduler operates on individual tests.
- **Step** — a named, timed, status-bearing span inside a test, with a
  properties bag and attachments. Directly mirrors the stress framework's
  `Step` context manager and maps 1:1 to the portal's `step` rows. This is the
  unit of live progress the portal shows.
- **Environment** — a handle to a running yb-light target: a controller URL, a
  project/branch/endpoint set, connection strings, and the k8s/docker handles
  needed for chaos. Produced by an **Env Provider**. A `SHARED` environment may
  host **many** co-tenant tests over its lifetime (each in its own project for
  data isolation); an `EXCLUSIVE` environment hosts exactly one test then is torn
  down. Released back to the pool (or destroyed) when its last occupant finishes.
- **Reporter** — receives run/test/step/attachment/metric events and fans them
  out to **one or more sinks** (see §4.5). Sinks are independent, so a run can,
  e.g., stream to the portal *and* post a Slack summary *and* dump JSON at once.

### 4.3 Test lifecycle
Per-test lifecycle, in order:
`acquire lease → provision env → setup → RUN (steps) → collect logs+metrics → teardown → release lease`.
**Teardown and lease-release always run, even on failure or panic.**

**`provision env` and `setup` are optional stages.** A test can attach to an
already-provisioned environment (`--env <id>`) and skip data setup
(`--skip-setup`), so one expensive env + dataset is built once and tests run
against it **many times with slight config changes, without redeploying** —
e.g. TPC-C: provision one environment with an 8-hour data load once, then
re-run the 30-minute execute phase repeatedly against different configs or
builds. When stages are skipped, teardown is skipped symmetrically (the
standing env outlives the test and is reclaimed by its own TTL/teardown).
- **collect** pulls component logs (pageserver/safekeeper/compute/controller),
  cores, and configs into the files service, and snapshots metrics into Victoria
  Metrics — all keyed by the test result id.
- **teardown** deletes the project/endpoint (or the whole dedicated stack); the
  lease is released only after teardown so capacity accounting stays honest.
- On failure, an **optional keep-alive** flag leaves the env up for debugging
  (mirrors the stress `keep_universe_on_failure` knob).

**Reproducibility — the random seed.** Every test execution gets a **random
seed** (auto-generated unless pinned with `--seed`), and *all* randomness in the
run derives from it: workload data shapes and value generation, the nemesis
loop's action sequence, timings, and targets, randomized test inputs. The seed
is recorded in the test result on the portal and printed in the logs, so a
failure caused by a particular random combination — say, a specific chaos
ordering that races a compaction — is not a one-off: re-running with the same
seed (`ampqa run --seed <s> …`) replays the same sequence and turns a "random
chaos flake" into a reproducible scenario. (Same discipline as the stress
framework's `random_seed`.)

### 4.4 Parallelism: a packing scheduler + the Resource Lease Manager — the heart of the design
This is where we improve on the stress model. In `yb-stress-test`, Jenkins fans
out one build per test and caps concurrency with `max_tests_per_portal`.
Here, **the framework owns parallelism internally**, and the scheduler is a
**bin-packer** because tests have two very different demands:

- **`SHARED` tests** are **packed onto shared environments.** The scheduler keeps
  a small set of shared environments alive; each accepts up to
  `per_env_parallelism` co-tenant tests at once (each test in its own project for
  data isolation). A batch of, say, 40 functional tests might run across 2 shared
  environments — one batch of functional tests sharing a portal.
- **`EXCLUSIVE` tests** each **provision their own environment**, run alone, and
  tear it down. 10 exclusive destructive tests ⇒ up to 10 environments spun up in
  parallel — each test creating an environment for itself.
- **Both happen at once in a single run.** A run is typically *one shared
  environment (or a few) carrying the functional batch* **plus** *N exclusive
  environments each carrying one stress/perf test* — all concurrent.

The **Resource Lease Manager** is what bounds all of this. A lease is taken per
**environment** (not per test), with a **weight** = the capacity that environment
shape consumes (a shared control plane and a dedicated stack cost different
amounts of pods/quota). Total capacity is configurable and ultimately bounded by
a real k8s `ResourceQuota`. So:
- Spinning up a shared environment or an exclusive environment each acquires an
  environment lease; **co-tenant `SHARED` tests do *not* each take an
  environment lease** — they share the one their environment already holds (they
  only take an in-env project slot).
- When capacity is exhausted, the next environment request **blocks and waits for
  a lease to be released** — the "hit limits → wait for freed resources"
  behavior. The functional batch keeps flowing on the environments already up;
  new exclusive environments queue until room frees.
- Two knobs: `--parallel` (max concurrent tests overall / desired ambition) and
  the lease capacity (max concurrent environments / reality). Effective
  throughput = whatever both allow.

Worked example — one run with `per_env_parallelism = 8` and a lease capacity of
**6 environments**. `SHARED` tests (`f1…f40`) pack onto shared envs; `EXCLUSIVE`
tests (`s1…`) each take their own:

| Lease | Environment | Running now |
|---|---|---|
| 1 | env#1 — SHARED | `f1`–`f8` (co-tenant functional) |
| 2 | env#2 — SHARED | `f9`–`f16` (co-tenant functional) |
| 3 | env#3 — EXCLUSIVE | `s1` (pageserver-kill soak) |
| 4 | env#4 — EXCLUSIVE | `s2` (perf: benchbase, isolated) |
| 5 | env#5 — EXCLUSIVE | `s3` |
| 6 | env#6 — EXCLUSIVE | `s4` |
| — | *(queued)* | `s5`, `s6`, … — wait for an exclusive env to free |

As `f1`–`f8` finish, env#1 pulls the next functional tests (`f17`, `f18`, …); as
`s1` finishes and frees env#3, the queued `s5` starts. Capacity is never
exceeded; nothing is dropped.

### 4.4.1 Does 100 / 1,000 tests overwhelm a single Go process? No.
The test count is **not** what limits us, and Go is never the bottleneck:
- **Goroutines are ~2 KB and multiplex onto a few OS threads.** Holding 1,000 —
  or 100,000 — tests "in flight" as queued work items costs kilobytes. Parking
  980 waiting tests on a channel is free.
- **The real ceiling is environments, not goroutines.** Running 1,000 yb-light
  stacks is not feasible; actual concurrency is bounded by **cluster capacity, enforced
  by the lease manager**. 1,000 tests through a lease capacity of ~20 envs =
  at most 20 provisioned at once, the rest parked. So "1,000 tests in one
  process" is a *scheduling* problem (Go's strength), not a concurrency-count
  problem.

So one process owning parallelism is the right **default**, and it scales far
past any real suite size. The only "messy at scale" risk is process hygiene —
stream logs/metrics to the portal, never buffer them in the framework (the stress
agent-OOM lesson) — which is a discipline issue, not a parallelism-model issue.

### 4.4.2 When to shard across Jenkins child tasks (the stress-framework model)
Sharding is a **fallback lever**, reached for when it buys something the test
count *doesn't* force:
- **Fault isolation / blast radius** — one process crash (OOM, panic, lost agent)
  kills coordination for the whole run. Per-test panics are recovered; a dead
  agent cannot be. Child builds mean one dead shard ≠ dead run. (This is why the
  stress framework fans out one build per test.)
- **Long-run durability** — a many-hour, thousand-test run in one process is a
  single long-lived thing; if the agent dies at hour 6 the run is lost unless
  checkpoint/resume exists. Several shorter child builds recover naturally.
- **Horizontal I/O scale** — several agents pulling against a very large cluster
  in parallel, each with its own scheduler slice.

Either way the **shared ceiling is the k8s `ResourceQuota`** on the test
namespace(s), so independent agents still can't over-commit the cluster — the
lease/quota is the real throttle. **MVP: single agent.** Multi-agent sharding is
a later scale/isolation lever, not a day-one requirement.

### 4.5 Extensibility surfaces
Extension is adding a file, not editing the engine — an app, an action, a
checker, a sink, a task is one new package/struct implementing a small
interface:
- **Apps** — a new system or tool to talk to = a new app. This is how the
  stress framework grew from YugabyteDB to scylladb/tidb/jepsen/taqo without
  core changes.
- **Env Providers** — add a new way to get a stack (e.g. a bare-metal lab)
  without touching tests.
- **Exec transports** — SSH / kubectl / docker today; anything that can reach
  a target is one more transport.
- **Workload drivers** — add a new load generator (see §6).
- **Chaos actions** — add a new fault (see §5.6).
- **Reporter sinks** — a reporter fans out to any combination of these, and new
  ones plug in behind the same interface:
  - **Custom report portal** (§7) — the primary sink; live steps/logs/metrics.
  - **JSON dump** — write the collected run/test/step/metric data as plain JSON
    files (per test or per run). The simplest possible sink: no portal needed,
    great for local dev, offline runs, ad-hoc analysis, or archiving raw data.
  - **Slack** — post status messages (run start, pass/fail rollup, a failing-test
    alert, a long-running health-check failure) to a channel.
  - **stdout / JUnit XML** — human-readable console output and a CI-consumable
    JUnit file.
- **Test registration** — tests self-register with tags; selection is by
  regex/tag exactly like the stress framework's `-s suite -a test` + `@attrs`.

### 4.6 Configuration — one mechanism for everything
The stress framework's config system is copied wholesale, because it's what
makes the framework manipulable from anywhere: **layered config files**
(`-c file1 -c file2 …`, deep-merged in order, `include` supported; a checked-in
default profile at the bottom) plus **`-o key=value` overrides** with typed
prefixes (`int:`, `bool:`, `float:`, `list:`, `json_file:`, `json_text:`) and
dotted keys nesting deep. Splitting concerns across files is the intended
usage — a base profile, a private secrets file, and a per-run deltas file each
stay separate and merge into one config
(`-c configs/default.yaml -c ~/secrets.yaml -c my-changes.yaml`). Format:
**YAML by default**, with **HOCON accepted too** (parsed by file extension into
the same merged tree), so stress-style `.conf` profiles remain usable. Everything the framework does — versions, env
shapes, cloud regions, timeouts, workload parameters, reporter targets — flows
through this one mechanism, so a config file, a Jenkins parameter, a GH Actions
input, a laptop invocation, and an agent all steer the framework identically:
files for the stable shape, `-o` for the deltas. The fully resolved config is
recorded into every result.

### 4.6a Secrets — credentials, tokens, and keys
Anything sensitive — GitHub tokens, cloud API keys, SSH private keys, service
account JSON — flows through one named-secret mechanism, copied from the stress
framework's `Secret`. Each secret is declared by name in a `secrets` config
section (layered like all config, so a base file, a private secrets file, and
`-o` overrides all contribute) and supplied in any of three variants:
- **token** — the value inline (or via `-o secrets.x.token=…`);
- **file** — a path to a key/cert/JSON on disk (e.g. an SSH `.pem`, a GCP
  service-account JSON);
- **env** — the name of an environment variable holding the value (so a `.env`
  or the ambient shell carries it, nothing sensitive in the config files).

Anything that needs a credential — the artifact fetchers (`secret = "..."`
references), the cloud apps, the YBA app creating providers — resolves it by
name, and secret values are **masked in logs and never recorded into results**.
This is what lets cloud keys and SSH keys reach the YBA provider setup without
being hardcoded or leaked.

### 4.6b Infrastructure hygiene — reuse, don't pollute
A hard rule when the framework touches shared cloud accounts: **create no
redundant or orphan resources, reuse shared ones, and clean up what we do
create.** The load-bearing case is YBA provider creation — YBA by default
*generates a new SSH keypair and uploads it to AWS*, which at company scale
exhausts the account's keypair limit and litters it with garbage. The framework
never lets it: it passes an **existing shared keypair** (`yb-dev-aws-2` by
default — the key everyone already has, name + local `.pem` supplied as a
secret) to the provider, exactly as the stress framework does, so no per-run key
is ever minted. The same discipline applies everywhere the framework provisions
cloud resources: tag them for cleanup, reap them, and prefer a shared/existing
resource over a freshly-created one.

### 4.7 Artifacts — the versioned things we work with
Tests and tasks constantly need *things from elsewhere* — workload apps and
utilities (benchbase tarball, sample-app jars, client tools), datasets
(pre-built dumps, benchmark data), binaries. The stress framework solves this
with a declarative artifact system, and this framework copies it:

- **Declared in config** (an `artifacts` section — YAML by default, layered
  like everything else): each named artifact states its `glob_path` (filename)
  and `url`, both with `{version}` placeholders substituted from the central
  `versions` block — bump one version key and every reference follows.
- **Sources:** GitHub release assets (resolved via the GH API, token from
  secrets), S3 objects, plain https, local files — one fetcher per source type;
  a new source kind is just another fetcher.
- **Delivery is part of the declaration:** download directly **on the target
  host/pod** vs fetch locally and upload; **fetch once and copy host-to-host**
  (one download, rsync to the rest — cheap at scale); unpack after landing;
  and a **target scope** saying which hosts get it (client / server / all).
- **Cached locally** (an artifacts dir keyed by name+version, size-checked to
  skip re-transfer), so repeated runs don't re-download the world.

Apps consume artifacts by name ("deploy `benchbase`, unpack, run") and never
hand-roll downloads — which is exactly what keeps adding a new tool or dataset
a config entry, not code.

### 4.8 Cloud & k8s access — ambient credentials
The framework talks to **AWS, GCP APIs and to k8s** directly (cloud wrapper
apps under `apps/cloud/{aws,gcp}`, like the stress `apps/cloud`; kubeconfig
for k8s — there is no Azure backing). Credential rule: **use what the machine
already has** — the AWS default credential chain, GCP Application Default
Credentials, and the standard kubeconfig context. On a box where `aws`,
`gcloud`, and `kubectl` are already authed, the framework needs **no
additional credential parameters at all**. Explicit keys/service principals
remain settable through the config system (secrets / `-o`) and win when
provided — for non-interactive CI boxes.

---

### 4.9 Version compatibility — one framework for every product version
There are **no per-version framework branches, ever**: one framework (master)
runs against **any** yb-amp build and any YB backing version, exactly the way
the stress framework stays compatible with every YugabyteDB release line. The
mechanics, copied from stress:

- **Auto-detect versions from the live environment** — the product build
  (controller/image version) and the YugabyteDB version are read from the env
  at setup and recorded into the result; nothing is assumed from config alone.
- **Declarative version bounds on tests** (the `YugabyteRequiredVersions`
  pattern): a test declares min/max versions — per release line where needed —
  and the framework **auto-skips** it (status `skipped`, reason recorded) when
  the target doesn't qualify. Never hand-rolled `if version < X: return`
  early-returns inside test bodies.
- **Feature gates inside tests** (the `is_feature_enabled_for_release`
  pattern): when a test *runs* on all versions but one step only applies to
  some, a helper answers "is this feature available on this version" and the
  test branches or skips the step.
- **Version checks inside apps**: apps handling API differences between
  versions compare the live version and **take the other route** — an endpoint
  that moved, a field that was renamed, an older flow for older builds — so
  tests stay version-agnostic and the version logic lives once, in the app.

## 5. Environment provisioning for yb-amp

The defining difference from the stress framework: yb-light is a control plane,
so most environments come from **calling the product's own API**. But the
framework also **provisions raw infrastructure itself** when an environment
needs it — the stress `SetupPortalTask` / `SetupK8SYugawareTask` pattern:
**start cloud VMs** (create → tag → wait for SSH → bootstrap software over
SSH, via the cloud apps) and **create custom k8s namespaces, pods, secrets,
and services from specs** — arbitrary objects, not only the amp Helm chart.
Env providers and tasks compose these primitives, and every environment is
remote (a VM, a docker host, a cluster) — never the operator's machine.

A test's **EnvSpec** (§4.2) picks one of these; **the choice is per test — not
all tests use multitenancy**, only the ones that opt into `SHARED`.

### 5.1 Shared environment — many co-tenant tests on one control plane (`SHARED`)
**Only non-destructive, non-perf tests co-tenant.** Anything that kills
components, simulates zone outages, or measures performance gets a dedicated
environment (§5.2) — a perf number taken while a neighbor's nemesis is killing
YugabyteDB nodes is garbage. That is exactly what the per-test EnvSpec (§4.2)
encodes; sharing is opt-in per test, never assumed.

For tests that don't need isolation (most **functional**, and perf that only
measures the SQL/API path without needing a clean box). A long-lived control
plane (controller + storage_controller + one YugabyteDB) is deployed once (via
`deploy/up.sh` / the `yb-amp-controller` Helm chart) and **reused across many
tests** in the run. Each co-tenant test still gets **its own project** for data
isolation, created/dropped through the controller REST API: `createProject` (auto
default branch `main`) → `createEndpoint` → run psql/workload → `deleteProject`
(cascades) → free the in-env project slot.

The resource ladder **User → StoragePool → Project → Branch → Endpoint** and the
async **Task** model (202 + `Location` + poll `GET /tasks/{id}` to
`succeeded|failed`) are exactly what the framework drives. The scheduler packs up
to `per_env_parallelism` such tests onto one shared environment concurrently
(§4.4); the whole batch shares one environment lease, so it's cheap. A run may
keep a few shared environments if the functional batch is large.

### 5.2 Exclusive environment — a whole stack per test (`EXCLUSIVE`)
For tests that must kill a pageserver, partition safekeeper↔compute, degrade the
backing store, or need guaranteed perf isolation. Each such test gets its **own
isolated stack** — its own k8s namespace (own YugabyteDB StatefulSet + own
controller + endpoints), stood up by the same `deploy/up.sh` path parameterized
by namespace (or a docker-compose stack for a single-box run). One environment
lease each; runs alone on it; torn down entirely at the end. This is where "10
tests create 10 environments for themselves" happens — bounded by lease
capacity, the rest wait.

### 5.3 Single-container environment — a whole amp in one docker container
The endpoint image already packs the full data path (endpoint_storage,
safekeeper, pageserver, compute_ctl + Postgres) into a single container, and
with `DEV_PROVISION_SCHEMA=true` it self-provisions its schema against any
YugabyteDB. That makes the cheapest possible environment: **one docker container
per test** (plus a shared or per-test YB), no k8s at all — running on a
**remote docker host** (a test-instance VM or a CI runner's docker), driven
over SSH; the operator's machine only coordinates. Each functional test can
get its own disposable amp in parallel — or several tests can share one. This
is the default shape for the functional bulk and for pre-commit checks.

It is not a substitute for the real thing: control-plane behaviour (controller,
deploy driver, endpoint lifecycle) and anything k8s-specific still needs the
full stack on a real k8s cluster with all parts deployed (§5.1/§5.2) — because
that is how the product actually runs. The EnvSpec shape decides per test.

### 5.4 Remote / pre-existing stack (attach, don't provision)
Point the framework at an already-deployed stack (staging, a dev's `deploy/up.sh`,
a nightly-kept environment) by URL. Runs read-mostly functional and perf
suites against real infrastructure. Mirrors the stress framework's
"existing portal" path and `deploy/smoke-test.sh`'s remote-controller model, and
the `remote_cluster` opt-in marker the legacy pytest runner already uses.

> There is a `yb_light_local` "cluster-in-a-box" binary — it stays a
> product-developer tool for hand-driven loops. The framework itself never
> hosts environments on the operator's machine: even its cheapest shape runs
> on a remote docker host, and CI uses the shared/exclusive k8s environments.

### 5.4a Universe target — a YugabyteDB cluster as the environment (`Universe`)
Not every target is a yb-light stack. A YugabyteDB cluster itself can be the
environment under test — either a **YBA-managed universe** or a **yugabyted
cluster** — driven by the YBA app and the yugabyted app (§4.1). Three modes,
chosen by config, mirroring the attach/provision split used elsewhere:
- **attach to a YBA universe** — the YBA app looks the universe up by name/id
  and hands back its YSQL connection (tserver host:ports); teardown is a no-op
  (never delete someone's universe) unless the framework owns it.
- **attach by address** — a plain host:port list points at any running
  YugabyteDB / yugabyted cluster; teardown no-op.
- **provision a yugabyted cluster** — the yugabyted app starts a fresh cluster
  on configured nodes (VMs over SSH, or pods) and tears it down at the end.
The environment exposes a YSQL connection the same way the amp shapes expose
their compute endpoint, so the pg and workloadgen apps and their tests run
against a universe unchanged. This is what lets the framework **give a universe
as a target** — the same atomic apps that provision yb-light's YB backing store
also stand a universe up as a first-class target in its own right.

### 5.5 Persistent environment (`PERSISTENT`) — detached, health-checked on a schedule
For long-running Mode 2 (§2). Same shape as an `EXCLUSIVE` stack, but its lease
is **pinned**: the provisioning invocation records it in the **environment
registry** and exits without tearing it down, and the Resource Lease Manager
**excludes it from reclamation** (it keeps counting against capacity so the pool
never over-commits, but no scheduler ever reclaims it). A persistent background
workload pod keeps load on it. Later `check` invocations look it up in the
registry, attach, assert health, post a result, and exit. Destroyed only by an
explicit `teardown` invocation or a max-lifetime reaper.

The same `provision` entrypoint also serves the **manual** placement category
(§2): stand up an environment from a spec and hand it to a human or agent for
PoC / evaluation work — no scheduled checks, just a standing environment
recorded in the registry with a TTL so the reaper eventually reclaims it.

### 5.6 Chaos for a storage-disaggregated system
Because durability lives in **YugabyteDB + safekeeper WAL**, chaos here targets
the *components*, not VMs. The chaos engine (only on `EXCLUSIVE` environments, so
one test's fault never disturbs another) offers actions such as:
- **Component kill/restart** — delete/restart pageserver, safekeeper, compute,
  or controller pods; assert recovery + no data loss (WAL replay, GetPage
  correctness after restart — the Rust integration framework already scans
  `pageserver.log` for corruption markers; we reuse that idea as an assertion).
- **Network partition / slowdown** — sever or delay compute↔pageserver,
  pageserver↔YB, safekeeper↔YB links.
- **Backing-store faults — the nemesis kills the underlying YugabyteDB nodes
  too**, not only yb-light components: kill/restart/throttle tservers and
  masters under load, simulate a zone outage, and verify how the storage layer
  tolerates its backend's faults.
- **LSN / lag injection, disk pressure, endpoint churn.**
Each action is a plugin; a **nemesis loop** (à la the stress `NemesisContext`)
runs a randomized sequence in the background during a workload. The sequence —
which actions, on which targets, at which moments — is derived from the test's
**random seed** (§4.3), so a failure triggered by one particular chaos ordering
can be replayed exactly instead of hoping the roulette lands there again.

---

## 6. Workloads & client apps

**Rule: workloads run in dedicated client pods, never on the framework host.** A
client pod is a long-lived runner the framework commands ("connect to endpoint X,
run workload Y with params Z, report progress"). This mirrors the stress
framework's client-host + sample-app model and the "ClientServer" idea.

Workload drivers (pluggable):
- **benchbase (TPC-C and friends)** — the perf backbone. A benchbase/TPC-C
  integration **already exists** in `benchmark/scripts/` (the `tpcc-run.sh` →
  `tpccbenchmark` path, `set_target` named-target registry). Wrap benchbase in a
  client-pod image and a driver; reuse its warehouse configs and load/execute
  phases. This is the recommended perf load source.
- **pgbench** — quick micro-latency / throughput.
- **pagebench** — the in-repo Rust pageserver load generator, for storage-layer
  perf.
- **Custom SQL / data loaders** — a small **Go workload runner** for functional
  and stress data shapes (schema setup + data load, random ops, DDL churn),
  keeping the client side single-language and easy to extend. (The legacy Neon
  `random_ops` and extension `regular-test.sh`/`test-upgrade.sh` suites are
  candidates to fold in as drivers.)
- **pg_regress** — run the stock Postgres regression suite against a live
  endpoint for correctness (the repo already does this locally via
  `test/integration/compute/pg_regress.sh`).
- **Stress sample apps** — the existing yb-stress-sample-apps fleet
  (`SqlDataLoad`, `CassandraKeyValue`, `SqlInserts`, and the other proven
  workloads from the stress framework), wrapped in a client-pod image and a
  driver. They give a battle-tested set of realistic load shapes for stress and
  soak runs without reinventing them, and the framework drives them the same way
  the stress framework does (start → wait-for-load → monitor → collect logs →
  report-kill).

The choice between reusing the Java stress sample apps and building a small
single-language Go workload runner for custom shapes is an open question (§11);
both can coexist behind the same driver interface.

---

## 7. Report portal

Deliberately a **simpler sibling** of `stress-report-service`, reusing its proven
shape. That portal is: Angular 18 + ECharts frontend · Python aiohttp API ·
**Go files service** (XZ/GZIP, large-log batch viewer, multi-region routing) ·
Victoria Metrics + Grafana for time-series · PostgreSQL 16 · a status-driven
daemon for Jenkins triggering.

**What we keep, what we simplify for the MVP:**
- **Keep** the Go files service almost verbatim — it's already generic: uploads
  land uncompressed for speed, a background worker pool compresses to XZ
  (~70–80% smaller), GET auto-decompresses, files fan out on disk by the first
  chars of the name, and reads are range/batched for the in-browser large-log
  viewer. Attachments form a tree via `/`-separated names (e.g.
  `pageserver/node-1/pageserver.log`). Multi-region routing (307 redirect to the
  owning region) and retention/cleanup workers come for free. Its
  *similarity-detection* (Jaccard shingling over crash dumps) and *log-spew
  top-lines* analysis are nice-to-haves we can enable later, not MVP. No reason
  to rebuild any of this.
- **Keep** Victoria Metrics for time-series (metrics labeled by result id).
- **Simplify** the API: a single **Go** service (matches the framework language;
  the framework can even share models) instead of the Python aiohttp + triage
  split.
- **MCP-first, not MCP-later — with full parity.** The rule is simple: **every
  UI and API surface has a corresponding MCP tool** — trigger (start/re-run a
  test or run), analyze (read a result with steps/logs/metrics, search files,
  history), and reporting (comment, triage, issue-link). If a human can do it in
  the portal, an agent can do it over MCP. (The stress portal grew its MCP
  server after the fact; here parity is part of the API contract.)
- **The framework binary is also the CLI — framework and support tool in one.**
  No separate triage binary: `ampqa` itself carries both roles. The same binary
  that executes tests locally also **starts remote runs** (trigger a CI run /
  point at a remote stack), **reads report data** (test overview, results with
  stack traces, step timelines, history across builds, file list/read/search,
  metrics), and **writes back** (comment, re-run, triage, issue-link) — the
  `yb stress …` command surface of `yb-cli`, but living inside the framework
  binary that already shares models with the portal. Conventions carried over
  from `yb-cli` for agentic use: a self-describing `manifest` command emitting
  the full command tree as JSON, a stable `--json` envelope on every command,
  bare IDs and full portal URLs interchangeable, meaningful exit codes. One
  binary to install, one command surface to learn — run a test, watch it, and
  triage its failure without switching tools. The MCP server exposes the same
  operations for MCP clients; terminal agents just use `ampqa` directly.
- **GitHub integration.** Failures link to GitHub issues (known-issue matching,
  the way the stress portal links known issues), and the portal/MCP can file or
  link an issue for a confirmed failure — closing the loop from red test to
  tracked bug. Jira can hang off the same hook if needed.
- **MVP web UI = three core views** (plus a fourth that can trail):
  1. **Runs / tests list** — every test result across **branches and builds**,
     filterable by branch, commit/build, category, status; grouped by run. The
     primary page.
  2. **Test detail** — steps timeline, logs (via files service), attachments,
     per-test metrics.
  3. **Performance tab** — benchmark trends across builds/branches with baseline
     comparison and regression flags.
  4. **Long-running / monitoring view** — live `PERSISTENT` environments and each
     one's rolling health-check history (green/red per hourly/daily check), so a
     multi-day env's status is visible at a glance. (Can trail the first three if
     MVP scope is tight, but the data model supports it from day one.)

### 7.1 Reporting API (what the framework calls)
Mirrors the stress reporting flow (create report → push results → attach files →
push metrics), streamlined:
- `POST /runs` — open a test_run (trigger type, branch, commit, build tag,
  triggered_by). Returns run id.
- `POST /runs/{id}/results` — open a test result (case, branch, commit, env ref).
- `POST /results/{id}/steps` / `PATCH …/steps/{sid}` — create/update steps live.
- `POST /results/{id}/attachments` — register a blob (uploaded to the files
  service; the row stores the files-service ref, kind, size, compression).
- `POST /results/{id}/metrics` — push a metric point / register a VM series
  pointer.
- `POST /results/{id}/perf` — push a perf datapoint (benchmark, metric, value,
  unit) — feeds the perf tab.
- `PATCH /results/{id}` / `PATCH /runs/{id}` — final status + summary.

### 7.2 Data model (Postgres) — proposed core tables
Adapted from the stress portal's `stress_tests / steps / metrics / attachments /
results` + `auto_runs / jobs`, generalized for branch/build comparison:

| Table | Key columns | Purpose |
|---|---|---|
| `test_run` | id, trigger_type (nightly/on_demand/on_push), branch, commit_sha, build_tag, image_tag, started_at, ended_at, status, triggered_by | one CI-triggered batch |
| `test_case` | id, name, suite, category (functional/stress/perf), owner, tags[] | the test catalog (stable identity across runs) |
| `test_result` | id, run_id→test_run, case_id→test_case, branch, commit_sha, status, started/ended, duration, **seed**, env_shape, env_ref, error_summary | one execution of a test; `seed` makes any run replayable |
| `step` | id, result_id→test_result, ordinal, name, status, started/ended, properties(jsonb) | timeline span (live progress) |
| `attachment` | id, result_id/step_id, name, kind (log/core/config/artifact), files_ref, size, compression | pointer to a blob in the files service |
| `metric_ref` | id, result_id, key, vm_series_selector | pointer to a Victoria Metrics series |
| `perf_result` | id, result_id, benchmark, metric_name, value, unit, baseline_value, delta_pct, is_regression | perf tab source; baseline resolved per branch |
| `env_lease` *(optional)* | id, result_id, shape, weight, acquired/released | resource accounting / debugging |
| `test_catalog` | id=hash(branch,suite,name), branch, suite, name, category, data(jsonb: doc, tags, options) | tests available **per branch** (ingested by the framework's `--collect`), so the UI knows what exists on each branch even before a run |
| `persistent_env` | id, name, case_id→test_case, branch, commit_sha, created_at, status (live/torn_down), lease_weight, data(jsonb: controller_url, namespace, connection, workload ref) | the **environment registry** for long-running Mode 2: detached envs a later `check` invocation attaches to |
| `health_check` | id, env_id→persistent_env, checked_at, status (ok/failed), summary, details(jsonb) | one periodic (hourly/daily) health assertion against a persistent env; feeds the monitoring view's rolling history |

Blobs → files service. Time-series → Victoria Metrics. Relational tables hold
structure, status, and pointers — same separation the stress portal uses, which
is what keeps Postgres small and the large-log viewer fast.

**Deliberate improvement over the stress portal:** that portal is *JSONB-centric*
— `branch`, `build`, `yb_version`, `report`, `page` all live inside a `config`
JSONB blob, so cross-branch/cross-build history is painful query-time JSONB
grouping (`get_test_history` groups `stress_tests` by `test_name` varying
`config->>'branch'`). Since **branch/build comparison is a primary use case
here**, we promote `branch`, `commit_sha`, `build_tag`, `category`, and `status`
to **real indexed columns** on `test_result`, and keep JSONB only for genuinely
free-form data (step properties, arbitrary config). This makes the runs list and
the perf trend queries simple and fast instead of JSONB-scan heavy.

**Test discovery:** the framework exposes a `--collect` mode (like the stress
framework's `run.py --collect`) that enumerates the tests present on a given
branch and ingests them into `test_catalog`, replacing that branch's rows
atomically. The portal uses this to render "what tests exist on branch X" and to
drive on-demand selection.

> **Where does the portal live?** In-repo under `test/portal/` for the MVP
> (single place, shared Go models). If Meko (or another product) joins, the
> **portal moves to its own repo** while **tests stay in each repo being
> tested** — the portal is product-agnostic by data model, so the split is a
> repo move, not a redesign. (Details §11.)

---

## 8. CI / orchestration flow

The framework is **executor-agnostic**: the same `ampqa` binary runs from a dev's
local machine, a Jenkins agent, or a GitHub Actions runner. CI's job is only to
trigger it and pass parameters — the framework owns parallelism — so the
orchestration below is deliberately thin. Two CI lanes, split by weight:

- **GitHub Actions — the commit lanes.** A **pre-commit (PR) pipeline**:
  build/lint/unit plus a functional slice on single-container environments
  (§5.3), and a **post-commit pipeline** on merge with a fuller set. For
  OneBox-scale parallelism the workflow spawns child GH Actions jobs; when a
  change needs the heavy tiers, GH Actions **invokes Jenkins** for that part.
  Runners are **provisioned, not free-tier** — free compute doesn't survive
  enterprise scale, so plan self-hosted/provisioned runners from the start.
- **Jenkins — the heavy lanes.** Nightly and **weekly** suites, on-demand runs,
  stress / perf / long-running — anything needing real k8s stacks, big quotas,
  and hours of wall-clock.

**PR testing is not fixed to the smoke slice — the test set is change-aware.**
A change can declare (or an **AI picks**) the set of tests it needs: most small
changes run a small subset (~10% of tests); a change touching a perf-sensitive
area pulls perf runs into its pipeline (routed to Jenkins); a feature branch
that accumulates dozens of commits runs a bigger set in one shot when it's
ready — saving cost by not running unnecessary tests, without capping what a
risky change can request. **Nightly and weekly runs** then catch whatever
slipped through and give early quality signal ahead of release deadlines.

### Build & artifacts — on GitHub
The build system lives with the code: **containerized builds in GitHub
Actions**, and **all artifacts live on GitHub** — images in the GitHub registry
(GHCR), binaries/bundles as GH release artifacts. This is the already-proven
pattern for the QA tooling (stress report service and itest report service
images, sample-apps jar + image all build and publish this way): code, CI,
artifacts, and images in one place, simpler than a separate Jenkins build farm.
Known trade-off, accepted: when GitHub is down the whole lane is down — it's
rare, and the third-party dependency chain already ties builds to GitHub being
up. For release-critical artifacts an S3 mirror can be added as belt-and-braces
later.

1. **Trigger** — pre-/post-commit (GitHub Actions) · nightly (Jenkins cron) ·
   on-demand (Jenkins param build, or a *Run* button in the portal → daemon →
   Jenkins).
2. **Orchestrator job (docker agent)** —
   (a) resolve/build the yb-amp image tag under test;
   (b) ensure a shared control plane is up for the SHARED batch, and plan
   namespaces/quota for the EXCLUSIVE tests;
   (c) `POST /runs` to the portal → run id.
3. **Run the framework** (one agent, or sharded across several) — roadmap
   invocation shape once the portal sink and CI plumbing land:
   `ampqa run --suite <regex> --tag <nightly|smoke|…> --parallel N --report <portal-url> --run-id <id> --branch <b> --commit <sha>`
   (`--report`/`--run-id`/`--branch`/`--commit` are not implemented today —
   see the real `ampqa run` flags in `QA_FRAMEWORK_IMPLEMENTATION.md` §4)
   - packing scheduler + lease manager (§4.4);
   - SHARED tests packed onto shared env(s); EXCLUSIVE tests each get their own;
   - each test: (lease/attach env) → provision project/stack → run → collect →
     report → free;
   - live steps/logs/metrics stream to the portal throughout.
4. **Finalize** — `PATCH /runs/{id}` status + summary; post a Slack message
   (pass/fail rollup + portal link — reuse the stress helper pattern).

Selection knobs mirror the stress framework: run **all** matching tests, capped
by `--parallel` (ambition) and the lease manager (reality). Nightly = the full
suite; pre-commit = a fast functional smoke tag on single-container envs;
on-demand = arbitrary regex/tag + params.

### 8.1 A separate scheduled pipeline for long-running health checks
**Roadmap — not built.** Distinct from the run pipelines above, a **cron
pipeline (hourly and/or daily)** would service long-running Mode 2 (§2). It
would not provision or run a normal suite: it would query the portal for
every `PERSISTENT` environment marked live, and for each one fire a
lightweight `ampqa check --env <id> --report <portal>` invocation that
attaches, asserts health, posts a `health_check` row, and exits. Today there
is no `ampqa check` command and no portal to query — the closest built
equivalent is `ampqa setup` (provision + leave standing, registered via
`ampqa env ls`) and `ampqa env rm <id>` (teardown-by-registry-entry); there is
no periodic re-attach/health-assert invocation yet.

### 8.2 Beyond qualification — the journey ends at prod, not at release
Qualifying a build is a gate, not the finish line. The same pipeline identity
(image tag + run results) carries into deployment: a build that passes its
required suites is promoted **dev → staging → canary → prod** (the managed
service is, at this level, a list of k8s clusters to roll). The portal records
which run qualified which build, so "what is running in prod and what tested
it" is one query. BYOC is explicitly out of scope for now; the promotion
machinery itself can live outside this framework — what the framework owns is
the qualification verdict and its traceability.

### 8.3 The agent loop
Because the framework is MCP-first (§1, §7), the commit → test → fix cycle does
not require a human in every seat: an agent (or a human) commits; the commit
lane runs; on failure an agent reads the result, steps, logs, and metrics
through MCP or the `ampqa` CLI, triages against known issues, proposes or lands
a fix, and re-runs — the same tools a human uses in the portal, consumable by
automation. CI stays the single pipeline of record either way.

---

## 9. How the categories differ operationally

| Aspect | Functional | Stress | Performance | Long-running |
|---|---|---|---|---|
| EnvSpec (§5) | `SHARED` (co-tenant, own project) | `EXCLUSIVE` (dedicated stack) | `EXCLUSIVE` or remote (attach) | `EXCLUSIVE` (Mode 1) / `PERSISTENT` (Mode 2) |
| Typical parallelism | high (packed onto few shared envs) | low (one env each, capacity-bounded) | low (isolation matters) | very low (few long-lived envs) |
| Duration | seconds–minutes | minutes–hours (soak) | minutes; repeated for stability | hours–days |
| Chaos | none | yes (nemesis loop) | none (clean signal) | optional (nemesis in soak) |
| Workload | small SQL / API drives | sustained load + faults | benchbase/pgbench, measured | sustained/background load |
| Key assertions | correctness, API contract | recovery, no data loss, no corruption | latency p50/p95, tps, cold-start, **regression vs baseline** | still healthy over time: no leak/corruption, GC/compaction progressing, data consistent |
| Invocation model | one run | one run | one run | one long run (Mode 1) / `provision`+scheduled `check`+`teardown` (Mode 2) |
| Portal view | list + detail | list + detail (+ long timeline) | **perf tab** (trend + regression) | **monitoring view** (rolling health) |

**Performance specifics worth calling out** (serverless makes these
first-class): **cold-start time** (endpoint create → first successful query),
**branch-create time**, **GetPage latency**, **WAL ingest lag**, **LFC
hit-rate** — beyond raw query latency/tps. Baselines are stored per branch;
a run flags `is_regression` when a KPI crosses a configured threshold vs the
branch baseline (or vs master). Historical comparison across builds is the perf
tab's whole reason to exist.

---

## 10. Requirements

### Functional requirements
- Register/select tests by suite/name regex + tags; three categories.
- Atomic architecture: reusable **Apps** (workloads, chaos actions, clients,
  collectors) usable in any context, and **Tasks** — runnable non-test units
  built from the same primitives (`ampqa task <name>`: provision, load data,
  run a workload, collect); tests = tasks + assertions.
- Optional lifecycle stages: attach to a standing env (`--env`) and skip setup
  (`--skip-setup`) to re-run tests against one expensive env/dataset many times
  with config changes, without redeploy (e.g. TPC-C: 8-hour load once,
  30-minute execute re-runs).
- MCP parity: every UI/API surface has a corresponding MCP tool — trigger,
  analyze, and reporting.
- Per-test `EnvSpec` (sharing policy + shape): `SHARED` co-tenant vs `EXCLUSIVE`
  dedicated, plus single-container (docker), remote-attach, and **Universe** (a
  YugabyteDB cluster as the target) — behind one Env Provider interface.
- YugabyteDB cluster provisioning as atomic apps: a **YBA app** (universe
  create/manage, the stress `yugaware_app` equivalent) and a **yugabyted app**
  (cluster start/stop over SSH/pods), reusable as yb-light's backing store and
  as a standalone universe target (§5.4a).
- Named-secret system (§4.6a): token / file / env-var variants, set from config
  or `-o`, resolved by name, masked in logs — feeding artifact fetchers, cloud
  apps, and YBA provider setup.
- Infrastructure hygiene (§4.6b): reuse the shared `yb-dev-aws-2` keypair for
  YBA providers (never auto-mint AWS keys), tag + reap anything created, no
  orphan cloud resources.
- Executor-agnostic framework: runnable from a local machine, Jenkins, or GitHub
  Actions (provisioned runners) with identical behavior.
- Stress-style configuration: layered config files (deep-merge + include) +
  typed `-o key=value` overrides with dotted nesting; one mechanism steers
  everything; resolved config recorded per result (§4.6).
- Declarative artifacts: named, versioned things (apps, utilities, datasets)
  from GH releases / S3 / https / local, with per-artifact delivery flags
  (on-host download, fetch-once-and-copy, unpack, target scope) and a local
  cache (§4.7).
- Cloud & k8s access (AWS/GCP APIs, kubeconfig — no Azure) with **ambient
  credentials by default** — an authed `aws`/`gcloud`/`kubectl` machine
  needs zero extra credential parameters; explicit keys via config when needed
  (§4.8).
- Version compatibility: one framework for any product/YB version — live
  version detection, declarative per-test version bounds with auto-skip,
  feature gates in tests, version-branching inside apps; no per-version
  framework branches (§4.9).
- MCP server over the framework + portal capabilities (list/read/re-run/triage/
  issue-link) — agents are first-class users.
- Framework-as-CLI: the `ampqa` binary is also the support tool — start remote
  runs, read report data (results/steps/files/metrics/history), and write back
  (comment/re-run/triage), with yb-cli conventions (JSON envelope,
  self-describing manifest, URL-or-ID args) for terminal agents and humans.
- GitHub issues integration: failure ↔ issue linkage, known-issue matching,
  file/link from the portal or MCP.
- Packing scheduler: `SHARED` tests packed onto shared environments, `EXCLUSIVE`
  tests each get their own — both concurrent in one run.
- Bounded, self-throttling parallelism with a per-environment resource lease
  manager (blocks for freed capacity, never over-commits, never drops a test).
- Atomic tests: acquire → run → collect → teardown → release, teardown always.
- Random seed per test execution (auto-generated, pinnable via `--seed`),
  driving all randomness (workloads, nemesis sequence/timing/targets); recorded
  in the result so chaos-caused random failures are replayable.
- Chaos engine with a pluggable action set and a background nemesis loop.
- Workload drivers incl. benchbase and pgbench, run in client pods.
- Stream steps/logs/metrics live to the portal; attach cores/configs/logs.
- Perf results with baseline comparison + regression flagging.
- **Long-running support:** long soak (single invocation) *and* detached
  `PERSISTENT` environments with a pinned lease, an environment registry, a
  `provision`/`check`/`teardown` entrypoint split, and scheduled health-check
  reporting.
- CI triggers: GitHub Actions pre-/post-commit lanes, Jenkins nightly, weekly,
  and on-demand, **plus a scheduled long-running health-check cron**; Slack
  summary.
- Change-aware test selection: a change declares (or AI picks) its test set —
  small changes run a small subset, perf-sensitive changes pull perf runs in,
  feature branches batch a bigger one-shot run.
- Build & artifacts on GitHub: containerized GH Actions builds; images in GHCR,
  binaries as GH release artifacts.
- Portal MVP: runs/tests list (cross-branch/build), test detail, perf tab, and a
  long-running/monitoring view (last may trail MVP).

### Non-functional requirements
- **Framework stays I/O-bound** — no workload, no DB, no large-file processing on
  the orchestrator; hard rule (§1). Prevents the agent-OOM class of failures.
- **Cluster ceiling enforced** by k8s `ResourceQuota`; the lease manager never
  over-commits.
- **Idempotent teardown** — no orphan projects/namespaces/pods; a reaper sweeps
  leaked envs by label/age (learn from the stress LRU-leak experience).
- **Observability** — every test result carries component logs + a metric
  snapshot; the files service handles arbitrarily large logs via batch reads.
- **Extensibility** — new provider/workload/chaos/reporter without touching
  existing tests.
- **Single language where it counts** — Go for framework + portal API + files
  service, so devs work in one stack and models can be shared.

---

## 11. Decisions and open questions

### Decisions made
- **Environment sharing is a per-test property, not a global default.** `SHARED`
  tests co-tenant on shared control plane(s) (own project each); `EXCLUSIVE`
  tests each get their own stack; both run concurrently in one run (§4.4, §5).
  Sub-knob to tune later: `per_env_parallelism` (co-tenant tests per shared env).
- **The framework owns parallelism** via a packing scheduler + lease manager
  (§4.4). Go handles any real test count because the lease manager, not
  goroutines, is the throttle (§4.4.1). Jenkins child-task sharding is a fallback
  lever for fault isolation / long-run durability / multi-agent scale, not a
  day-one requirement (§4.4.2).

### Open questions
- **Portal location** — in-repo `test/portal/` for the MVP (single place,
  shared Go models); agreed direction: if Meko joins, the portal moves to a
  separate repo and **tests stay in each repo being tested**. What remains open
  is only the timing of the split.
- **Client workloads** — reuse the Java yb-stress-sample-apps, or build a small
  Go workload runner + benchbase. Leaning toward the Go runner + benchbase for a
  single-language client surface.
- **Unit-test reporting** — should `cargo test` / `go test` results also publish
  into this portal (one dashboard), or stay in GitHub Actions only? yb-amp's
  Actions are currently mostly disabled, so there is room to define this.
- **Pre-commit scope** — how large a functional smoke subset is acceptable on
  PRs given environment cost (single-container envs make this much cheaper, but
  the budget still needs a number).
- **One system with Meko?** Meko needs integration-test automation (it has eval
  frameworks, not an e2e framework). Whether Meko adopts this framework + portal
  wholesale, or only the portal + MCP conventions, is open. The design keeps
  product specifics behind Env Providers and workload drivers, so a second
  product is a new provider, not a fork — the door stays open either way.
- **How far to merge the quality systems** — the goal is one system feature
  owners learn once (vs ybdb's phabricator/jenkins/taqo/itest/stress/perf/release
  spread), but where exactly release qualification and deployment promotion
  (§8.2) live — inside this framework or as thin consumers of its verdicts —
  needs a call.

---

## 12. Suggested phasing

- **Phase 0 — skeleton.** Go module `test/framework`, core interfaces (Test/Suite/
  Step/Env/Reporter), controller-client wrapper, the single-container docker
  provider (cheapest env, local + CI), a stdout reporter, 2–3 `SHARED` co-tenant
  functional tests on a shared control plane. Prove per-project isolation +
  teardown.
- **Phase 1 — parallelism + portal MVP.** Worker pool + lease manager; portal
  Go API (designed MCP-first — the MCP server ships with it, and `ampqa` grows
  its portal-read/write subcommands) + Postgres + reused files service;
  runs/tests list + test detail. Wire the portal reporter.
- **Phase 2 — CI.** GitHub Actions pre-/post-commit lanes on provisioned runners
  (functional smoke on single-container envs) + Jenkins nightly and on-demand;
  Slack summary; reaper for leaked envs.
- **Phase 3 — stress + long-running.** `EXCLUSIVE` dedicated stacks, chaos
  engine + nemesis loop, long soak (Mode 1); then the `PERSISTENT` env registry +
  `provision`/`check`/`teardown` entrypoints + the scheduled health-check cron +
  monitoring view (Mode 2).
- **Phase 4 — performance + integration depth.** benchbase/pgbench drivers,
  Victoria Metrics wiring, perf tab with baselines + regression flags; GitHub
  issues auto-linking; qualification → promotion traceability (§8.2).

---

### Appendix — reference points reused
- **Trigger/fan-out model & Slack rollup:** `yb-stress-test` Jenkins pipelines
  (`pipeline_on_demand_nightly.groovy`, `pipeline_suite_multi_portal.groovy`) and
  the stress portal's daemon (`RUN_TRIGGER_FLOW.md`).
- **Portal shape (files service, metrics, tables, large-log viewer, similarity
  detection):** `qa-services/stress-report-service` (`README.md`, `CLAUDE.md`,
  `scripts/db/*.sql`).
- **Env-creation & control-plane API:** yb-amp `controller/` (OpenAPI spec +
  oapi-codegen client), `deploy/up.sh` + Helm charts, `deploy/smoke-test.sh`,
  `control_plane/` `yb_light_local`, `test/integration/`.
- **Perf/benchbase:** yb-amp `benchmark/scripts/` (`tpcc-run.sh`, `_env.sh`,
  `_metrics.sh`), `pageserver/pagebench`.
- **Test taxonomy & quality dimensions:** YugabyteDB `coverage_process`.
