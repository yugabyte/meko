---
name: write-meko-tests
description: >-
  Plan and write functional, system, regression, stress or perf tests for meko
  with the mekoqa framework in `meko/test/functional`. Use when asked to write,
  add or extend tests, to cover a meko feature, change or bug with tests, or to
  "write a test for X". Forces a coverage analysis (roles, concurrency,
  restarts, upgrades, dependency outages, limits, bad input, isolation, soak)
  before any code, so the tests match what customers hit and not only the one
  path named in the request.
---

# Writing meko tests

A request like "write tests for feature X" names one path. A customer does not use one
path. Your job is to cover the feature and what surrounds it, from several angles, and to
push back when the request is narrower than the risk.

Test count is not a measure. Ten happy-path tests cover less than three that reproduce
what really happens on the customer side: a pod dies mid-request, two users race, an
upgrade lands with old and new builds running together.

## Read first

Read these before you write a line. They own the rules. This skill owns the way of
thinking.

- `meko/test/functional/framework/AGENTS.md`, the mandatory framework principles
- `meko/test/functional/docs/WRITING_TESTS.md`, the API, `EnvSpec`, helpers and a worked test
- `meko/test/functional/docs/ARCHITECTURE.md`, how stacks and the scheduler work
- `meko/test/functional/docs/RUNNING.md`, running locally, on the portal and on Jenkins
- `meko/test/functional/docs/TRIAGE.md`, product bug or test bug

Then run `mekoqa collect` and read the tests that already touch the feature. Extend or
complement them. Do not duplicate them. In this skill `mekoqa` means
`go run ./framework/cmd/mekoqa`, run from `meko/test/functional`.

## Rules that do not bend

- A product bug is reported and never worked around. A failing test stays red. No retry,
  sleep, looser assertion or `Skip` to get green. File the issue and attach it on the portal.
- A harness bug is fixed in the test or the framework. No issue.
- Assert what the user sees and what changed around it. A `200` alone proves little. Check
  the body, the error code and message a user would read, the row in the next list call,
  the MCP tool result, the LLM mock calls (`llm.Calls`), the pod state and the cleanup.
- Never decide a verdict from a clock. Use `t.WaitFor` with a condition and a bound.
- LLMs are always mocked. Touch only resources you created, named with `t.ResourceSuffix()`.

## Step 1: understand the feature

Read the handler code, the PR or issue, and the docs under `meko/docs/content/docs/**`.
Write down who calls it (REST, MCP, UI, agent), what it stores, which services it calls
(Cognito, Langfuse, the LLM, the database) and what state outlives a restart.

## Step 2: list scenarios across the angles

Go through every angle. For each one, write the scenario or write "not applicable,
because". Do not skip an angle silently.

| Angle | Ask |
|---|---|
| Roles and permissions | owner, viewer, a stranger, an anonymous caller, a PAT with each scope, a revoked or expired token. What does each get? Read the handler for the roles that exist |
| Concurrency and races | two creators, create against delete, share against revoke, the same request twice at once |
| Restarts and rollouts | kill the api-server, MCP server or gateway pod mid-request. Does a rollout drop requests? |
| Upgrade | old and new builds side by side, and data the old build wrote read by the new one. `functional/upgrade_previous_release` is the pattern |
| Dependency outage or slowness | Langfuse, the LLM (mock faults), Cognito, the database. Frozen, slow, erroring. See `functional/langfuse_outage` and `functional/llm_fault_surfaces` |
| Limits and quotas | free tier, maximum sizes, rate limits, a 429 with `Retry-After` |
| Invalid and boundary input | empty, huge, unicode, wrong type, duplicate names, ids that belong to someone else |
| Idempotency and retries | the same create twice, a retry after a timeout, a delete of something already deleted |
| Cleanup and leaks | after a delete, are the Langfuse project, the row, the token and the pod state gone? |
| Data surviving restart or upgrade | write, restart the component, read |
| Multi-tenant isolation | can account B see, guess or touch account A's datapack, token, conversation or error text? |
| Scale over time | many objects, long runs, memory growth, error rate drift. This is `stress` |
| What the user sees in the error | a clear code and message, never an internal endpoint (MEKO-812), never a stack trace, never a bare timeout |

Add the angles that are specific to the feature. Ask how a real customer hits it, not how
the developer tested it.

## Step 3: choose tests and place them

Meko has exactly three suites. There is no fourth.

| Suite | Use for | Tags |
|---|---|---|
| `functional` | behaviour on a shared stack, faults, upgrades. Every test carries `regression` | `tags()` gives `regression` and `nightly`. `tags("upgrade")` adds an area |
| `stress` | long load on a dedicated stack | `stress` |
| `perf` | latency and throughput datapoints with baselines | `perf` |

Fold scenarios that share setup into one test with several steps. Split the ones that can
fail for different reasons, so a known issue on one does not hide the other. A scenario
that kills or freezes a component sets `FaultsSharedInfra: true` and lists the guard kinds
it provokes in `Tolerates`. A scenario that needs a stack nobody else touches uses
`DedicatedStack`. That costs about 20 minutes of provisioning, so earn it.

Drop scenarios that duplicate an existing test, that target a route which does not exist
(there is no token refresh route),
or that only restate the framework's own guarantees. Say what you dropped and why.

## Step 4: write steps and assertions

Follow `WRITING_TESTS.md`. Use `stacktest.API`, `stacktest.Accounts`, `stacktest.LLM` and
the other helpers listed there. If a route has no client method, add it to `mekoapi`,
read off `router.go`. Structure the body as named steps, clean up with
`stacktest.CleanupCtx`, and put the expected and the observed value in every failure
message.

## Step 5: run it for real

`--fake-env` only proves a test registers and skips. Run on a real stack as `RUNNING.md`
describes. You need a kube context, `MEKOQA_QUAY`, `-o env.mekostack.auth=cognito_mock`
for anything with accounts, and `--portal ""` for a local debug run. Without the Cognito
mock the account tests skip, so check the skip count.

Run the test alone and at `--parallel 4`. Use `--env-mode keep-failed` to look at a failed
stack only when the user asked, and `mekoqa env rm <id>` when you are done. Break the thing
the test guards once, on purpose, to prove the test can see the bug.

Before you push, from `meko/test/functional`:

```bash
go build ./... && go vet ./... && gofmt -l . && go test -race ./...
```

Adding a test to a suite means updating the registered count asserted in
`suites/functional/functional_test.go` (or the matching test of the suite you used).

## Step 6: triage

Follow `TRIAGE.md` and the `meko-report-service` skill. Decide product or test from
evidence. A product bug gets a GitHub issue on `yugabyte/meko`, attached on the portal,
and the test stays red. A test bug gets fixed and rerun. Report the run id, the seed and
the image tag.

## Worked example

Request: "add a test for datapack sharing".

`functional/datapack_sharing` already covers one path, an owner sharing with a viewer. A
test that repeats it adds nothing. A coverage plan looks like this.

| Angle | Scenario | Where |
|---|---|---|
| Roles | a viewer cannot write or delete. A stranger learns nothing about whether the datapack exists | `functional`, one test with a step per role |
| Roles | the owner revokes a share. The ex-viewer loses MCP access with the PAT they already hold | same test |
| Concurrency | the owner deletes the datapack while B accepts the invite. B accepts twice at once | `functional`, own test |
| Isolation | account C guesses the datapack id over REST and MCP and sees nothing. The error text names no internal endpoint | `functional` |
| Input | share with yourself, with an unknown user, twice, with an invalid role | `functional` |
| Dependency | the database or Cognito is slow during an accept. The user gets a clear error and no half-made share | `functional` with `FaultsSharedInfra` |
| Restart | kill the api-server after an accept. The share is still there and the next list agrees | `functional` with `Tolerates` |
| Upgrade | a share made on the previous release still resolves after the upgrade | add steps to `upgrade_previous_release`, no new test |
| Cleanup | deleting the datapack removes its invites and the incoming-share entries | `functional` |
| Scale | hundreds of shares on one account, pagination stays correct | `stress`, only if the list path changed |

Seven tests worth running beat thirty near-identical ones. When the request said only
"sharing works", tell the requester what you added, what you left out and why.

## Done means

- Every angle has a scenario or a stated reason it does not apply.
- Each test ran on a real stack and passed, or is red with an issue attached.
- Assertions cover the outcome and the side effects, not only the status.
- Clean-up is verified. No `mekoqa-*` namespace of yours is left behind.
