# mekoqa: plan for a meko system-test framework, pipelines and report portal

Status: planning document, written 2026-10-02 and kept for the reasoning behind the design.
Much of it has since been built and parts changed, so it does not describe current behavior.
For how mekoqa works today read [`README.md`](README.md) and [`docs/`](docs/). Where this
file disagrees with them, the code and `docs/` win.

Known drift: the framework, the portal and the pipelines exist and run. `apps/mekoapi` is
hand-written, not generated from the swagger. The suites are `functional`, `stress` and
`perf`, there is no `quality` suite and the real-model tier below is not built. The database is a `db.*` provider
(`yugabyted`, `yba` or `chart`), not the bundled subchart alone.

amp has a working QA loop: the `ampqa` framework (`amp/test/functional/`), Jenkins
pipelines in meko-devops (`jenkins/amp/`), and a report portal in qa-services
(`amp-report-service/`). meko has the daily image build and unit tests, and nothing
that deploys a meko stack and tests it end to end. This plan copies the amp loop for
meko: a framework called `mekoqa`, a portal called `meko-report-service`, and a set of
`meko-*` Jenkins jobs. The engine ideas stay the same. The product apps, suites and
deploy path are new, because meko is a different product.

[`README.md`](README.md) and [`docs/`](docs/) next to this file describe how the framework works.
The Jenkins side is `jenkins/meko-qa/README.md` in meko-devops, and the portal is
`meko-report-service/README.md` in qa-services, each on its own branch (see
[Where everything lives](#where-everything-lives)).

## What meko is, from a tester's side

meko is spread over five repositories. A test has to drive all of them through one
deployment, so the framework sees them as one stack.

| Component | Source | Image (`quay.io/yugabyte/`) | What a test drives |
|---|---|---|---|
| api-server | `yugabyte/meko` `meko/api_server` | `meko-api-server` | REST `/api/v1`: datapacks, agents, knowledge bases, memories, collections, sharing, `meko-tokens`, deployments, health. Spec committed at `meko/api_server/docs/swagger.yaml` |
| mcp-server | `yugabyte/meko-mcp-server` | `meko-mcp-server` | MCP over streamable HTTP. 25 tools in `TOOLS_MANIFEST.json`: `memory_*`, `conversation_*`, `knowledgebase_*`, `context_search`, `datapack_*`, `artifact_*`, `track_token_usage` |
| inference-gateway | `yugabyte/meko` `meko/inference_gateway` | `meko-inference-gateway` | `/chat`, `/chat/confirm-action`, `/memory/add`, `/index-conversation` |
| rag-agent (text, pdf, conversations workers) | `yugabyte/yugabyte-db` `python/ai/rag_agent` | `meko-rag-agent` | nothing directly. It drains the `pg_dist_rag` queue, so a test sees it as "the uploaded document becomes searchable" |
| langfuse web + worker | `yugabyte/meko-langfuse` | `meko-langfuse-web`, `meko-langfuse-worker` | traces and audit records for tool calls |
| ui, admin | `yugabyte/meko` `meko/meko_ui`, `meko/meko_admin` | `meko-ui` | HTTP smoke only in the first phases |
| provisioner | `yugabyte/meko` `meko/meko-provisioner` | `meko-provisioner` | `MekoUserAccount` CRs into per-tenant namespaces. Hosted topology only |
| reconcile-agent | `yugabyte/meko` `ycp/cmd/reconcile-agent` | `ycp-reconciler-agent` | converges `agent` and `datapack` resources a YCP control plane declares |
| database | YugabyteDB release with MAGE and `pg_dist_rag` | `meko-yugabytedb` (not built by `meko-build`) | YSQL on 5433. A stock YugabyteDB image fails the first data migration |
| cli | `yugabyte/meko` `meko/cli_app` | none, a Go binary | `meko datapack ...` against the api-server |

### How a release is built today

Observed on Jenkins (`users/shubin/meko-devops/MEKO/meko-build`, 34 builds, last one
`0.261002.0-b75b2a658` on 2026-10-02):

- The pipeline is `meko/jenkins/pipelines/pipeline_meko_build.groovy` in this repo, not
  in meko-devops. It runs daily at 14:00 Pacific on the `amp-agent` pool inside
  `ghcr.io/yugabyte/meko-devops/amp-jenkins-agent:0.3`.
- `MODE=release` cuts `0.<YYMMDD>.<patch>` when anything under `meko/` changed since the
  last `meko-0.*` tag (a denylist that excludes docs, tests and `meko/jenkins/`), tags
  every image `<version>-<short-sha>`, and pushes the git tag `meko-<version>`.
  `MODE=custom` tags `custom-<short-sha>`.
- One version covers eight images, built in parallel on ephemeral EC2 builders by
  `meko/jenkins/scripts/build-images-aws.sh`. `meko-mcp-server`, `meko-langfuse` and
  `yugabyte-db` come in by ref (`MCP_REF`, `LANGFUSE_REF`, `YBDB_REF`) and do not trip
  the release gate.
- The database image is published to `quay.io/yugabyte/meko-yugabytedb` outside the
  build, when its tag moves.

The older per-service Jenkinsfiles in meko-devops (`jenkins/api-server/Jenkinsfile` and
siblings, jobs under `dev-environment/` and `prod-environment/`) are the hosted dev and
prod deploy path. The test framework does not use them, or anything they publish.

### How a meko environment is deployed

One Helm chart, `meko/deploy/helm/meko/`, deploys every component. Presets compose
left to right: a cloud preset, an env preset, then a mode preset last.

- `values-byoc.yaml` is a self-contained data plane in one namespace: its own
  YugabyteDB (the `yugabyte` subchart on `meko-yugabytedb`), langfuse with bundled
  Valkey, chart-rendered Secrets (`dataPlaneSecrets`), Cognito off, MCP auth
  `meko_tkn`, provisioner off, rag-agent off, everything pulled from Quay through
  `quay-registry-creds`. `ycp/test/local/meko-reconcile.sh` already installs it on
  kind, which proves the install works with no hand-made Secrets.
- `values-byoc-full.yaml` after it turns on the rag workers and moves the inference
  gateway to OpenAI.
- `mode: mcp` without the BYOC preset is the hosted topology: the provisioner, its CRD
  and ClusterRoles, and per-tenant namespaces.
- `values-ycp.yaml` is the YCP control plane (`api-server --control-plane-mode`).

Two product facts shape the tests:

- **With Cognito off, every request is one account.** `registerNoAuthRoutes` in
  `meko/api_server/internal/api/router.go` runs `DevAuth`, which authenticates every
  request as the account with sub `dev-local`. Tests on a shared stack are isolated by
  datapack, not by account. Tenant-isolation tests need real accounts, which the
  api-server supports through `--auth-provider oidc`.
- **Uploads need S3, and the api-server has a MinIO mode.** `s3_service.go` supports
  static credentials, a custom endpoint and path-style addressing. amp's framework
  already deploys MinIO (`apps/minio`), so knowledge-base tests need no AWS bucket.

### What a developer's BYOC install looks like today

A meko developer's install runbook (2026-10-02, the untracked `meko/meko.md`) is the
hand-made version of what `apps/mekostack` automates. Compared with the BYOC preset
alone it:

- pulls each component from a personal Quay namespace, with per-component tags that do
  not share a version. The framework does not: every image comes from `meko-build`
  (decision 8). A component at its own ref is a `meko-build` custom build with
  `MCP_REF`, `LANGFUSE_REF` or `YBDB_REF` set, and the framework takes a tag per
  component for that case.
- creates `rag-agent-secret` by hand (`YUGABYTEDB_CONNECTION_STRING` plus AWS keys),
  because the chart does not render it. `mekostack` creates it.
- turns the rag workers on with `ragAgent.gpu.enabled=false` and a null
  `nodeSelector` on each worker.
- uses real AWS keys for the gateway's Bedrock, the rag workers and a real S3 bucket
  (`meko-kb-store`, us-east-1). A test stack replaces all three with `llmmock` and
  MinIO and holds no AWS key.
- waits up to `--wait --timeout 20m`, which sets the first guess for
  `timeouts.provision`.
- targets the GKE context `gke_yugabyte_us-west1_amp-dev`, a second candidate
  cluster besides `itest-dev`.

The runbook adds facts the framework has to build on:

- **Readiness has more steps than pods.** The api-server's `ensure-extensions` init
  container logs `extensions ready`. The langfuse pod runs `metadata-schema-init` then
  `tracing-schema-init`, which log `Metadata schema init complete` and `Tracing schema
  init complete`. A pod stuck in `Init:` is the first thing to look at. `mekostack`
  waits on those lines and attaches those init logs to every failed provision.
- **A timed-out `--wait` leaves the release `pending-upgrade`.** The next `helm
  upgrade` then fails. `mekostack` checks `helm list -a` before an upgrade, and a
  release left pending by its own earlier attempt is rolled back first, not
  reported as a product failure.
- **The real S3 needs exact grants.** `s3:ListBucket` on the bucket (the upload's
  dedup check calls `ListObjectsV2`, and without it a missing `HeadObject` turns into
  403), and `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject` on its objects. The rag
  workers need `s3:GetObject` and `s3:ListBucket` on the same key pair as their
  Bedrock calls. `mekostack` gives each component a MinIO user with exactly that
  policy, so a code change that needs a new grant fails in the test, not at a
  customer.
- **The rag workers embed through Bedrock** in this setup (`bedrock:InvokeModel`).
  `llmmock` has to answer Bedrock `InvokeModel` for the Titan embedding models, not
  only OpenAI's.
- **The rag secret repeats the database password.** `rag-agent-secret`'s connection
  string carries `password=yugabyte` while the chart sets the database password from
  `dataPlaneSecrets`. `mekostack` writes both from one generated value.
- **The chart has a cheap check before any cluster.** `helm dependency update`, then
  `helm lint` with the three presets and placeholder tags and langfuse secrets, then
  `helm unittest`. `functional/chart_render` runs the same with `helm template` and the
  stack's real generated values, in seconds, so a chart error fails without waiting
  for a 20-minute install.

### Any cluster, many stacks per cluster

Nothing in the framework names a cluster. The cluster is the kube context the run is
given (`--kube-context`, or `env.k8s.context`, or the current context), and what
differs between clusters is config under `env.k8s.clusters.<name>` with defaults in
`default.yaml`: storage class, node selectors, whether pod and Service addresses are
routable from the coordinator over the tailnet, and the cluster's slot capacity. A
cluster with no entry runs on the defaults. The Jenkins job takes the cluster name
and its cloud (`gke` or `eks`) and fetches the context, so `itest-dev`, `amp-dev` on
GKE and `meko_dev_eks` are each one parameter value.

Several stacks share one cluster, one namespace each. Observed: `helm template` of
`values-gcp.yaml`, `values-dev.yaml` and `values-byoc.yaml` with the rag workers on, at
`8bc5622a4`, renders 9 Deployments, 2 StatefulSets, 8 Services, 8 Secrets, 10
ConfigMaps, 2 PodDisruptionBudgets, 2 ServiceAccounts and 1 PVC. Every one is
namespaced, and there is no ClusterRole, CRD, LoadBalancer Service or host port. So:

- A stack is a namespace `mekoqa-<st|ds>-<run id>-<n>` with a Helm release named
  `meko` in it, labelled with the run id, owner and expiry. Two runs, or two tests in
  one run, never pick the same name.
- Capacity is the shared cluster-slot ConfigMap. A dedicated stack holds slots for
  its whole life, so ten parallel stress tests wait for room instead of overcommitting
  the nodes.
- `meko-cleaner` and `mekoqa env reap` delete by label and expiry, never by name
  pattern alone, so a developer's own `meko-byoc-*` namespace on the same cluster is
  never touched.
- The cluster needs a default storage class: the bundled YugabyteDB's storage guard
  fails the install without one.
- The hosted topology is the exception. The provisioner's CRD and ClusterRoles are
  cluster-scoped, so one hosted stack runs per cluster, behind a cluster-wide lock
  (`LockProvisionerCRD` as a ConfigMap lease), not a per-run lock.

Observed, same render with `values-aws.yaml` in place of `values-gcp.yaml`: the output
is identical, and it sets no storage class, node selector or toleration. So the same
stack installs on GKE and EKS with no chart change.

**Chart changes: none needed so far.** The only chart-side finding is cosmetic. In a
namespace other than `meko-system`, the UI's nginx sends
`Host: meko-api-server-service.meko-system.svc.cluster.local:80`, because the
`proxy_set_header Host` lines use `upstreams.api` and `upstreams.inference` raw
(`templates/meko-ui/nginx-configmap.yaml:164`) while the `upstream` address goes
through the namespace-rewriting `meko.upstream` helper (`templates/_helpers.tpl:325`).
The api-server reads `X-Forwarded-Host` before `Host` (`oauth_handler.go:304`,
`environment_install_hint.go:509`), and nginx sets that to the browser's host, so
nothing reads the wrong value. If running stacks shows a chart change is needed
for another cluster or for stacks side by side, it is made on its own branch with a
`helm unittest` case. One values change is already known: a null `nodeSelector` on the rag workers, which
the framework sets per stack.

### Dependencies meko-build does not build

Two pieces every stack runs come from outside `meko-build`:

- **`meko-mem0`, a Python package, not an image.** A fork of the open-source mem0
  library in the private repo `yugabyte/meko-mem0`, line `meko-mem0-2.0`, published to
  public PyPI (2.0.14.4 on 2026-09-29). The mcp-server pins it
  (`meko-mem0>=2.0.14.4,<3`, locked in its `uv.lock`), so it ships inside the
  `meko-mcp-server` image. A mem0 change reaches a stack only after an mcp-server PR
  bumps the lock and `meko-build` rebuilds the mcp-server. The repo's `cd.yml`
  publishes on a GitHub release tagged `v*`, but its newest tag is `v1.0.11`, so the
  2.0.x uploads came some other way. That process is unverified.
  - **The schema is copied by hand.** The chart sets no `MEM0_CREATE_TABLES`, so
    mem0 creates no tables. `api_server` migration `data/000004_mem0_entity_store`
    carries a copy of the DDL that mem0's `PGVector.create_col()` emits, and its
    comment says a later mem0 change must be followed by hand. A mem0 bump that
    changes that DDL breaks memory tools on every BYOC stack, with every unit test
    green.
- **The database image.** `ybdb-meko-release`, a YugabyteDB build with MAGE and
  `pg_dist_rag`, is built by the YugabyteDB side, not by meko CI, and published to
  `quay.io/yugabyte/meko-yugabytedb` when its tag moves (current
  `2026.1.0.0-b1-meko-2`). #1319 records that the `Database` field in
  `dataplane_release.go` is never read and has drifted from the chart.

The framework treats both as inputs it records rather than builds. Every result
records the mem0 version installed in the mcp-server pod and the database image
digest. `functional/mem0_schema_matches` installs a second stack with
`MEM0_CREATE_TABLES=true` and compares mem0's own tables with the ones the migration
made, so a mem0 bump that changes the DDL fails before it ships.

### Where meko calls an LLM

meko runs no model of its own. Every LLM call goes over the internet to OpenAI or AWS
Bedrock with a key from the environment:

| Caller | Calls | For |
|---|---|---|
| mcp-server, `src/mem0_client.py` | chat model and embeddings, OpenAI or Bedrock | `memory_add` asks the model which facts to keep, then embeds them. `memory_search` embeds the query |
| rag-agent, `embeddings/embed.py` (`langchain_openai.OpenAIEmbeddings`) | embeddings | knowledge-base chunks and queries |
| inference-gateway, `llm.py` (`ChatOpenAI` or `ChatBedrockConverse`) | chat model, Bedrock Nova by default | `/chat` replies and conversation summaries |
| api-server, `internal/llm/client.go` (`openai-go`) | chat model and embeddings | server-side prompts and embeddings |

Without a key the stack still starts. Memory extraction and RAG then silently do
nothing.

All OpenAI callers use an official OpenAI SDK, and those SDKs read `OPENAI_BASE_URL` from
the environment. The Bedrock callers use boto3, which reads
`AWS_ENDPOINT_URL_BEDROCK_RUNTIME` from boto3 1.28.57 on. So two env vars point every
call at a mock `mekoqa` runs in the namespace, with no product change, and a stack
makes no call to OpenAI or AWS at all. Each chart component has a `config:` map that
becomes its env. Spike S2 confirms that the extra keys pass through on every
component and that each image's boto3 is new enough.

## What a month of meko bugs asks for

Source: the issues and merged PRs in `yugabyte/meko` touching `meko/` and the issues in
`yugabyte/meko-mcp-server`, 2026-09-01 to 2026-10-02. That is 10 meko issues, 16
mcp-server issues and about 95 merged PRs. Grouped by the failure:

| Class | Cases | What a test needs to catch it |
|---|---|---|
| Authorization | #1459 a denied datapack-access check wrote the 404 and then ran the query anyway, so any account could read or change another's datapack by id (HackerOne). #1562 an authorization hole on `invite-action`. #1659 a Viewer can "Keep as memory". #1485 the admin console accepted any customer's token | several accounts with different roles on one datapack (owner, maintainer, contributor, viewer, stranger), every route tried by every role, asserting the status and that nothing changed |
| Migrations and rollout | #1064 two migrations claimed one version and no pod started on any environment. #1516 a multi-statement migration failed on YugabyteDB while 3 api-server replicas still served heartbeats. #1381 merged, reverted the same day as #1393 | fresh install on YugabyteDB, never Postgres. Upgrade from the previous release with data in it, with 3 api-server replicas, while traffic runs |
| Auth-disabled BYOC mode | #1373 `provision-status` answered 401 with Cognito off, and the UI logged the user out to a login page that 404s. #1546 the gateway's MCP endpoint and CORS were left unfilled. #1372 the UI ingress claimed a shared hostname | the BYOC preset with Cognito off as the default stack, and the calls the UI makes on load, checked for 401 |
| PAT route coverage | #1285 KB document delete was JWT-only, so the MCP tool got 401. #1562 the share routes the same | every route an MCP tool or agent calls, called with a PAT |
| Wire contracts between components | #1353 the gateway sent `memory_add` a dict where the tool takes a JSON string, and no `conversation_id`. mcp-server #390 `datapack_list` rejects a call without `conversation_id` | real images of every component talking to each other, with the gateway's own routes exercised |
| Data correctness | #1553 `conversation_list` shows the first title after `conversation_update`. mcp-server #355 `context_search` drops promoted memories that `knowledgebase_search` returns. #1316 the UI reads the fused `memory_search` score as cosine. mcp-server #332 `datapack_update` cannot repair a datapack with no Langfuse project | round trips that compare list with get, and search results compared across tools. #355 and #1316 turn on embedding distances, so the test has to set the distances itself |
| Speed at real data size | #1236 the account-usage query does a full scan of langfuse and returned 504 in production. #1540 and #1548 the admin KB panel timed out | read APIs timed against a stack seeded with production-sized langfuse and conversation data |
| Unbounded growth | mcp-server #368, #369, #370: one unbounded langfuse trace per conversation, and full payloads copied into tool spans | a long conversation in a soak, and the trace size checked |
| Client-side | mcp-server capture hooks and installer: #331, #337, #356, #357, #358, #377, #389 | not a server environment. Out of scope here |

Three observations change the plan:

- **None of these bugs needs a real model.** Authorization, migrations, PAT routes,
  wire contracts and BYOC mode are plumbing. #355 and #1316 need chosen embedding
  distances, which a mock gives exactly and a real model only gives roughly.
- **Several accounts are needed from the first regression run, not later.**
  Authorization is the largest class, and four of its cases are security reports.
- **The committed `swagger.yaml` cannot be the client's source.** #1199: it is missing
  58 paths and documents a removed parameter on 19.

## Decisions

1. **Copy the amp framework into `meko/test/functional/`, do not share a module yet.**
   The amp engine (`core`, `sched`, `report`, `exec`, `chaos`, `diag`, `artifact`,
   `baseline`, `env/fake`, `env/registry`) is product-agnostic in intent, but `core`
   still carries amp words: shapes named after the control plane, `conndrop`
   attestation for endpoints, `LockEndpointFleetUpgrade`. Extracting a shared module
   now means editing a framework that runs amp's daily lanes, to serve a framework
   that does not run yet. Copy, adapt, and extract a shared `qa/` module at the repo
   root (like `ycp/` and `foundation/`) once both frameworks have settled. Until then a
   fix to a copied engine file is ported by hand, and the PR says so.
2. **One Go module, one binary.** Module `github.com/yugabyte/meko/meko/test/functional`,
   binary `mekoqa`, Go 1.25, same dependency set as ampqa (pgx, cobra, yaml.v3,
   x/crypto). It imports no product code. The api-server client is hand-written for the
   routes the tests call, because the committed `swagger.yaml` is stale (#1199). Once
   #1199 is fixed, the client is generated from a vendored copy with a `sync-spec.sh`,
   the way `ycp/internal/ampclient/` does it.
3. **The default stack is the BYOC chart, one namespace per stack.** It is the
   cheapest complete meko, it needs no cluster-scoped objects, and it is the topology
   customers install, and it runs with Cognito off, which is where #1373 lived. Two
   settings differ from a bare BYOC install: the api-server runs 3 replicas (#1516 only
   fails with replicas serving during a migration), and dex runs in the namespace with
   the api-server on `--auth-provider oidc`, so tests log in as separate accounts
   (the authorization class). Tests that check the auth-disabled path itself get a
   stack without dex. A run builds one stack (`mekoqa-st-<id>`) and gives each
   `regression` test its own accounts and datapacks on it. `stress` and `perf` tests get a dedicated
   stack each (`mekoqa-ds-<id>`). The hosted topology comes later, because its CRD and
   ClusterRoles are cluster-scoped and two runs on one cluster would fight over them.
4. **Every LLM call goes to `llmmock`, and nothing reaches OpenAI or Bedrock.**
   `llmmock` runs in the stack's namespace and speaks both the OpenAI API
   (`/v1/embeddings`, `/v1/chat/completions`) and the Bedrock Runtime API
   (`Converse`, `InvokeModel`). Every component points at it through
   `OPENAI_BASE_URL` and `AWS_ENDPOINT_URL_BEDROCK_RUNTIME`, with dummy keys. A run then
   needs no LLM credentials, costs nothing per call, and gives the same answers on
   every run. A test controls three things:
   - **Embeddings.** A fixture maps text to a vector the test chose, so a test sets the
     exact distance between a query, a memory and a KB chunk (#355, #1316). Text with
     no fixture gets a token-hashing vector: deterministic, and texts that share words
     are close, so plain search tests need no fixtures.
   - **Chat replies.** A script per test: the facts mem0's extraction prompt should
     return, the tool call `/chat` should make, a summary. Without a script, built-in
     answers cover the prompts the stack always sends (mem0 extraction returns the
     input's sentences as facts, its update prompt returns ADD).
   - **Faults.** Latency, 429 with `Retry-After`, 500, a timeout, malformed JSON, a
     truncated stream, per route. Last month had no bug here, and no test today checks
     what meko does when its provider misbehaves.

   `llmmock` records every call, so a test can assert that the call happened and what
   it carried. The `quality` tier, the one place a real model would answer, is
   optional and not scheduled (see the test catalog).
5. **One portal per product, copied.** `meko-report-service` is a copy of
   `amp-report-service` with its own database, URL and Jenkins jobs. A single portal
   with a `product` key would save one VM, and would also put a meko schema change in
   the path of amp's daily runs. Copy now, and keep the CONTRACT shapes identical so
   the `yb` CLI and the MCP server can serve both from one code path.
6. **Pipelines mirror `jenkins/amp/` one to one.** Same agent image, same pool, same
   parameter contract (`TAGS`, `TEST`, `TESTR`, `IMAGE_VERSION`, `BUILD_FROM_SOURCE`,
   `OPTIONS`, `RUN_ID`, `REPORT`, `PORTAL_URL`), same `buildWithParameters` trigger.
   The build job is the existing `meko-build`. No new build pipeline.
7. **Share cluster capacity with ampqa.** Both frameworks run on `itest-dev`. mekoqa
   takes slots from the same `default/ampqa-cluster-slots` ConfigMap, so a meko stress
   run and an amp stress run cannot overcommit the cluster between them.
8. **meko's own images come from `quay.io/yugabyte`, published by `meko-build`.** That
   is the process: a release, or a custom build of a branch, and nothing else. A stack
   never runs a meko component from ECR or a personal registry, and `mekoqa` refuses a
   `meko-*` image from anywhere else as a usage error before it provisions anything.
   Public third-party images stay on their public registries and need no copy:
   `docker.io/valkey/valkey` and `nginx` from the chart, and MinIO and dex from the
   framework. `functional/chart_render` fails if a `meko-*` image in the rendered chart
   is not on `quay.io/yugabyte`. The test namespace holds one pull secret, for Quay.

## Where everything lives

| Repo, branch | Path | What |
|---|---|---|
| `yugabyte/meko`, `main` | `meko/test/functional/PLAN.md` | this plan |
| same | `meko/test/functional/README.md` and `docs/` | how the framework works |
| same | `meko/test/functional/framework/` | engine, apps, env providers, CLI (`cmd/mekoqa`) |
| same | `meko/test/functional/suites/` | `functional`, `stress`, `perf` |
| same | `.claude/skills/meko-report-service/` | agent skill for the portal, a copy of `amp-report-service` |
| `yugabyte/meko-devops`, `main` | `jenkins/meko-qa/README.md` | the portal to Jenkins to framework contract |
| same | `jenkins/meko-qa/pipelines/pipeline_meko_{run,deploy_test,catalog,reap}.groovy` | the jobs |
| same | `jenkins/meko-qa/scripts/` | `deploy-meko-test.sh`, `collect-catalog.sh`, `reap-mekoqa.sh` |
| `yugabyte/qa-services`, `main` | `meko-report-service/README.md` | the portal: what it is and how it differs from amp's |
| same | `meko-report-service/{backend,frontend,qd.conf,docker-compose.yaml}` | the copied service |
| same | `yb-cli/` | `yb meko ...`, the same commands as `yb amp ...` |

Jenkins jobs go in the existing folder `users/shubin/meko-devops/MEKO/`, next to
`meko-build`: `meko-run-tests`, `meko-deploy-test`, `meko-collect-catalog`,
`meko-cleaner`.

## Framework: what is copied, adapted, dropped and new

Counts are Go lines, tests included, in `amp/test/functional/framework` at `8bc5622a4`.

**Copied with a module-path rename and nothing else:** `sched` (2.0k), `report` and its five sinks (3.7k), `exec`, `exec/k8s`, `exec/ssh` (3.3k), `artifact` (1.5k), `baseline` (0.7k), `kubemeta`, `diag` (0.7k), `chaos` (1.9k), `task`, `env/fake`, `env/registry`, and the DB apps a stress test needs for its backing: `apps/{pg,minio,metrics,clientpod,netshape,yba,ybastack,ybdeploy,ybk8s,ybpod,yugabyte,ybadmin,cloud}`.

**Adapted:**

- `core` (8.8k): keep `Test`, `Registry`, `T`, steps, results, status taxonomy, seeds,
  secrets, masking, retries, timeouts, `VersionSpec`. Replace the `Shape` values with
  `SingleStack` (the run's shared stack), `DedicatedStack` and `Remote`. Drop
  `conndrop.go`, the endpoint fleet lock and `servermetrics.go` (amp's endpoint
  metrics). Rename locks to meko's: `LockProvisionerCRD` for the hosted topology.
- `configs/default.yaml` (3.1k lines): rewrite. Keep `sched`, `report`, `chaos`,
  `secrets`, `timeouts`, `apps.yba`, `apps.cloud`. Replace `env.ampstack` and
  `env.sharedk8s` with `env.mekostack` and `env.k8s`. `core/defaults_test.go` keeps
  every default documented.
- `env/reap`: the same sweeper, keyed on `mekoqa-managed-by` labels and `mekoqa-*`
  namespaces.
- `cmd/ampqa` becomes `cmd/mekoqa` with the same command tree: `run`, `collect`,
  `describe`, `manifest`, `task`, `env`, `report`, `version`.

**Dropped:** `apps/{ampstack,controller,pageserver,history,benchbase,pgbench,sysbench,sqlancer,workloadgen,sampleapp}`, `env/{sharedk8s,exclusivek8s,universe,vm}`, every amp suite.

**New:**

| Package | Does |
|---|---|
| `apps/mekostack` | the stack App: namespace, Quay pull secret, `helm upgrade --install` of `meko/deploy/helm/meko` with `values-gcp.yaml`, `values-dev.yaml`, `values-byoc.yaml` and a generated values file (image tags, `dataPlaneSecrets`, MinIO S3, `OPENAI_BASE_URL` and `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` pointing at `llmmock`, 3 api-server replicas, OIDC), then waits for every Deployment and StatefulSet and for `/api/v1/health`. Teardown collects logs first, then deletes the release and namespace. Reached the way `ampstack` reaches its controller: cluster-internal address over the tailnet, port-forward as the fallback |
| `apps/mekoapi` | REST client, hand-written for the routes tests call until #1199 makes the spec usable |
| `apps/mcpclient` | MCP client: `initialize`, `tools/list`, `tools/call` over streamable HTTP with a PAT bearer. Stdlib only. Small enough that a library is not worth the dependency |
| `apps/llmmock` | deploys the OpenAI and Bedrock mock, and gives the test a handle to set embedding fixtures, chat scripts and faults, and to read back every call |
| `apps/oidc` | deploys dex in the stack's namespace with static users, so a test logs in as distinct accounts. Needed by every authorization test |
| `apps/langfuse` | reads traces back through langfuse's public API |
| `apps/mekoload` | an in-cluster load driver: pods running `meko-load` from qa-services (`meko-load/`), because a coordinator on a Jenkins agent cannot generate honest MCP load across the tailnet. HTTP-controllable, the way qa-services `horde` is for YSQL |
| `env/k8s` | the provider: one run-scoped stack for `SingleStack` tests, one stack per `DedicatedStack` test, `--env-mode build|keep|attach`, standing envs in `~/.mekoqa/envs.json` |
| `env/remote` | attach to an existing deployment by URL (a fleet portal, hosted dev), read-only tests only |

The database under a stack is built by a provider from a `universe.Spec`: `yugabyted`
(default) or `yba`, with the chart's bundled subchart as an explicit `chart` option.
docs/ARCHITECTURE.md, "The database", has the details.

## Test catalog

Every test registers with a tier tag. `nightly` is a superset, as in ampqa.
The names below are the first cut, and `mekoqa collect --tag <tier>` will be the
source of truth once they exist.

### `regression`: the gate, one shared stack, datapack per test

| Test | Asserts |
|---|---|
| `functional/chart_render` | `helm template` with the presets and the stack's generated values renders, every `meko-*` image in it is on `quay.io/yugabyte`, and `helm unittest` passes. No cluster |
| `functional/stack_health` | every component Ready, `/api/v1/health` 200, migrations applied (a real request, not only health: a missed migration answers health and fails everything else), `mage` and `pg_dist_rag` extensions present |
| `functional/access_matrix` | owner, maintainer, contributor, viewer and a stranger on one datapack. Every datapack route, each role, over a session token and over a PAT: the status code is the expected one, and a refused call changed nothing (#1459, #1562, #1659) |
| `functional/pat_route_coverage` | every route an MCP tool calls answers a PAT, not 401 (#1285, #1562) |
| `functional/byoc_auth_disabled` | a stack with no IdP: the calls the UI makes on load and on a datapack page, none answers 401, `/auth/config` says `none` (#1373) |
| `functional/mem0_schema_matches` | the tables the `api_server` migration made for mem0 match what the installed `meko-mem0` creates itself with `MEM0_CREATE_TABLES=true` |
| `functional/migrations_fresh_yb` | a fresh install migrates on YugabyteDB, the migration set has no duplicate version, and a second api-server start is a no-op (#1064, #1516) |
| `functional/datapack_lifecycle` | create, list, describe, status, update, delete. The `account_id_<name>` database exists after create and is gone after delete |
| `functional/agent_lifecycle` | agent CRUD, attach to and detach from a datapack |
| `functional/pat_auth` | mint a PAT, MCP accepts it, a revoked PAT is rejected, an expired one too |
| `functional/mcp_tools_contract` | `tools/list` matches the tool names and input schemas the image ships |
| `functional/memory_lifecycle` (built) | `memory_add`, `memory_search` ranks the match first, `memory_get_by_id`, `memory_get_all`, the REST memories page, filter and count, `memory_update` re-embeds, a viewer reads but cannot delete, a stranger learns nothing, deletion over MCP and REST and `memory_delete_all` |
| `functional/conversation_lifecycle` (built) | create, add message, get, list over MCP and REST, the observe detail and trace, rename shows everywhere (#1553), conversation search, the opt-out flag, malformed ids refused cleanly, a stranger refused, delete |
| `functional/search_consistency` | a promoted memory and a KB chunk on one topic: `context_search` returns what `memory_search` and `knowledgebase_search` return on their own (mcp-server #355). Embedding fixtures put the chunk past the KB distance floor and the memory inside it |
| `functional/llm_provider_faults` | `llmmock` answers 429, 500, timeouts and malformed JSON on embeddings and chat. `memory_add`, KB ingest and `/chat` each return an error the caller can read, nothing is half-written, and the stack works again when the faults stop |
| `functional/kb_ingest_text` | presigned upload to MinIO, `upload-complete`, document status reaches indexed, `knowledgebase_search` returns its chunk |
| `functional/kb_ingest_pdf` | the same through the CPU pdf worker |
| `functional/kb_document_lifecycle` (built) | the presigned and the direct upload both index, the documents list, counts, summary, activity log and status agree, search finds both, the download URL returns the bytes, a viewer searches but cannot delete, a stranger sees nothing, deletion over REST and MCP, a pending upload until discarded |
| `functional/context_and_chat_grounding` (built) | `context_search` returns a memory, a document and a conversation turn from their own sources, `force_source` narrows, nothing crosses datapacks, the search shows in token savings, `/chat` puts the memory in the model's prompt and keeps a resumed conversation |
| `functional/artifacts_roundtrip` (built) | a database-stored and an S3-stored artifact round-trip over MCP and REST, the download URL, idempotent put, viewer and stranger, the size limit |
| `functional/datapack_sharing` | share, invite, accept, revoke |
| `functional/inference_chat` | `/chat` with a scripted completion that calls an MCP tool, and the tool result lands in the reply. `/memory/add` and `/index-conversation` called directly (#1353) |
| `functional/langfuse_trace` | a tool call produces a trace in the datapack's langfuse project |
| `functional/restart_durability` | api-server, mcp-server and tserver pods restarted, every datapack, memory and chunk still there |
| `functional/tenant_isolation` | needs `apps/oidc`: user A sees none of B's datapacks over REST or MCP, and a guessed id answers 404 |
| `functional/cli_smoke` | `meko datapack create/list/describe/delete` against the stack |
| `functional/meko_upgrade` | a stack on release N-1 with data and 3 api-server replicas, `helm upgrade` to the candidate while clients run, data intact and every tool answers (#1516) |
| `functional/meko_mixed_versions` | old and new api-server pods against one database after the new migration ran, old pods keep serving (the AGENTS.md upgrade-safety rule, tested) |
| `functional/memory_promote_flow` (built) | learnings promoted over REST and MCP reach the promoted list and the knowledge-base counts, keep-as-memory, delete a promoted memory, bad input refused cleanly, a stranger cannot promote |
| `functional/projects_and_placement` (built) | projects from a theme, folders, filing, moving and unfiling datapacks, rename, a viewer sees the owner's project, a stranger cannot, deleting a project deletes the datapacks filed in it |
| `functional/datapack_delete_cascade` (built) | a datapack with memories, a conversation, a document, an artifact, a member, a pending invite and a placement: after delete no view and no table row names it, and its name is free |
| `functional/account_self_service` (built) | forgot password, change password, profile, onboarding, email change with its code, usage, nothing without a login |
| `functional/free_tier_limits` (built) | datapack creations count towards the free cap, the create past it is refused, an oversized file is refused, a paid tier lifts the cap |
| `functional/hook_capture` (built) | the published meko-skills Claude Code hooks capture a session into one conversation, a turn becomes a memory, the next session preloads it, an outage queues and delivers once |
| `functional/reconcile_agent` | the `ycp/test/local/meko-reconcile.sh` flow: declared datapacks converge, a withdrawn one is collected, a hand-made one survives |

### `stress`: a dedicated stack per test

| Test | Does |
|---|---|
| `stress/datapack_churn_soak` (built) | create and delete datapacks concurrently for an hour, then check nothing leaked: databases, langfuse projects, S3 prefixes, `dist_rag` sources |
| `stress/mcp_conversations_soak` (built) | N MCP clients on memory and search at a fixed rate, error budget and p99 held for the whole soak |
| `stress/kb_ingest_soak` | a large document set ingesting while queries run, the queue drains, no chunk lost or duplicated |
| `stress/kb_ingest_with_worker_kills` (built) | the text rag worker deleted mid-ingest under continuous uploads: every document ends indexed within the task lease, none FAILED, each searchable |
| `stress/churn_with_api_pod_kills` (built) | api-server pods deleted every interval under REST load, recovery within a bound |
| `stress/recall_under_mcp_pod_kills` (built) | recall and captured sessions on seeded datapacks while the MCP server is deleted every interval: recovery within a bound, a quiet error rate between kills, every seeded fact still found |
| `stress/dependency_faults_under_load` (built) | every workload at once while Langfuse is frozen, Valkey, api-server, MCP, gateway and database pods are deleted and the LLM is slow then failing, in turn: recovery after each fault, a quiet error rate between them, invariants, survivors and cleanup |
| `stress/db_nemesis` | on a `yba` RF3 backing: tserver kill and network partition under load |
| `stress/db_maintenance` | rolling restart, gflag change and YugabyteDB upgrade of the backing under load |
| `stress/rolling_upgrade_under_load` | `helm upgrade` N-1 to N while clients run |
| `stress/multitenant_load` | many OIDC users, each with datapacks, mixed load |
| `stress/install_interrupted` | the langfuse pod killed during `metadata-schema-init`, and the api-server pod during `ensure-extensions` and migrate: a re-run converges, and `_meko_schema_init_done` is what makes langfuse's retry safe |
| `stress/long_conversation` | one conversation grown for hours, the langfuse trace and its spans stay under a size bound (mcp-server #368, #369, #370) |

### `perf`: dedicated, pinned stacks, gated by the portal

Same method as ampqa's `suites/perf`: the candidate and the previous release run side
by side on identical stacks (the live arm), throughput at saturation and latency at a
fixed fraction of it, and the portal's `POST /perf/gate` decides when the arm is
missing.

| Test | Measures |
|---|---|
| `perf/api_latency`, `perf/mcp_latency` (built) | closed-loop concurrency ladder, p50, p95, p99 and throughput per op as datapoints |
| `perf/mcp_tool_latency` | p50, p95, p99 per tool at a fixed rate, and throughput at saturation |
| `perf/recall_latency` (built) | `memory_search`, `context_search`, `knowledgebase_search`, `datapack_describe` and the memories pages against the seeded volume of a datapack |
| `perf/write_path_latency` (built) | `conversation_add_message` and the time until an extracted fact is searchable, with the LLM mock as slow as a model |
| `perf/kb_ingest_throughput` (built) | upload latency, time from upload to indexed, documents per second, by concurrent uploaders |
| `perf/ops_latency` | datapack create and delete, PAT mint, time to first successful tool call |
| `perf/chat_latency` (built) | `/chat` end to end and every processing step the gateway times, with the LLM mock as slow as a model |
| `perf/ui_read_latency` (built) | the read routes the web app calls, on datapacks seeded with memories, conversations and documents (#1236, #1540, #1548) |
| `perf/idle_footprint` | CPU and memory of an idle stack |

### `quality`: real model, optional, not scheduled

The one tier that would let a real model answer: `llmmock` forwards to OpenAI instead
of answering itself. It would wrap the evals in `yugabyte/meko-mcp-server/e2e`
(`retrieval-eval`, `extraction-probe`, `audit-eval`) and post recall and precision as
perf datapoints. It needs `secrets.openai_api_key`, skips without it, and runs only
when someone asks for it. Whether to build it at all is decided in P8.

## Pipelines

| Job | Pipeline | Does |
|---|---|---|
| `meko-build` (exists) | `meko/jenkins/pipelines/pipeline_meko_build.groovy` | release and custom images. Gains one stage: report the release and the build to the meko portal, as `amp-build` does |
| `meko-run-tests` | `pipeline_meko_run.groovy` | GKE attach, optional build-from-source through `meko-build`, `mekoqa run --portal`, JUnit and `report.html` archived |
| `meko-deploy-test` | `pipeline_meko_deploy_test.groovy` | deploy, update or delete a standing meko stack (a fleet portal) in a namespace, `values-byoc.yaml` plus a backing |
| `meko-collect-catalog` | `pipeline_meko_catalog.groovy` | hourly: branches, test manifests, quay tags for the `meko-*` repos, release tags `meko-*`, GitHub checks from `meko-pr-build.yml` |
| `meko-cleaner` | `pipeline_meko_reap.groovy` | every 3h: delete `mekoqa-*` namespaces a killed run left |

The daily cycle, Pacific time, before amp's evening runs so the two never share the cluster:

| Time | What |
|---|---|
| 14:00 | `meko-build` |
| 15:00 | schedule `daily-regression`: tag `regression`, `latest_release`, parallel 4 |
| 16:00 | schedule `daily-perf`: tag `perf`, parallel 1 |
| 17:00 | schedule `daily-stress`: tag `stress`, parallel 2 |

## Portal

`meko-report-service` is `amp-report-service` with the amp names replaced. The data
model, CONTRACT shapes, files service, MCP server and frontend stay as they are. What
changes is listed in `meko-report-service/README.md`. The short version:

- quay repos in the catalog and in `latest_release`: `meko-api-server`,
  `meko-mcp-server`, `meko-ui`, `meko-inference-gateway` as the release core, plus
  `meko-rag-agent`, `meko-langfuse-web`, `meko-langfuse-worker`, `meko-provisioner`,
  `meko-yugabytedb`, `ycp-reconciler-agent` collected for autocomplete.
- release tag prefix `meko-`, versions `0.YYMMDD.PATCH`, images `<version>-<sha>`.
- Jenkins jobs under `users/shubin/meko-devops/MEKO/`.
- the status page's unit and integration cells read GitHub checks from
  `meko-pr-build.yml`.
- known issues point at `yugabyte/meko` with meko labels.
- LRU and the amp-dev promotion flow are out of scope for the first release. The
  pages stay, empty.

## Phases

Each phase ends on something that can be run and seen. A phase is one branch and one
PR per repo it touches.

**P0, spikes (no product code).** Each answers one question before code depends on it.

- S1: authentication. Answered on paper: with no IdP, `DevAuth` covers the datapack
  routes and `POST /me/meko-tokens` (`router.go:614-621`, `:1046`), and the
  mcp-server validates PATs through `/internal/meko-tokens/validate`. Cognito is not
  needed anywhere. With dex, each account logs in over OIDC and mints its own PAT.
  Confirm both on the first stack.
- S2: the LLM redirect. Every OpenAI caller uses an official SDK that reads
  `OPENAI_BASE_URL`, and every Bedrock caller uses boto3, which reads
  `AWS_ENDPOINT_URL_BEDROCK_RUNTIME` (see "Where meko calls an LLM"). Left to confirm:
  each component's `config:` map passes the extra keys into its env, each image's
  boto3 is 1.28.57 or later, and mem0 accepts the model name the mock reports. Then
  capture the exact prompts mem0 and the gateway send, which the mock's built-in
  answers have to recognise.
- S3: `values-byoc.yaml` with MinIO-mode S3 settings. Which keys, and does the
  presigned URL work from the coordinator over the tailnet? The rag workers read
  uploads with a bare `boto3.client("s3")` (`rag_pipeline/partition_chunk_pipeline.py`),
  so they reach MinIO through `AWS_ENDPOINT_URL_S3` and need path-style addressing
  through an AWS config file, or MinIO on a virtual-host domain.
- S4: where the portal runs. A new VM `meko-report-service` with
  `meko-qa.dev.yugabyte.com`, or a second qqd project on the amp portal's VM. Needs a
  DNS name either way.
- S5: CPU and memory of one BYOC stack on `itest-dev`, which sets
  `sched.capacity` and the slot cost of a dedicated stack.

**P1, framework skeleton.** Copy the engine, rename, rewrite `default.yaml`, port the
`canary` suite. Done when `go test -race ./...` is green, `mekoqa run --suite canary
--fake-env` shows every status, and `mekoqa manifest` lists the commands.

**P2, the stack.** `apps/mekostack`, `apps/mekoapi`, `apps/mcpclient`, `apps/llmmock`,
`apps/oidc`, `env/k8s`, a kept stack. Done when `mekoqa run --test functional/stack_health
--env-mode keep` stands a stack up on `itest-dev`, `functional/stack_health` passes on
it, and a bare run tears everything down.

**P3, the portal.** Copy the service, rename, run it with `docker compose` and
`mockjenkins`, point `mekoqa --portal` at it, then deploy with qqd. Done when a real
run streams into the deployed portal.

**P4, the pipelines.** `meko-run-tests`, `meko-collect-catalog`, `meko-cleaner`, the
portal's trigger wired to them, `meko-build` reporting releases, the three daily
schedules. Done when `daily-regression` fires on its own and its result fills the
status page cell.

**P5, regression.** The `functional` table above, landing in the order last month's bugs
suggest: `access_matrix`, `migrations_fresh_yb`, `meko_upgrade`, `pat_route_coverage`,
`byoc_auth_disabled`, `inference_chat`, then the round trips and the rest.

**P6, stress.** Dedicated stacks, `apps/mekoload`, the `yba` backing, chaos. Done
when `daily-stress` runs green twice in a row.

**P7, perf.** The live arm, datapoints with `load_signature`, gating through the
portal. Done when one release's numbers sit on the Perf trends page next to the
previous one's.

**P8, later.** `quality`, the hosted topology with the provisioner, `meko-deploy-test`
and fleet portals, a YCP control-plane suite, UI checks, extraction of the shared
engine module.

## Risks

- **The engine copy drifts.** Every ampqa engine fix after the copy is a manual port.
  The extraction in P8 is what ends that, and putting it off past the point where both
  frameworks are stable makes it harder.
- **The mock is only as good as its copy of the provider.** A scripted fact list tests
  what meko does with mem0's answer, not whether a real model gives a good one. A
  prompt change in mem0 or the gateway can also make the built-in answers stop
  matching. Then the mock answers with its default, and a test that expected
  extraction fails loudly instead of passing on the wrong path. Pin the prompts the
  mock recognises in its own tests, and bump them with the mcp-server.
- **The mock's wire format can drift from the real APIs.** OpenAI and Bedrock add
  fields, and the SDKs in the images move. Pin the mock's responses to recorded real
  responses, and check them again when an image's SDK version moves.
- **The admin console needs Cognito.** #1485 lived in `meko_admin`, which verifies
  tokens against a dedicated admin Cognito pool. A BYOC stack has no such pool, so
  that class stays untested here. It can only run as a `Remote` test against hosted
  dev with a test admin account.
- **Images built from other repos.** `meko-mcp-server`, `meko-langfuse` and the
  rag-agent change without a meko release. A run records every image's tag and the
  per-repo shas from `build-manifest.txt`, so a result says which mcp-server it ran.
- **Cluster capacity.** `itest-dev` already runs amp's daily lanes. The shared slot
  ConfigMap prevents overcommit, but a slow meko run then delays amp's, and the other
  way round.
