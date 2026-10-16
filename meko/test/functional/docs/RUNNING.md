# Running tests

You can run mekoqa in three places. They all end in the same `mekoqa run` and the same
results on the portal.

| Where | Use it for |
|---|---|
| Your machine, against amp-dev | Developing a test, reproducing a failure, one-off checks |
| The portal (Run tests dialog or `yb meko run trigger`) | Running a branch or release the way the team does, with no setup |
| The Jenkins job `meko-run-tests` | Anything the portal does, plus parameters the portal does not expose |

The portal calls the Jenkins job, so the first two use the same pipeline. Only the local run
uses your machine and your credentials.

A fresh stack takes about 20 minutes to build. Plan a run around that, and prefer one run with
several tests over several runs with one.

In this document `mekoqa` stands for `go run ./framework/cmd/mekoqa` run from
`meko/test/functional`, or a binary built from it.

## Selecting tests

```bash
mekoqa collect                      # every test, one line each
mekoqa collect --tag regression     # one tier
mekoqa describe functional/datapack_lifecycle   # a test's description and its config keys
mekoqa manifest                     # commands, tests and config keys as JSON, for agents
```

`run` selects with `--test <exact name>`, `--testr <regex>`, `--suite <regex>` and `--tag <tag>`
(repeatable, a test needs every tag). Tags in use:

| Tag | Tests |
|---|---|
| `regression` | every `functional/` test, shared stack. The portal's Functional card |
| `stress` | every `stress/` test, dedicated stack each. The Stress card |
| `perf` | every `perf/` test, dedicated stack each. The Perf card |
| `universe`, `upgrade`, `resilience` | subsets of `functional/`, to run one area alone |

There are three suites, `functional`, `stress` and `perf`. The engine's self-tests in
`suites/canary` run under `go test` only and are not in the catalog.

## 1. Locally against amp-dev

mekoqa builds its own stack in a new namespace on the cluster your kube context points at.
Nothing runs on your machine except coordination.

### Prerequisites

- Go, to run `go run` and to cross-compile the mock binary for the cluster's nodes
  (`env.mekostack.mocks.arch`, default `amd64`).
- `kubectl` and `helm`, authenticated to the cluster. For amp-dev:

  ```bash
  gcloud container clusters get-credentials amp-dev --region us-west1 --project yugabyte
  kubectl config current-context        # check it before you run anything
  ```

  The run builds on the current context unless you pass `--kube-context`. A wrong context
  builds `mekoqa-*` namespaces on the wrong cluster, so look first. The framework only touches
  namespaces it created and labelled.
- A Quay credential with pull access to `quay.io/yugabyte/meko-*`, as `<user>:<token>`. Every
  meko image and the database image are private.
- A route from your machine to the cluster's pod and Service addresses (the corp tailnet). Without
  it the framework falls back to a `kubectl port-forward` per stack. `-o env.mekostack.reach=port_forward`
  forces that.

### Run

From `meko/test/functional`:

```bash
export MEKOQA_QUAY='<user>:<token>'

go run ./framework/cmd/mekoqa run \
  --test functional/stack_health \
  --kube-context gke_yugabyte_us-west1_amp-dev \
  --image-version 0.261002.1-b5c44e042 \
  -o secrets.quay=env:MEKOQA_QUAY \
  --out-dir ./mekoqa-out --html ./mekoqa-out/report.html \
  --portal ""
```

- `--portal ""` keeps the run off the portal. Without it the run streams to
  `https://meko-qa.dev.yugabyte.com`, a local run included, and a debugging run shows up beside the team's.
- `--out-dir` writes `<out-dir>/<runid>/` with one JSON per result, the artifacts and the
  reap plan. `--html` and `--junit` write a report file.
- Build once with `go build -o /tmp/mekoqa ./framework/cmd/mekoqa` if you run it often, and use
  `/tmp/mekoqa run ...`. A static linux build is also what the mock pods copy in.
- `--parallel N` runs `N` test bodies at once (default 1). `--per-env-parallelism` (default 4) is
  how many shared tests co-tenant on one stack.
- `--seed N` replays a run's randomness. The seed is printed at the start.
- Stress and perf tests need the `meko-load` binary: `-o apps.mekoload.source=<path to a
  meko-load checkout>`, or a GitHub token for the release download (`-o
  secrets.github=env:GITHUB_TOKEN`).

Quick checks that need no cluster:

```bash
go run ./framework/cmd/mekoqa run --tag regression --fake-env --portal ""  # every test skips, exit 4
go test ./suites/canary                                                     # the engine's self-tests
```
 `--fake-env` provisions nothing. A test that builds infrastructure in its own body still does.

### If a run dies

A killed run leaves its namespaces behind. The reap plan names them:

```bash
go run ./framework/cmd/mekoqa env reap --plan ./mekoqa-out/reap-plan.json -o env.k8s.context=<ctx>
```

`meko-cleaner` on Jenkins also deletes `mekoqa-*` namespaces by label and age. Never delete
namespaces by hand that you did not create.

## 2. From the portal

The portal is at `https://meko-qa.dev.yugabyte.com`. The Run tests dialog and the CLI send the same request
to the portal, which starts `meko-run-tests`.

```bash
yb meko run trigger --tag regression --dry-run      # print the request, send nothing
yb meko run trigger --tag regression --yes --wait   # launch and follow
yb meko run trigger --test functional/stack_health --branch me/my-fix --yes
```

`yb meko run trigger --help` is the authority on flags. The fields are:

| Flag | Meaning |
|---|---|
| `--tag`, `--test`, `--testr` | selector. One is required |
| `--branch` | meko branch to check out. Unset takes the portal default |
| `--commit` | pin a sha. Implies `--build-from-source` |
| `--build-from-source` | build the images from the branch first, tag `custom-<short-sha>` |
| `--image-version` | one prebuilt Quay tag for every meko image |
| `--version image=tag` | one component's tag. Images: `api_server`, `mcp_server`, `ui`, `inference_gateway`, `rag_agent`, `langfuse` |
| `--database-image` | a full reference, such as `quay.io/yugabyte/yugabyte-itest:<tag>`. Unset takes `env.mekostack.images.database` from `framework/configs/default.yaml` |
| `--option k=v` | a mekoqa `-o` override, repeatable |
| `--parallel` | concurrent test bodies |
| `--cluster` | cluster, default from the portal |
| `--keep-env`, `--keep-env-failed` | keep the stack after the run, see below |
| `--report`, `--owner` | report bucket and who launched it |

Leave every flag you do not need unset. The portal fills in branch, cluster and parallelism.
`--yes` is required to launch, and `--dry-run` shows what would go.

The run appears on the portal while it builds. If the run shows no tests, check the
Jenkins console for `portal sink` lines.

### Config page defaults

The portal's Config page holds defaults every launch inherits when it names none.
`yb meko settings` prints them and `yb meko settings set ... --yes` changes them.

| Setting | Effect |
|---|---|
| `default_branch` | The meko branch a run builds from. Also what scheduled runs use |
| `default_k8s_cluster` | The cluster, `amp-dev` today |
| `default_parallel` | Concurrent test bodies. Unset falls back to the portal's own default |
| `default_database_image` | The database image. Unset takes the framework's |
| `default_options` | `-o` overrides merged under every launch's own options |

```bash
yb meko settings set --default-option db.provider=yba --yes
yb meko settings set --unset-default-option db.provider --yes
```

Changing a default changes every later launch for everyone, so do it deliberately. The
page also shows `default_yb_version`. It is stored on the run and does not reach the
framework. Choose the database with `--database-image`.

## 3. Through Jenkins

The job is `users/shubin/meko-devops/MEKO/meko-run-tests` on `jenkins.dev.yugabyte.com`. Use it
directly for a parameter the portal does not expose. `yb jenkins job params users/shubin/meko-devops/MEKO/meko-run-tests`
lists the live parameters. The ones you will use:

| Parameter | Default | Meaning |
|---|---|---|
| `TAGS` | `regression` | `mekoqa run --tag`. Ignored when `TEST` is set |
| `TEST`, `TESTR` | empty | one exact test, or a name regex |
| `PARALLEL` | `2` | concurrent test bodies |
| `BRANCH` | the team branch | meko branch to check out and, in custom mode, build |
| `COMMIT` | empty | exact sha. Implies `BUILD_FROM_SOURCE` |
| `BUILD_FROM_SOURCE` | false | build the images through `meko-build` first |
| `IMAGE_VERSION` | empty | one Quay tag for all images. Empty is the framework's pin |
| `API_SERVER_VERSION`, `MCP_SERVER_VERSION`, `UI_VERSION`, `INFERENCE_GATEWAY_VERSION`, `RAG_AGENT_VERSION`, `LANGFUSE_VERSION` | empty | per-component tags |
| `DATABASE_IMAGE` | empty | full Quay reference of the database image |
| `MCP_REF`, `LANGFUSE_REF`, `YBDB_REF` | empty | refs of components `meko-build` pulls in by ref |
| `OPTIONS` | empty | a JSON object of `-o` overrides, `{"db.provider": "yba"}` |
| `K8S_CLUSTER_NAME`, `K8S_CLOUD`, `K8S_REGION`, `K8S_PROJECT` | `amp-dev`, `gke`, `us-west1`, `yugabyte` | the cluster to run on. `meko_dev_eks` runs on EKS |
| `KEEP_ENV`, `KEEP_ENV_ON_FAILURE` | false | keep the stack, see below |
| `REPORT` | empty | portal report bucket |
| `PORTAL_URL`, `RUN_ID` | the portal, empty | where the run streams. With `RUN_ID` set the framework adopts that portal run |

`YB_BACKING` is also a parameter and its description names `env.yb_deploy`, a key the
framework no longer reads. Choose the database with `OPTIONS`, for example
`{"db.provider": "yba"}`.

The job archives the JUnit file and `report.html`, and the run's results are on the portal
under the run id.

## Configuration overrides

Later layers win. The order is the embedded `framework/configs/default.yaml`, then each `-c`
file, then each `-jc` JSON file, then each `-o`. `default.yaml` documents every key. `mekoqa describe
<test>` lists the keys one test reads.

`-o key=value` nests on dots. A value is a string unless it starts with a type prefix:

| Form | Value |
|---|---|
| `-o env.mekostack.auth=cognito_mock` | a string |
| `-o stress.client_pods=int:4` | an integer |
| `-o env.mekostack.rag_workers=bool:false` | a boolean |
| `-o db.zones=list:us-west1-a,us-west1-b` | a list |
| `-o db.master_gflags=json_text:'{"a":"1"}'`, `json_file:<path>` | a JSON value |
| `-o secrets.quay=env:MEKOQA_QUAY` | a declared secret (`env:`, `file:` or `token:`) |

Settings you will reach for:

| Key | Effect |
|---|---|
| `env.mekostack.auth` | `cognito_mock` (default) or `none` |
| `env.mekostack.image_version`, `env.mekostack.images.<component>.tag`, `env.mekostack.images.database` | image tags |
| `env.stack` | `dedicated` or `shared`, moves every stack test between the two shapes |
| `db.provider`, `db.rf`, `db.nodes`, `db.cpu`, `db.memory_gib` | the database under the stack |
| `report.collect_diagnostics` | `on_failure` (default), `always` or `never` |
| `guard.enabled` | `false` turns the guard off |
| `timeouts.<category>`, `timeouts.provision_multiplier` | test and provision timeouts. Raise the multiplier on a busy cluster |
| `sched.capacity`, `sched.cluster_slots.capacity` | lease units, and stacks per cluster |
| `owner` | stamped on every resource, default `mekoqa` |

### Cognito mock

`env.mekostack.auth=cognito_mock`, the default, runs the api-server's Cognito path against
a mock pool. `-o env.mekostack.auth=none` runs every request as one account, and every test
that needs accounts skips.

### Image versions

`--image-version <tag>` (or `IMAGE_VERSION`) sets one tag for every `meko-*` image. List
candidates with:

```bash
yb meko releases --limit 5                          # complete release tags
```

The catalog's `meko-yugabytedb` tags predate Bedrock: their pg_dist_rag has no `AWS_BEDROCK`,
so data migration 1 fails on them. The default database is the `yugabyte-itest` build
`values-byoc-bundled-db.yaml` ships, and a render test fails when the two drift.

For a branch under test, use the portal's `--build-from-source` or `BUILD_FROM_SOURCE`, which
calls `meko-build` in custom mode and runs against `custom-<short-sha>`. A custom build of one
component at its own ref is a `meko-build` run with `MCP_REF`, `LANGFUSE_REF` or `YBDB_REF`,
then `--version mcp_server=custom-<sha>`. Images come only from `quay.io/yugabyte`. The
framework refuses a `meko-*` image from anywhere else.

## Keeping an environment

By default the stack is torn down after the run. Keep it to debug a failure or to run more
tests against it.

| Setting | Behavior |
|---|---|
| `--env-mode keep`, portal `--keep-env`, Jenkins `KEEP_ENV` | keep the stack after the run |
| `--env-mode keep-failed`, `--keep-env-failed`, `KEEP_ENV_ON_FAILURE` | keep it only when something did not pass |
| `--setup-only` | build the stack, skip the test bodies, keep it |
| `--env <id>` (`--env-mode attach`) | run against a kept stack and tear nothing down |
| `--ttl 8h` | auto-reap a kept stack after a duration |
| `--env-name` | a label for the kept entry |

```bash
mekoqa run --test functional/stack_health --env-mode keep --kube-context <ctx> -o secrets.quay=env:MEKOQA_QUAY
mekoqa env ls
mekoqa run --tag regression --env <id> -o secrets.quay=env:MEKOQA_QUAY
mekoqa env rm <id>
```

`env ls` reads `~/.mekoqa/envs.json` (`--registry`), so it lists only what this machine kept. A
stack kept by Jenkins is findable by label on the cluster:
`kubectl get ns -l mekoqa-keep=true`. A kept stack costs cluster CPU, so remove it when done.
`mekoqa env rm <id>` reconstructs its teardown from the registry, and `--force` drops the entry
even when teardown fails.

## Exit codes

`0` all selected tests passed (or passed and skipped), `1` a test failed, broke, timed out or
had no environment, `2` usage, `3` the selection matched no test, `4` tests were selected and
none ran.

## Other commands

| Command | Does |
|---|---|
| `mekoqa report --from <out-dir/runid> --out report.html` | renders an HTML report from a finished run, no network |
| `mekoqa env reap --plan <file>` | reclaims what a killed run recorded |
| `mekoqa env reap --tag <runTag>`, `--stale <duration>` | sweeps cloud instances (`--cloud aws` or `gcp`). It does not delete Kubernetes namespaces |
| `mekoqa task [name]` | runs one built-in task, or lists them |
| `mekoqa mock llm`, `mekoqa mock cognito` | runs a mock on your machine, see below |

### The mock commands

The two mocks are part of the `mekoqa` binary. A stack runs them in pods, and you can run them
by hand to try a request:

```bash
go run ./framework/cmd/mekoqa mock llm --port 8080
go run ./framework/cmd/mekoqa mock cognito --tls-port 8443 --ctl-port 8081
```

`mock llm` takes `--host` and `--port` (default `$LLMMOCK_PORT`, else 8080). `mock cognito` takes
`--region`, `--pool-id`, `--client-id`, `--client-secret`, `--token-ttl`, `--code`, `--kid`,
`--tls-port`, `--ctl-port`, `--tls-cert`, `--tls-key`, `--token-key` and `--state`. Without
`--tls-cert` the public listener is plain HTTP, without `--token-key` the signing key is
generated, and without `--state` users stay in memory.
