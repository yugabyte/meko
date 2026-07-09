# yb-light endpoint: single-container image + Helm chart

This directory packages a **yb-light endpoint** as a single container and a
Helm chart to run it on Kubernetes. It implements
[issue #83](https://github.com/yugabyte/yb-light/issues/83).

```
deploy/
  up.sh             # one-shot bring-up: namespace + pull secret + YugabyteDB + controller (see below)
  endpoint/
    Dockerfile      # builds the single-container endpoint image
    supervisor.py   # PID-1 (under tini) process manager for the 5 services
  controller/
    Dockerfile      # the controller layer: controller + storage_controller bundled (see deploy/controller/README.md, #94)
    redeploy.sh     # (re-)deploy the controller via the yb-amp-controller chart (assumes ns/YB/pull-secret exist)
  helm/
    yb-light-endpoint/     # Helm chart for an endpoint (Deployment + ConfigMap + Secret + Service)
    yb-amp-controller/   # Helm chart for the controller layer + the MCP server pod (mcp.enabled) + the amp_ui pod (ui.enabled)
```

## One-shot bring-up — `up.sh`

[`up.sh`](up.sh) stands the whole stack up on Kubernetes **from nothing**, then
prints the controller UI URL. It is the from-scratch companion to
[`controller/redeploy.sh`](controller/README.md) (which redeploys only the
controller and assumes its prerequisites already exist). In order, `up.sh`:

1. creates the **namespace**;
2. **(re)mints the docker-registry pull secret** from the `dshubin_bot` bot account
   so the cluster can pull `amp/controller` + `amp/endpoint`; the secret's ECR token
   expires ~12 h, so `up.sh` refreshes it every run (see [Refreshing the pull
   secret](#refreshing-the-pull-secret));
3. deploys a **single-node YugabyteDB** (`yugabyted` StatefulSet + headless Service
   + a PVC) and waits for it to be ready;
4. creates the **`yb_storage` database** (the controller's `db.Connect` targets it
   at startup, so it must exist first);
5. runs `controller/redeploy.sh` to deploy the **amp controller** wired to that YB.

It is idempotent (create-or-apply throughout) and does **not** wipe YugabyteDB
data on re-run (the StatefulSet keeps its PVC).

```sh
# prereqs: kubectl (target cluster), helm, docker, aws (the dshubin_bot creds in
#          the environment — see "ECR access & the pull secret" below)
deploy/up.sh

# common overrides:
NS=amp deploy/up.sh
CONTROLLER_TAG=<tag> ENDPOINT_TAG=<tag> deploy/up.sh        # else: most-recent ECR tag
CONTROLLER_EXPOSE=internal-lb deploy/up.sh                  # controller via internal LB instead of NodePort
ENDPOINT_SERVICE_TYPE=LoadBalancer deploy/up.sh            # per-endpoint LB instead of ClusterIP
ENDPOINT_STORAGE_CLASS=yb-standard deploy/up.sh            # endpoints get a /data PVC (EBS/PD) instead of emptyDir
```

### Exposure

To avoid the slow per-Service cloud LB provisioning, the defaults use no cloud LBs:

- **Controller** (`CONTROLLER_EXPOSE`, default `nodeport`): a NodePort Service
  (`amp-controller-np`) reachable at any node IP (an `ExternalIP` if the nodes have
  one, else an `InternalIP`/VPC address — may need a node-port firewall rule). Other
  modes: `internal-lb` / `external-lb` (GKE LoadBalancer, private / public IP) and
  `none` (in-cluster ClusterIP only). The in-cluster `amp-controller` Service
  (API + the SC port endpoints register against) is always created.
- **Endpoints** (`ENDPOINT_SERVICE_TYPE`, default `ClusterIP`): no per-endpoint
  cloud LB — reach an endpoint's Postgres via its Service DNS in-cluster, or
  `kubectl -n <ns> port-forward svc/<endpoint> 55433:55433` from outside.
  `NodePort` is also supported (the controller records `<nodeIP>:<nodePort>` as the
  endpoint's connection address, shown in the UI; may need a node-port firewall
  rule), as is `LoadBalancer` (uses `ENDPOINT_SERVICE_ANNOTATIONS`).

From outside the GCP VPC (e.g. a laptop or an AWS box), use `kubectl port-forward`
for both. The UI/API is unauthenticated — do not expose it publicly.

The **controller layer** — the `controller` plus the shared `storage_controller`
this endpoint deliberately omits — has its own bundled image under
[`controller/`](controller/README.md) and is deployed via the
[`yb-amp-controller`](helm/yb-amp-controller) Helm chart. That chart also
ships the **MCP server** (a FastMCP wrapper that exposes the controller's REST
API as MCP tools for AI agents; see
[`controller/architecture/ac-mcp.md`](../controller/architecture/ac-mcp.md)) as
its own pod, gated by `mcp.enabled` (default on), and the **[`amp_ui`](../amp_ui/README.md)
web UI** (the React SPA served by nginx, reverse-proxying `/api/` to the
controller) as its own pod, gated by `ui.enabled` (default on).

## ECR access & the pull secret

Images live in ECR at **`454529406029.dkr.ecr.us-west-2.amazonaws.com`**, repos
**`amp/controller`** and **`amp/endpoint`** (full ref
`454529406029.dkr.ecr.us-west-2.amazonaws.com/amp/controller:<tag>`). `up.sh` and
`redeploy.sh` default to these repos.

ECR access — both for pushing images from a dev box / CI and for the cluster's
pull secret — uses a dedicated **`dshubin_bot`** IAM bot account, not a personal
SSO session (which expired mid-deploy in the past). The bot's **static access keys
don't expire**, so you can always authenticate and **refresh** the pull secret
without an SSO round-trip. That matters because the ECR *login password* the pull
secret is built from **is only valid ~12 h** — the secret must be refreshed
periodically (see [Refreshing the pull secret](#refreshing-the-pull-secret) below).

1. **Bot account (one-time, by an admin).** Create the `dshubin_bot` IAM user with
   ECR pull (and push, for publishing images) on the `amp/*` repos and issue it an
   access key.
2. **Provide the bot's credentials to the deploy scripts.** `up.sh` and `redeploy.sh`
   (via [`ecr-creds.sh`](ecr-creds.sh)) resolve AWS credentials in priority order:
   `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` already in the environment → else
   `AWS_BOT_JSON` pointing at a JSON file holding the bot's keys (e.g. the
   `aws iam create-access-key` output for `dshubin_bot`; the scripts parse the access
   key + secret out of it) → else the ambient AWS config (an `aws sso login` session).
   Set `AWS_DEFAULT_REGION` too (used for `get-login-password`; defaults to `us-west-2`).
   ```sh
   # option A — keys directly in the environment:
   export AWS_ACCESS_KEY_ID=<dshubin_bot access key> AWS_SECRET_ACCESS_KEY=<secret>
   # option B — a JSON keys file (e.g. `aws iam create-access-key` output):
   export AWS_BOT_JSON=/path/to/dshubin_bot-keys.json
   export AWS_DEFAULT_REGION=us-west-2
   ```
3. **Log Docker in to ECR** (for `docker push` / `docker pull` from this machine):
   ```sh
   aws ecr get-login-password --region "$AWS_DEFAULT_REGION" \
     | docker login --username AWS --password-stdin 454529406029.dkr.ecr.us-west-2.amazonaws.com
   ```

### Refreshing the pull secret

Each namespace has a docker-registry secret named **`pull-secret`** built from
`aws ecr get-login-password` — whose token **expires ~12 h after it's minted**. So
it isn't "set once and forget": a namespace left idle longer than that will
`ImagePullBackOff` until the secret is refreshed. **Both `up.sh` and `redeploy.sh`
refresh it on every run** (resolving the bot creds via [`ecr-creds.sh`](ecr-creds.sh)),
so a normal (re)deploy always mints a fresh token. Refresh it manually only when a
namespace has sat idle *between* deploys:

To create **or refresh** it in a namespace — idempotent (`apply` updates in place),
run with the `dshubin_bot` creds in the environment:

```sh
kubectl -n <ns> create secret docker-registry pull-secret \
  --docker-server=454529406029.dkr.ecr.us-west-2.amazonaws.com \
  --docker-username=AWS \
  --docker-password="$(aws ecr get-login-password --region "$AWS_DEFAULT_REGION")" \
  --dry-run=client -o yaml | kubectl apply -f -
```

For a long-running dev deployment, run that on a schedule so it never goes stale —
e.g. an in-cluster CronJob (every ~6 h, mounting the `dshubin_bot` keys) that runs
the same command. That's the durable "doesn't time out" answer: the ECR token
can't be made long-lived, so keep re-minting it from the bot account.

## Smoke test — `smoke-test.sh`

[`smoke-test.sh`](smoke-test.sh) validates a **deployed** stack end-to-end. It is
the deployment-side companion to the controller-on-Docker e2e in
[`test/e2e/control_plane`](../test/e2e/control_plane): that suite proves the control
plane on Docker; this one proves an actual Kubernetes deployment, including the
write-ahead task contract (#113) and **real data I/O** through a created endpoint.

It port-forwards the controller API and walks the full async-task lifecycle —
`createProject` / `createEndpoint` / `branch` / `deleteEndpoint` / `deleteProject` /
`deleteBranch` each return `202 + a Task + a Location` header and run to
`status=succeeded` (polled via `GET /tasks/{id}`) — then **connects to the new
endpoint with `psql` and writes + reads actual data**: full CRUD + `CHECKPOINT`
(forcing the writes through safekeeper → pageserver → YugabyteDB) and a readback,
and checks that the recorded connection address routes to a ready Service backend.
It also asserts the contract edges (`GET /tasks` filters + a bad
`status` → `400`, a duplicate name → a *failed* task, deleting a branch with a
child fork → `409`, and `delete_project` cascading). `createUser` /
`createStoragePool` are checked to stay synchronous `201`.

It cleans up everything it creates and exits non-zero on any failure.

```sh
# prereqs: kubectl (pointed at the cluster), curl, jq; a controller deployed in $NS
# (deploy/up.sh) with the endpoint deployer on. psql is NOT needed locally — the
# data-I/O checks exec the psql baked into the endpoint pod.
deploy/smoke-test.sh                 # namespace amp-test
NS=amp deploy/smoke-test.sh          # another namespace
KEEP=1 deploy/smoke-test.sh          # leave the created resources for inspection
```

For *debugging* a failing deployment (an endpoint that won't come up, a mutation
task that fails), see [`../runbooks/controller_runbook.md`](../runbooks/controller_runbook.md).

## What's in the container

One image runs every process that makes up an endpoint, supervised by a small
Python script (`supervisor.py`) started by `tini` as PID 1:

| Service          | Role                                   | Backed by   |
|------------------|----------------------------------------|-------------|
| `endpoint_storage` | LFC pre-warm scratch blobs           | YugabyteDB  |
| `safekeeper`       | WAL                                  | YugabyteDB  |
| `yb-pageserver`    | page service                         | YugabyteDB  |
| `compute_ctl`      | supervises Postgres v18 + `neon` ext | (compute)   |

The supervisor renders each service's config from environment variables,
brings the stack up in order (endpoint_storage → safekeeper → pageserver →
compute), creates the safekeeper timeline, then monitors the children and
restarts them with backoff; if a service exceeds its restart budget the
supervisor exits non-zero so the pod is recreated.

### Why no storage_controller

The `storage_controller` is a **shared, cluster-level** control plane (it owns
the pageserver node registry, generation/attach fencing, and the endpoint
registry in YugabyteDB's `storage_controller` schema). It is **not** part of
the endpoint pod — it lives in the controller layer. The pod talks only to YugabyteDB.

A consequence: the `yb_storage` schema (created by the storage_controller's
diesel migrations) must already exist. In production, the controller provisions it. For
standalone testing, set `DEV_PROVISION_SCHEMA=true` and the supervisor will
create the database and apply the bundled migrations itself.

## Building the image

yb-light is Postgres v18 only. From the **repo root**:

```sh
docker build -t yb-light/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
```

This compiles patched Postgres v18, the Rust services, and the `neon`
extension, so the first build is slow. The `ghcr.io/neondatabase/build-tools`
base image is required (same as the repo-root Dockerfile).

## Configuration contract

The supervisor reads these environment variables (the Helm chart sets them via
a ConfigMap + Secret):

| Variable | Default | Meaning |
|---|---|---|
| `YB_HOST_PORTS` | `127.0.0.1:5433` | CSV of YB tservers `host:port[,...]` |
| `YB_USER` | `yugabyte` | YB role |
| `YB_DBNAME` | `yb_storage` | YB database |
| `YB_SSLMODE` | _(unset)_ | `disable`/`require`/`verify-ca`/`verify-full` |
| `YB_SSLROOTCERT` | _(unset)_ | CA bundle path (for verify modes) |
| `YB_PASSWORD` | _(unset)_ | YB password (from a Secret) |
| `ENDPOINT_PG_VERSION` | `17` | compute Postgres major version |
| `COMPUTE_PORT` | `55433` | compute Postgres port |
| `COMPUTE_PASSWORD` | `cloud_admin` | password for the `cloud_admin` role |
| `SHARED_PRELOAD_LIBRARIES` | `neon` | compute `shared_preload_libraries` |
| `TENANT_ID` / `TIMELINE_ID` | _(unset)_ | serve an existing tenant; unset ⇒ bootstrap a fresh one |
| `DEV_PROVISION_SCHEMA` | `false` | create DB + apply migrations (test only) |

Internal ports (`PAGESERVER_PG_PORT`, `PAGESERVER_HTTP_PORT`,
`PAGESERVER_GRPC_PORT`, `SAFEKEEPER_PG_PORT`, `SAFEKEEPER_HTTP_PORT`,
`ENDPOINT_STORAGE_HTTP_PORT`, `COMPUTE_CTL_HTTP_PORT`) and supervisor knobs
(`SUPERVISOR_RESTART_BACKOFF_SECONDS`, `SUPERVISOR_MAX_RESTARTS`,
`SUPERVISOR_RESTART_WINDOW_SECONDS`) have sensible defaults.

## Running it locally (against a YugabyteDB on the host)

```sh
docker run --rm --name yb-endpoint \
  --network host \
  -e YB_HOST_PORTS=127.0.0.1:5433 \
  -e YB_USER=yugabyte -e YB_PASSWORD=yugabyte \
  -e DEV_PROVISION_SCHEMA=true \
  yb-light/neon-endpoint:dev

# then, from the host:
psql -h 127.0.0.1 -p 55433 -U cloud_admin -d postgres   # password: cloud_admin
```

(`--network host` lets the container reach a host YugabyteDB on `127.0.0.1:5433`
and exposes the compute on the host's `55433`.)

## Helm chart

See [`helm/yb-light-endpoint`](helm/yb-light-endpoint). It renders a
Deployment (stateless — PGDATA is ephemeral, all durable state is in YB), a
ConfigMap carrying the YB connection block + endpoint settings, an optional
Secret for the YB password, and a Service exposing the compute port.

```sh
helm install ep deploy/helm/yb-light-endpoint \
  --set yugabyte.hostPorts=yb-tserver.default.svc:5433 \
  --set yugabyte.password=$YB_PASSWORD \
  --set image.repository=yb-light/neon-endpoint --set image.tag=dev

# unit tests (requires the helm-unittest plugin):
helm unittest deploy/helm/yb-light-endpoint
```
