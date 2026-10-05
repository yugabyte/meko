# mekoqa: the meko system-test framework

`mekoqa` deploys a complete meko stack on Kubernetes, drives it the way a customer does
(REST, MCP, the inference gateway), injects faults, loads it, and reports every result to the
meko report portal at `https://meko-qa.dev.yugabyte.com`. It is the meko copy of amp's `ampqa`
(`amp/test/functional/`). The engine is the same, and the apps, suites and deploy path are
meko's own.

It is a standalone Go module. It imports no product code and reaches meko only over the wire.

## Documentation

| Read | For |
|---|---|
| [`docs/RUNNING.md`](docs/RUNNING.md) | Running tests locally, from the portal and through Jenkins. Config overrides, images, keeping environments |
| [`docs/WRITING_TESTS.md`](docs/WRITING_TESTS.md) | Adding a test. Registration, `EnvSpec`, helpers, the mocks, a worked example |
| [`docs/TRIAGE.md`](docs/TRIAGE.md) | Reading a failed run on the portal and deciding product bug or test bug |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How a run works. Scheduler, stacks, the guard, diagnostics, reaping, reporting |
| [`framework/AGENTS.md`](framework/AGENTS.md) | The rules every change must keep |
| [`framework/configs/default.yaml`](framework/configs/default.yaml) | Every config key and its default, documented |
| [`PLAN.md`](PLAN.md) | The 2026-10-02 plan and the reasoning behind the design. History, not a description of today's behavior |

## Layout

```
framework/            the engine and everything it talks to
  core/ sched/ ...    engine: tests, environments, scheduler, guard, config, reporting
  env/k8s/            builds stacks on a cluster
  apps/               one package per system the framework drives (mekostack, mekoapi, mcpclient,
                      llmmock, cognitomock, mekoload, minio, database providers)
  universe/           provider-neutral YugabyteDB universe spec
  diag/               failure diagnostics
  configs/default.yaml
  cmd/mekoqa/         the CLI
suites/               the tests
  functional/ stress/ perf/   the three suites
  canary/                     the engine's self-tests, run by go test only
  internal/stacktest/ helpers every suite uses
```

Full tree in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md#layout).

## Quick start

From `meko/test/functional`, with Go installed:

```bash
go run ./framework/cmd/mekoqa collect                       # list every test
go run ./framework/cmd/mekoqa describe functional/stack_health   # one test and its config keys
go run ./framework/cmd/mekoqa run --tag regression --fake-env --portal ""   # no cluster, every test skips
```

A real run builds a stack in new `mekoqa-*` namespaces on your current kube context. It needs a
cluster, `kubectl`, `helm` and a Quay credential:

```bash
export MEKOQA_QUAY='<user>:<token>'
go run ./framework/cmd/mekoqa run --test functional/stack_health \
  --kube-context gke_yugabyte_us-west1_amp-dev \
  -o secrets.quay=env:MEKOQA_QUAY --portal ""
```

A fresh stack takes about 20 minutes to build. `--portal ""` keeps the run off the portal.

## Where to run

| Where | How |
|---|---|
| Your machine, against amp-dev | `go run ./framework/cmd/mekoqa run ...`. See [RUNNING.md](docs/RUNNING.md#1-locally-against-amp-dev) |
| The portal | The Run tests dialog, or `yb meko run trigger --tag regression --yes`. See [RUNNING.md](docs/RUNNING.md#2-from-the-portal) |
| Jenkins | `users/shubin/meko-devops/MEKO/meko-run-tests`. See [RUNNING.md](docs/RUNNING.md#3-through-jenkins) |

The suites:

| Suite | Tag | Tests | Stack |
|---|---|---|---|
| `functional` | `regression` | REST, MCP, auth, sharing, PATs, KB ingest, chat, database topology, Langfuse outage, upgrade from the previous release, universe backup and restore, read replica, xCluster | shared |
| `stress` | `stress` | datapack and MCP soaks, churn with api-server pod kills | dedicated each |
| `perf` | `perf` | REST and MCP latency ladders | dedicated each |

Each tag is what the matching card on the portal's Status page runs, and the card it reports to.

## Conventions

- **Product bugs are reported as GitHub issues and never worked around in a test.** A failing
  test stays red until the product is fixed. Harness bugs get fixed.
- **LLMs are always mocked.** A stack runs `llmmock`, so a run makes no call to OpenAI or
  Bedrock and needs no key.
- **Images come only from Quay, built by `meko-build`.** The framework refuses a `meko-*`
  image from anywhere else.
- **Touch only what mekoqa created.** Namespaces carry the labels `mekoqa=1` and
  `mekoqa-test=true`. Never edit or delete another person's namespace.
- **Every default lives in `default.yaml`.** One config mechanism: the embedded defaults,
  then `-c` files, then `-jc` JSON, then `-o key=value`.
- **Honest status.** `passed`, `failed`, `timed_out`, `broken`, `env_failed` and `skipped` say
  where the failure happened. `env_failed` and `skipped` are never a pass.

## Before you push

```bash
make build vet fmt-check test       # go build, go vet, gofmt -l, go test -race
go test -race ./suites/canary       # the engine self-test, asserts every status
```

Unit tests use in-process doubles and run no cluster. Run changes on a real stack before you
merge them, and see [`docs/WRITING_TESTS.md`](docs/WRITING_TESTS.md#checking-a-test-before-you-push).
