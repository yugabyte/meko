# mekoqa architecture

How a mekoqa run works, from `mekoqa run` to a result on the portal. For running tests see
[`RUNNING.md`](RUNNING.md), for writing one [`WRITING_TESTS.md`](WRITING_TESTS.md), for
reading a failed run [`TRIAGE.md`](TRIAGE.md). The rules every change must keep are in
[`../framework/AGENTS.md`](../framework/AGENTS.md).

## Layout

```
meko/test/functional/
  go.mod                 module github.com/yugabyte/meko/meko/test/functional
  framework/
    core/                the engine: Test, EnvSpec, T, steps, metrics, lifecycle, guard, config, secrets
    sched/               the packing scheduler and the cluster-slot cap
    exec/ chaos/ task/   kubectl and ssh transports, fault actions, one-off tasks
    report/              sinks: portal, jsonfile, junit, html, stdout
    diag/                failure diagnostics collected before teardown
    env/k8s/             the environment provider that builds stacks on a cluster
    env/reap/            the serializable reap plan
    env/registry/        the file registry of kept environments
    env/fake/            a provider with no cluster, for the canary self-tests
    apps/                one package per thing the framework talks to (see below)
    universe/            provider-neutral YugabyteDB universe spec and capabilities
    configs/default.yaml every default, documented
    cmd/mekoqa/          the CLI
  suites/
    functional/          tag regression: features, faults, upgrade and universes on a shared stack
    stress/ perf/        load from client pods against a dedicated stack
    canary/              engine self-tests, go test only, not in the catalog
    internal/stacktest/  helpers every suite uses (Accounts, API, MCP, LLM, Universes, Gateway)
    internal/loadtest/   shared plumbing of the stress and perf suites
```

The engine (`core`, `sched`, `exec`, `report`) knows nothing about meko. Everything that
talks to the product or to another system is an app under `framework/apps/`:

| App | What it is |
|---|---|
| `mekostack` | Renders values and runs the Helm release of one stack, waits for readiness, tears it down |
| `mekoapi` | Hand-written REST client for the api-server routes tests call |
| `mcpclient` | MCP over streamable HTTP |
| `llmmock`, `cognitomock` | Clients of the two mocks, plus the servers under `server/` |
| `mekoload` | Starts `meko-load` pods and steers them |
| `minio` | The S3 the stack and universe backups use |
| `yugabytedk8s`, `ybk8s`, `ybastack`, `yba*`, `pg` | Database providers and their SQL access |
| `metrics`, `netshape`, `mockpod`, `clientpod` | Server-metrics sampler, network shaping, pods that run a binary copied in |

`suites/` imports the framework, never the reverse. Suites register their tests in
`init()`, and `framework/cmd/mekoqa/main.go` blank-imports each suite package. A new suite
package needs one import line there.

The module imports no product code. It reaches meko over the wire (REST, MCP, SQL, kubectl),
so `go build ./...` works with the rest of the repo absent.

## One run, start to finish

1. **Select.** `--test`, `--testr`, `--suite` and `--tag` filter the registry. Setup-only
   tests are dropped unless `--setup-only` is set.
2. **Configure.** The embedded `configs/default.yaml`, then each `-c` file, then each `-jc`
   file, then each `-o key=value`. The resolved config is recorded on every result, with
   credentials redacted.
3. **Pick a provider.** `--fake-env` selects the fake provider. Otherwise a kube context
   (`--kube-context`, `env.k8s.context`, or the current context) selects the k8s provider.
   With neither, real-environment tests skip.
4. **Schedule.** The scheduler gives each test an environment, described in the next section.
5. **Run each test.** Per test: registered `Skip`, `Preflight`, the version gate, provision,
   guard mark, the body, guard check, collect, teardown. Every step before provision skips
   without costing an environment.
6. **Report.** Results stream to the sinks as they happen. The run ends with a summary and an
   exit code.

Exit codes: `0` all selected tests passed (or passed and skipped), `1` a test failed, broke,
timed out or could not get an environment, `2` usage, `3` the selection matched no test,
`4` tests were selected but none ran.

### Statuses

The status says where the failure happened, so a reader knows whose problem it is.

| Status | Meaning |
|---|---|
| `passed` | The body returned nil, no `t.Fail`, the guard was clean |
| `failed` | An assertion failed, or the guard saw the services misbehave. A product signal |
| `broken` | The test body panicked. A harness bug |
| `timed_out` | The test timeout fired. The body is cancelled, collect and teardown still run |
| `env_failed` | The environment could not be built, or the cluster API was unreachable. Never a product signal, never a pass |
| `skipped` | Not run, with a reason. Never a pass |

A body marks its own error as an environment failure with `core.EnvFail(...)`.

### Seed

Each run has one seed, printed at the start and recorded on every result. All randomness
(workloads, fault timing and targets) derives from it, so `--seed` replays a run. Resource
names use `t.ResourceSuffix()`, which adds a per-execution nonce so two runs of one seed
never collide.

## The scheduler

A test declares `EnvSpec{Sharing, Shape}`.

- `Shape` says what environment it needs. `SingleStack` is the run's shared stack,
  `DedicatedStack` is a whole stack of its own, `Remote` is an existing deployment attached by
  URL, `ClusterOnly` is the cluster and the database provider with no stack, for a test that
  builds everything it uses.
- `Sharing` says whether it may share. `Shared` tests pack onto one environment, up to
  `sched.per_env_parallelism` (4) at a time. `Exclusive` tests each take their own.

| Shape | Namespace | Used by |
|---|---|---|
| `SingleStack` | `mekoqa-st-<run>`, one per run, built on first use | `functional` |
| `DedicatedStack` | `mekoqa-ds-<run>-<n>`, built and deleted with the test | `stress`, `perf` |
| `Remote` | none, attached by `env.k8s.remote_url` | smoke tests against a deployed portal |
| `ClusterOnly` | none of its own, the test builds universes or stacks | `functional` upgrade, xcluster, read replica |

When the shared stack never comes up, only the `SingleStack` tests report `env_failed`. Every
other shape still runs, and none of them builds the shared stack when it is all a run selects.
A `ClusterOnly` test must be `Exclusive`.

`-o env.stack=dedicated` moves every `SingleStack` test onto a stack of its own for one run,
which rules out cross-test interference. `-o env.stack=shared` does the opposite.

Capacity has two layers:

- `sched.capacity` (8) is the number of lease units for the run. A shared stack costs 1 and
  a dedicated stack costs 2 (`sched.weights`). `--parallel` caps running test bodies.
- `sched.cluster_slots.capacity` (12) caps the stacks all runs together may hold on one
  cluster, through the ConfigMap `default/ampqa-cluster-slots`, which ampqa shares. A test
  with `EnvSpec.ClusterSlots` waits for its slots before its timeout starts.

A test that faults the shared stack (`FaultsSharedInfra`) runs alone on it. `SharedLock`
serializes tests that need one stack-wide resource. If a faulting test fails and leaves the
shared stack unserving past `timeouts.shared_infra_probe`, the stack is rebuilt in
`mekoqa-st-<run>-r<n>` on a new database and the remaining shared tests run there. Only a
failed rebuild makes them report `env_failed`, naming the test that broke it.

A bring-up that fails for any reason but a bad image is first rebuilt once the same way, on the
same provider, so a run's YBA is not stood up again. A scheduling failure (unschedulable pod,
insufficient CPU or memory, readiness timeout) is then retried `sched.env_retries` times after
`sched.env_retry_backoff` on a rebuilt provider.

## The stack

A stack is one Helm release of `meko/deploy/helm/meko` in its own namespace, built by
`apps/mekostack` from the BYOC presets in this order:

```
values-gcp.yaml  ->  values-dev.yaml  ->  values-byoc.yaml  ->  <generated per stack>
```

The generated file is the only part mekoqa writes. It carries the image tags, fresh
`dataPlaneSecrets`, the MinIO S3 settings, the LLM mock endpoints, the database host and
3 api-server replicas. The chart's templates do the rest, so a stack mekoqa builds is the
stack a customer installs. The file holds secrets, so it is written `0600` and its attached
copy is masked.

What runs in a stack namespace:

| Workload | From |
|---|---|
| api-server, mcp-server, inference-gateway, ui | the chart |
| langfuse web and worker, Valkey | the chart |
| rag-agent text, pdf and conversations | the chart, turned on by mekoqa |
| MinIO | `apps/minio`, the S3 for uploads and backups |
| `llmmock` | `apps/llmmock`, the model mock every LLM call goes to |
| `cognito-mock` | `apps/cognitomock`, only with `env.mekostack.auth=cognito_mock` |
| `mekoqa-ysql` | a pod with `ysqlsh` for the framework's own SQL |
| `mekoqa-backing` | a ConfigMap and Secret recording the database, so a later `--env` attach finds it |
| load client pods | `apps/mekoload`, stress and perf only |

Every namespace carries the labels `mekoqa=1` and `mekoqa-test=true`, and an expiry
annotation (`env.mekostack.ttl`, 12h). The reapers select on these labels and never on a
name pattern.

### Bring-up

A fresh stack takes about 20 minutes, most of it the api-server and Langfuse schema
migrations running as DDL on YugabyteDB. Three choices in the install are deliberate:

- It runs in two Helm passes. The first holds the api-server at 0 replicas until Langfuse's
  schema init finishes. A Langfuse DDL landing on golang-migrate's `schema_migrations` write
  makes YugabyteDB refuse it with a catalog version mismatch, and the database stays at
  `Dirty database version 1`. The framework detects that state and rebuilds once. An api-server
  with the MEKO-831 recovery clears a dirty version 1 itself, but not a later one, and older
  builds do not, so the ordering stays.
- Helm runs without `--wait`. The migrations in init containers outlast the Deployments' 600s
  progress deadline, which Helm reads as a failed install. The framework waits for every
  workload itself.
- After the first request creates the dev account, the framework moves it to the `enterprise`
  tier. The free tier refuses the 11th datapack over the account's lifetime, and one shared
  stack's tests pass that.

### The database

`db.provider` picks what builds the YugabyteDB under the stack.

| `db.provider` | Builds | Notes |
|---|---|---|
| `yugabyted` (default) | yugabyted in pods, one StatefulSet per zone, in `mekoqa-yu-<run>-...` | RF3 on three nodes of 2 CPU and 6 GiB. At 1 CPU the migrations hold their advisory lock too long and fail |
| `yba` | YugabyteDB Anywhere, one per run, in `mekoqa-yba-<run>` | Not usable for the stack database unless its image tag is one YBA accepts |
| `chart` | the chart's bundled single-node database | Only to test what a BYOC customer installs. Ignores every other `db.*` key |

The `db.*` keys (`rf`, `nodes`, `zones`, `cpu`, `memory_gib`, `disk_gib`, `storage_class`,
`image`, `version`, `master_gflags`, `tserver_gflags`) are documented in `default.yaml` and
overridable with `-o`. A test can ask for a database three ways:

- the run, through `db.*`
- `EnvSpec.Database`, a `*universe.Spec` for a `DedicatedStack` test (a shared stack ignores it)
- `stacktest.Universes(t).Create(ctx, spec)` in the body, for extra universes the test owns

`framework/universe` holds the provider-neutral `Spec` (name, version or image, clusters,
RF, nodes per zone, node shape, gflags) and the optional capabilities a universe may offer
(`XCluster`, `ReadReplicas`, `Backups`, `PITR`, `Upgrades`, `Gflags`, `NodeControl`), found
with `universe.Capability[T](u)`. A provider that cannot returns `*universe.Unsupported` and
the test skips. The stack's database always gets `ysql_yb_enable_mage=true` (named in
`allowed_preview_flags_csv`) and the `vector`, `pg_dist_rag` and `mage` extensions are checked
before the chart is installed.

### Accounts

With no identity provider the api-server runs every request as one dev account and ignores
the Bearer header. That is the BYOC default and `env.mekostack.auth=none`. It cannot test
sharing, grants or PAT scopes.

`env.mekostack.auth=cognito_mock`, the default, runs the api-server's real Cognito path against a mock user
pool in the namespace, with no product change. The api-server reaches Cognito at two
hardcoded `https://cognito-idp.<region>.amazonaws.com` addresses (the AWS SDK and a JWKS
fetch), so the stack:

- runs `mekoqa mock cognito` in a pod, serving the Cognito JSON API and the JWKS over TLS
  with a certificate for that hostname
- gives the api-server `hostAliases` resolving the hostname to the mock Service, and
  `SSL_CERT_FILE` pointing at the mock's CA
- generates the signing key and the CA per stack. A confirmation code is always `123456`

A second plain-HTTP control port lets the framework create confirmed users and mint tokens
the product never would (expired, from another pool, an ID token). It is in-cluster and
unauthenticated.

`stacktest.API(t)` returns the stack's default client, which in `cognito_mock` mode is the
default account logged in once. `stacktest.Accounts(t, n)` creates `n` more accounts for a
test, named after it, and skips on a stack with `auth=none`.

### The LLM

meko calls OpenAI or Bedrock over the internet. In a stack `llmmock` answers instead, so a
run needs no key and no network. It is `mekoqa mock llm`, stdlib only. `env.mekostack.llm`
is `mock` by default. `off` leaves the callers on real providers and is not used by any
suite.

Each caller finds the mock through the variable its SDK already reads: `OPENAI_BASE_URL` and
`AWS_ENDPOINT_URL_BEDROCK_RUNTIME`, set through the chart's `extraEnv` values for the
api-server and mcp-server, `inferenceGateway.config` and `ragAgent.config`. A chart without
`extraEnv` gets the same environment patched onto the Deployments after install.

The mock answers `/v1/embeddings`, `/v1/chat/completions` (also streamed), `/v1/responses`,
`/v1/models`, and Bedrock `Converse`, `ConverseStream` and `InvokeModel`. A test controls it
through `stacktest.LLM(t)`:

- **Embedding fixtures** map a text to a vector the test chose, zero-padded to the width the
  caller asked for. Text with no fixture gets a deterministic token-hash vector, so texts
  that share words come out close.
- **Chat scripts** give a reply, or tool calls with fixed arguments, to the first request
  whose prompt holds a substring. Without a script the built-in answers cover what the stack
  always sends.
- **Faults** are latency, an HTTP status (429 with `Retry-After`, 500), a hang, malformed
  JSON or a cut stream, per route and for a set number of requests.

One mock serves the whole stack, so every script and fault takes a `Match` substring and a
test puts a marker word in its own text. A test removes its own rules and never calls
`Reset`. The mock records every call (route, model, input, tools, status, the fault that
fired, what answered), a test reads them with `Calls(Filter{...})`, and teardown attaches
them as `Stack llmmock calls.json`.

### How the mock pods get the binary

Both mocks run on `env.mekostack.mocks.image` (`mirror.gcr.io/library/alpine:3.20`). The pod waits for
`/opt/mekoqa/.ready`, and the framework copies the `mekoqa` binary in with `kubectl cp`
because a ConfigMap holds 1 MiB. The binary is the first of:

1. `env.mekostack.mocks.binary`, a ready linux binary
2. the running executable, when it is a static linux binary for `env.mekostack.mocks.arch`
3. a cross-compile (`CGO_ENABLED=0 GOOS=linux GOARCH=<arch> go build ./framework/cmd/mekoqa`),
   once per run, cached in the user cache dir. It needs a Go toolchain and the source checkout

The pod carries the binary's digest as a label, so a changed binary rolls the pod. The
load pods get `meko-load` the same way.

### Images

Every meko image comes from `quay.io/yugabyte`, published by `meko-build`, and the framework
refuses a `meko-*` image from anywhere else before it provisions anything. Public third-party
images (Valkey, MinIO, Alpine) come from their own registries.

`--image-version <tag>` sets one tag for every `meko-*` image, either a release
(`0.261002.0-b75b2a658`) or a custom build (`custom-<sha>`). Each component also takes its
own tag (`env.mekostack.images.<component>.tag`) for a `meko-build` custom build of one
component. With nothing set, `default.yaml` pins a release. The database image follows
YugabyteDB's cadence and has its own key, `env.mekostack.images.database`. Every image is
private, so a run needs a Quay credential (`secrets.quay`).

## The guard

Around every test body on a self-built stack, the guard watches the services. A finding fails
the test (`failed`, step `service_guard`) unless the test lists its kind in `Tolerates`.

| Kind | Fatal | Seen as |
|---|---|---|
| `pod_restart` | yes | a container's restart count rose during the body |
| `crash_log` | yes | a panic or traceback in a service log |
| `http_5xx` | yes | the api-server served a 5xx, read from its metrics on port 9464 |
| `error_logs` | no | ERROR log lines, counted on the result |

On a shared stack the window covers every test running in it. A fatal finding is excused
only when an overlapping test on the same stack tolerates that kind, because that test may
have caused it, or when it is an `http_5xx` whose answers all went to overlapping tests' own
`mekoapi` calls, which already fail those tests. Otherwise it fails every test that overlapped. `Unguarded: true` skips the
guard for a body that never calls the stack's services. `-o guard.enabled=false` turns it off
for a run.

## Diagnostics

`report.collect_diagnostics` is `on_failure` by default. When a test needs explaining,
`Collect` gathers evidence before teardown deletes the namespace, and attaches it to the
result. A green run collects nothing. `always` collects every run, `never` collects none.

The attachments form a folder tree on the portal:

| Attachment | Holds |
|---|---|
| `Stack logs/<pod>/<container>.log` | current and previous logs of every stack pod, including init containers |
| `Stack events.txt`, `Stack resources/`, `Stack describe/` | namespace events, wide listings and describes |
| `Stack values.yaml` | the generated Helm values, secrets masked |
| `Stack llmmock calls.json` | every call the LLM mock saw |
| `Universe logs/` | the same for the database namespace |
| `Stack nodes.txt`, `Stack node-usage.txt` | cluster nodes and their usage |
| `mekoload-<id>/` | client pod logs, summaries and event files of a load test |

Secrets are masked in logs, step properties and text attachments. A credential is declared
by name under `secrets:` (`env:VAR`, `file:/path` or `token:literal`), and a credential-shaped
config key (`quay_token`, `*.password`) is redacted from the config snapshot and masked
everywhere once it differs from its default.

## Teardown and the reap plan

Teardown runs once provisioning succeeded, on its own deadline (`timeouts.teardown`, 5m),
after the body, after collect and after any `t.OnTeardown` functions (last registered runs
first). A timed-out or failed body does not skip it.

A process that is killed never reaches its own teardown. To cover that, the run writes a
reap plan (`--reap-plan`, default `<out-dir>/reap-plan.json`) listing the namespaces and
cloud tags it holds. It updates the file as stacks come and go and removes it when nothing is
live. A file left behind means the run died. `mekoqa env reap --plan <file>` reclaims exactly
what it names. `meko-cleaner` on Jenkins deletes `mekoqa-*` namespaces by label and age as a
second net.

Only objects the framework created (labelled `mekoqa-test=true`) are ever modified or
deleted. The run never touches another namespace, workload or cluster-scoped object.

## Keeping environments

`--env-mode` sets what happens to the environment after the run.

| Mode | Behavior |
|---|---|
| `build` (default) | provision, run, tear down |
| `keep` | provision, run, record the environment in the registry, skip teardown |
| `keep-failed` | tear down when everything passed, keep and record when anything did not |
| `attach` | attach to `--env <id>`, run, skip teardown |

`--setup-only` stands each selected test's environment up, skips its body and keeps it. A kept
environment gets the labels `mekoqa-keep=true`, which stops the janitor reaping it on age, and
a registry entry in `~/.mekoqa/envs.json` (`--registry`). `mekoqa env ls` lists entries,
`mekoqa env rm <id>` tears one down from its recorded plan, and `--ttl` auto-reaps after a
duration. The registry file lives on the machine that ran the command. On Jenkins the
build's container is gone afterwards, so there the namespace labels are the only record.

## Reporting

`--portal <url>` streams the run to the meko report portal while it executes. That covers each result,
step, attachment, metric and perf datapoint. `report.portal.url` defaults to
`https://meko-qa.dev.yugabyte.com`, so a run reports there unless you pass `--portal ""`. `--report
<bucket>` files the run under a report page. The sink is non-critical, a portal outage never
fails or slows a run. Pipelines pass `report.portal.run_id` so the framework adopts the run
the portal created at trigger time, and the identity stamps (`branch`, `commit`,
`build_tag`, `triggered_by`).

Without a portal, `--out-dir <dir>` writes `<dir>/<runid>/run.json`, one JSON file per
result and an `artifacts/` tree. `--junit` and `--html` write a JUnit file and a
self-contained HTML report, and `mekoqa report --from <dir> --out report.html` renders HTML
from a finished run. Every result records the image tag of each component.

## Load: stress and perf

Load never runs on the coordinator. `apps/mekoload` starts pods that run `meko-load`, an
HTTP-controlled Go binary from `yugabyte/qa-services` (`meko-load/`). It drives the REST API
and the MCP server with virtual users spread over a list of PATs (workloads
`datapack_churn`, `pat_churn`, `mcp_conversations`, `memory`, `mixed`), times each call into
a histogram, classifies failures, checks cheap invariants and cleans up after itself. The
coordinator mints the PATs, starts the pods, steers them and merges their summaries.

The binary comes from the first of `apps.mekoload.binary`, `apps.mekoload.source` (a
checkout built for linux) or the newest `meko-load-*` release of `qa-services` (needs a
GitHub token in the secret named by `apps.mekoload.github_secret`).

The api-server allows 10 active PATs per account, and every PAT the load mints counts. A
plan is refused when `credentials + client_pods * max_active_pats` is over 8. The api-server
also limits `/api/v1` to 10 requests a second per client IP, so the suites send
`forwarded_for: per_request`, which gives every call its own `X-Forwarded-For`.

Stress tests run on a dedicated stack with a duration and a pass bound (error rate, no 5xx,
no invariant break, cleanup leaves nothing). Perf tests run a closed loop at each
concurrency of a ladder (`[1, 8, 32]`), one fresh run per level, and record datapoints named
`<api|mcp>.c<users>.<op>.<stat>` (`p50_ms`, `p95_ms`, `p99_ms`, `throughput_ops`). A level
whose calls fail records no latency, because a percentile over errors measures nothing. Every
default is a key under `stress.` or `perf.` in `default.yaml`.

## Versions

A test declares a `VersionSpec` (`MinProduct`, `MaxProduct`, `MinYB`, `MaxYB`) and the
framework skips it outside the range, before provisioning. A test body never branches on a
version. Cross-version differences are handled inside apps.

## Upgrade safety is a test subject

meko requires every change to survive old and new builds running together. CI runs one build,
so nothing else checks that. `functional/upgrade_previous_release` installs the release before the run's
(`upgrade.from` overrides it) from its own chart and images, seeds data, upgrades in place
and checks the data and the API outage window (`upgrade.max_outage`). The previous release
comes from the portal's release list, never from a constant.
