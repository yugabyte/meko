---
name: write-amp-tests
description: >-
  Plan and write functional, system, regression, stress or perf tests for amp
  with the ampqa framework in `amp/test/functional`. Use when asked to write,
  add or extend tests, to cover an amp feature, change or bug with tests, or to
  "write a test for X". Forces a coverage analysis (concurrency, controller and
  pod restarts, upgrades with mixed versions, pageserver, safekeeper and
  backing outages, quotas, bad input, isolation, durability, soak) before any
  code, so the tests match what customers hit and not only the one path named
  in the request.
---

# Writing amp tests

A request like "write tests for feature X" names one path. A customer does not use one
path. Your job is to cover the feature and what surrounds it, from several angles, and to
push back when the request is narrower than the risk.

Test count is not a measure. Ten happy-path tests cover less than three that reproduce
what really happens on the customer side: a compute pod dies mid-write, two API calls
race on one endpoint, the controller restarts during a task, an upgrade runs with old and
new builds side by side.

## Read first

Read these before you write a line. They own the rules. This skill owns the way of
thinking.

- `amp/test/functional/framework/AGENTS.md`, the mandatory framework principles
- `amp/test/functional/framework/README.md`, mental model, providers, CLI, status taxonomy
- `amp/test/functional/framework/docs/testing-tiers.md`, what each tier is and what it holds
- `amp/test/functional/framework/docs/invariants.md`, which tests guard which guarantee, and the thin cells
- `amp/docs/qa-framework/architecture.md`, the design, including how a known product bug is handled
- `amp/AGENTS.md`, and the architecture docs under `amp/*/architecture/` for the feature itself

Run `ampqa collect` and `ampqa describe` and read the tests that already touch the
feature. Extend or complement them. Do not duplicate them. The README says never to
assume a test, suite or flag exists, so check. In this skill `ampqa` means
`go run ./framework/cmd/ampqa`, run from `amp/test/functional`.

## Rules that do not bend

- A product bug is reported and never worked around. A broken feature keeps running and
  keeps failing, and a skipped test hides it. File the GitHub issue (see the
  `create-amp-issue` skill) and attach it as a known issue on the portal. Do not add a
  retry, sleep, looser assertion or `Skip` to get green.
- `Skip` is for an environment the test cannot run on, or a surface that does not exist
  yet. Do not write a test against a route or field that is not built.
- A harness bug is fixed in the test or the framework.
- Assert the outcome and the side effects. A task reaching `completed` or a `200` is not
  enough. Check what the client sees, the SQL result, the persisted spec, the pod
  that exists or is gone, and that no orphan resource is left.
- Tests and the shared stack are parallel by design. Name what you create with
  `t.ResourceSuffix()` and never touch infrastructure the framework did not create.

## Step 1: understand the feature

Read the controller code, the API spec, the PR or issue and the architecture docs for the
component. Write down the API surface, the task steps it runs, what it persists, which
components it touches (controller, endpoint compute, pageserver, safekeeper, the YugabyteDB
backing) and what must survive a restart.

## Step 2: list scenarios across the angles

For each angle write the scenario, or write "not applicable, because". Check
`invariants.md` first. A row with an empty fault cell is a gap you can fill.

| Angle | Ask |
|---|---|
| Concurrency and races | two callers on one object, create against delete, resize against delete, a task and its cancel, the same request twice at once |
| Restarts | delete the compute pod, kill the process, restart the controller mid-task. Is the task resumed or does it end in a terminal status? |
| Rollouts and upgrades | old and new controller or endpoint builds side by side. Data an older build wrote read by the new one. See `amp_upgrade`, `amp_mixed_versions`, `amp_format_compat` |
| Dependency outage or slowness | pageserver, safekeeper, controller, the YB backing: killed, frozen, partitioned, slowed, disk full. Chaos actions live in `framework/chaos` |
| Limits and quotas | namespace quota, unschedulable requests, memory floor, resource ceilings. Does the task fail naming the cause, and does it clean up? |
| Invalid and boundary input | bad names, units, empty and huge values, ids from another tenant. A synchronous rejection with a clear message |
| Idempotency and retries | the same create twice, a retry after a timeout, delete of a deleted object, the retryable error class (57/08) |
| Cleanup and leaks | after a failed, cancelled or raced task, no pod, volume, row or namespace is left |
| Data surviving restart or upgrade | write, restart or upgrade, read back with a checksum. An acknowledged commit stays durable |
| Multi-tenant isolation | does another tenant's branch or endpoint see the rows, the settings or the password |
| Scale over time | many branches, endpoints or connections, long runs, drift in latency or memory. This is `stress` or `perf` |
| What the user sees in the error | a retryable error or a clear 4xx with a reason. Never a 500, a hang or a bare `deadline exceeded` |

Add angles that are specific to the feature. Ask how a real customer hits it.

## Step 3: choose tests and place them

Read `testing-tiers.md` and trust `ampqa collect --tag <tier>` over any count in a doc.
The suites under `suites/`:

| Suite | Use for |
|---|---|
| `features` | functional tests. The `regression` tag is the fast PR gate. `Category` is `core.Functional`, `Env` is usually `Exclusive` plus `SharedControlPlane` |
| `stress` | sustained load, nemesis and durability scenarios, each on a `DedicatedStack` |
| `perf` | benchmarks and measured-number tests gated against a baseline or portal history |
| `canary`, `presets` | engine self-test and setup menus. Not for product tests |

There are three tier tags, `regression`, `stress` and `perf`, and the docs say not to add
others. Some slow `features` tests carry `stress` for their runtime, and `scale` tests in
`suites/perf` run only on demand.

A test that faults the shared control plane sets `FaultsSharedInfra: true`. One that needs
a stack of its own uses `DedicatedStack`, which costs a whole YB and control plane, so earn
it. The `Test` fields are in `framework/core/test.go`. `OwningTeam` is one of
`TeamControlPlane`, `TeamCompute`, `TeamVoyager` or `TeamPerfAdvisor`. Amp has no
`Tolerates` field.

Fold scenarios that share an expensive setup into one test with named steps. Split ones that
fail for different reasons. Say what you dropped and why.

## Step 4: write steps and assertions

Copy the shape of a neighbouring test. A functional test registers in `init()` with
`core.Register`, calls `runAmpFeature` and hands it phases (see
`suites/features/amp_endpoint_resize_delete_race.go`). Use the `apps/controller` client for
API calls, `apps/pg` for SQL and `framework/chaos` actions for faults. Files that register tests
keep plain names. Support code ends in `_helper.go`. Add a client method in `apps/controller`
when a route is missing, generated from the OpenAPI spec as the principles require.

Wait on conditions with `t.WaitFor`, bound every wait, and put the expected and the observed
value in each failure. Assert a synchronous rejection with the non-waiting client call so a
bug fails fast and by name.

## Step 5: run it for real

`--fake-env` makes every regression test skip. Run on a real cluster. With a kube context
and no `-c` file, `ampqa run --test features/<name>` builds its own control plane, runs and
tears it down. Config needs, image registry and pull secrets are in `framework/README.md`
and `framework/configs/default.yaml`. For the portal, use the `amp-report-service` skill.
A portal run needs an image and a backing, so a bare `--branch` is not enough.

- Run the test alone and with the neighbours it shares a control plane with.
- Replay a failure with `--seed <N>`.
- Break the thing the test guards once, on purpose, to prove it can fail.
- Exit code 4 means all selected tests skipped. That is not a pass.
- After a live run, leak-check with `ampqa env ls`. Remove with `env rm` or `env reap`, never
  with manual `kubectl delete`.

Before you push, from `amp/test/functional`: `go build ./... && go vet ./... && gofmt -l . && go test -race ./...`.

## Step 6: triage

Status says where the failure happened. `failed` and `timed_out` are product signals.
`broken` is a harness bug to fix. `env_failed` and `skipped` mean the test never got a fair
run, so never read them as a pass. Check history on the portal before blaming the product.
A product bug gets an issue and a known-issue attachment, and the test stays red. A test bug
gets fixed and rerun.

## Worked example

Request: "add a test for endpoint resize".

The `features` suite already has `amp_endpoint_resize`, `_in_place`, `_memory_down`,
`_infeasible`, `_lock` and `_delete_race`. Check `invariants.md` and `ampqa collect`, then
plan around the gaps.

| Angle | Scenario | Where |
|---|---|---|
| Restarts | the compute pod is deleted or the controller restarts while a resize task runs. The task ends terminal, the persisted size matches what was asked or what it was before, and SQL works | `features`, `FaultsSharedInfra`, check `stress/control_plane_nemesis` for overlap first |
| Durability | writes acknowledged before and during the resize are all present after it, checked by checksum | same test, extra step |
| Upgrade | resize an endpoint during an endpoint upgrade roll, and one created by the previous build | `features`, beside `amp_endpoint_upgrade_roll` |
| Dependency | resize while the backing or pageserver is degraded. Clear retryable error, no orphan pod | `stress`, dedicated stack |
| Limits | resize past the namespace quota or onto no schedulable node. The task names the cause, the old size keeps serving | extend `amp_endpoint_resize_infeasible`, no new test |
| Input | zero, negative, wrong units, memory under the per-vCPU floor, an endpoint of another tenant. Synchronous 4xx with a reason | extend `amp_api_validation` |
| Idempotency | the same resize twice, a resize to the current size | `features`, short |
| Isolation | resizing tenant A's endpoint leaves tenant B's endpoint pod and connections untouched | `features` |
| Scale | resize many endpoints at once, latency stays within the `ops_latency` gate | `perf`, only if the product changed that path |

The plan is six or seven tests and steps. Several fold into existing files. State what you
chose not to cover.

## Done means

- Every angle has a scenario or a stated reason it does not apply.
- Each test ran on a real cluster and passed, or is red with an issue attached.
- Assertions cover the outcome and the side effects, not only a status code.
- The run left no `ampqa-*` namespace behind.
