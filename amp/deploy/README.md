# yb-amp endpoint: single-container image + Helm chart

This directory packages a **yb-amp endpoint** as a single container and a
Helm chart to run it on Kubernetes. It implements
[issue #83](https://github.com/yugabyte/amp/issues/83).

```
deploy/
  up.sh             # one-shot bring-up on ECR: namespace + pull secret + YugabyteDB + controller (see below)
  up-gke.sh         # the GKE/GCR-native sibling of up.sh (no aws/ECR; see "GKE / GCR bring-up")
  ycp-controller-upgrade-test.sh  # upgrade the controller through YCP desired state and assert the fleet survives
  endpoint/
    Dockerfile      # builds the single-container endpoint image; PID 1 is
                    # endpoint-supervisor (compute/endpoint_supervisor)
  controller/
    Dockerfile      # the controller layer: the Go controller (see deploy/controller/README.md, #94)
    redeploy.sh     # (re-)deploy the controller via the yb-amp-controller chart (assumes ns/YB/pull-secret exist)
  helm/
    yb-amp-endpoint/     # Helm chart for an endpoint (Deployment + ConfigMap + Secret + Service)
    yb-amp-controller/   # Helm chart for the controller layer + the MCP server pod (mcp.enabled) + the amp_ui pod (ui.enabled) + the reconcile-agent pod (agent.enabled)
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
   + a PVC) and waits for it to be ready — or, with `TLS=1`, the upstream `yugabyte`
   chart on cert-manager certificates at `yb-tserver-0.yb-tservers.$NS.svc…`
   (`tls/trust-root.sh` then `tls/yugabyte-tls.sh`);
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
ENDPOINT_EXPOSE=internal-lb deploy/up.sh                    # each endpoint gets a private VPC LoadBalancer (mirrors CONTROLLER_EXPOSE)
ENDPOINT_STORAGE_CLASS=yb-standard deploy/up.sh            # endpoints get a /data PVC (EBS/PD) instead of emptyDir
ENDPOINT_NODE_SELECTOR=cloud.google.com/gke-ephemeral-storage-local-ssd=true deploy/up.sh  # endpoints' emptyDir /data on local NVMe SSD
```

**NVMe-ephemeral endpoints.** Each endpoint's `/data` (pgdata, LFC, logs) defaults to
an `emptyDir` — all durable state is in YugabyteDB, so it's throwaway scratch. To put
that scratch on **local NVMe SSD** for speed, leave `ENDPOINT_STORAGE_CLASS` empty
(keep the `emptyDir`) and set `ENDPOINT_NODE_SELECTOR` (a `k=v,k=v` CSV of node labels)
to a node pool created with local-SSD ephemeral storage — on GKE:

```sh
gcloud container node-pools create nvme-ephemeral --cluster <cluster> --region <region> \
  --machine-type n1-standard-8 --ephemeral-storage-local-ssd count=1 \
  --num-nodes 1 --enable-autoscaling --min-nodes 0 --max-nodes 2
# GKE auto-labels those nodes cloud.google.com/gke-ephemeral-storage-local-ssd=true,
# which backs each pod's emptyDir with the local NVMe. Then:
ENDPOINT_NODE_SELECTOR=cloud.google.com/gke-ephemeral-storage-local-ssd=true deploy/up.sh
```

The selector threads through to the controller (`AMP_DEPLOY_NODE_SELECTOR`), which pins
every endpoint pod (`nodeSelector`) onto that pool. NVMe here is **ephemeral** — a
rescheduled pod gets a fresh empty `/data`; that's fine because the durable copy lives
in YugabyteDB. Don't combine it with `ENDPOINT_STORAGE_CLASS` (a PVC would override the
`emptyDir`).

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

The **controller layer** — the `controller`, the cluster control plane this
endpoint deliberately omits — has its own bundled image under
[`controller/`](controller/README.md) and is deployed via the
[`yb-amp-controller`](helm/yb-amp-controller) Helm chart. That chart also
ships the **MCP server** (a FastMCP wrapper that exposes the controller's REST
API as MCP tools for AI agents; see
[`controller/architecture/ac-mcp.md`](../controller/architecture/ac-mcp.md)) as
its own pod, gated by `mcp.enabled` (default on), and the **[`amp_ui`](../amp_ui/README.md)
web UI** (the React SPA served by nginx, reverse-proxying `/api/` to the
controller) as its own pod, gated by `ui.enabled` (default on). It can also
deploy the **reconcile-agent** (the YCP data-plane client that converges the
environment to YCP's desired state; see
[`controller/architecture/ac-ycp.md`](../controller/architecture/ac-ycp.md)) as
its own pod, gated by `agent.enabled` (default off).

## GKE / GCR bring-up — `up-gke.sh`

[`up-gke.sh`](up-gke.sh) is the **Google-Cloud-native** sibling of `up.sh`: it
stands the same stack up on GKE but pulls (and optionally builds/pushes) images
from **GCR / Artifact Registry** instead of ECR — so it needs **no `aws`, no
`dshubin_bot`, and no ~12 h ECR pull-secret refresh**. It runs the same steps
(namespace → YugabyteDB → `yb_storage` → the `yb-amp-controller` Helm chart) and
is likewise idempotent and non-destructive to YugabyteDB data. `FOUNDATION=operator`
swaps the YugabyteDB step for an operator-managed universe — see
[Foundation](#foundation--the-backing-yugabytedb) below.

The two GKE clusters are already kubectl contexts:
`gke_yugabyte_us-west1_itest-dev` and `gke_yugabyte_us-central1_itest-release`.

```sh
# prereqs: kubectl (target GKE cluster), helm, gcloud, docker

# deploy-only (images already in GCR), onto itest-dev:
CLUSTER=gke_yugabyte_us-west1_itest-dev NS=amp TAG=<tag> deploy/up-gke.sh

# build + push all four images to GCR, then deploy, onto itest-release:
CLUSTER=gke_yugabyte_us-central1_itest-release NS=amp BUILD=1 deploy/up-gke.sh

# Artifact Registry instead of classic gcr.io:
REGISTRY=us-west1-docker.pkg.dev/yugabyte/amp BUILD=1 deploy/up-gke.sh
```

**Pull auth** — four modes, selected by `PULL_SECRET` / `GCR_KEY_FILE` /
`PULL_SECRET_DOCKERCONFIGJSON` (checked in that order when `PULL_SECRET` is set):

- **none (default)** — `PULL_SECRET` unset. A same-project GKE cluster pulls from
  GCR via its **node service account**, which needs `roles/artifactregistry.reader`
  (Artifact Registry) or `storage.objectViewer` (classic `gcr.io` bucket).
- **mint from a GCP SA key** — hand it a GCP service-account key and it (re)creates
  the secret:
  ```sh
  PULL_SECRET=gcr-pull-secret GCR_KEY_FILE=./gcr-sa-key.json deploy/up-gke.sh
  ```
- **mint from a docker `config.json`** — hand it a raw docker config.json
  (`{"auths":{...}}`, e.g. a Quay robot cred straight from a CI secret) and it
  (re)creates the `kubernetes.io/dockerconfigjson` secret **in the namespace it just
  created** — nothing needs to exist beforehand, which is what CI uses:
  ```sh
  PULL_SECRET=quay-pull-secret PULL_SECRET_DOCKERCONFIGJSON="$(cat cfg.json)" deploy/up-gke.sh
  ```
- **bring your own** — you already have a pull secret in the namespace; pass its
  name and the script wires it into every pod (and into the endpoints the
  controller deploys) without creating it:
  ```sh
  PULL_SECRET=my-gcr-secret deploy/up-gke.sh
  ```
  (It fails fast if that secret isn't present in the namespace.)

**Building/pushing manually** (what `BUILD=1` automates) — after
`gcloud auth configure-docker <host>`:

```sh
G=gcr.io/yugabyte
make controller-bundle BUILD_TYPE=release
docker build -t $G/amp/controller:<tag> -f deploy/controller/Dockerfile . && docker push $G/amp/controller:<tag>
docker build -t $G/amp/endpoint:<tag>   -f deploy/endpoint/Dockerfile   . && docker push $G/amp/endpoint:<tag>
docker build -t $G/amp/controller-ui:<tag> amp_ui                         && docker push $G/amp/controller-ui:<tag>
docker build -t $G/amp/mcp:<tag> -f controller/mcp/Dockerfile controller/mcp && docker push $G/amp/mcp:<tag>
```

Key overrides: `REGISTRY` (base registry+path, default `gcr.io/yugabyte`), `TAG`
(default `latest`, or a timestamp when `BUILD=1`), `CONTROLLER_EXPOSE`
(`internal-lb` default), `ENDPOINT_SERVICE_TYPE`, `ENDPOINT_STORAGE_CLASS`
(e.g. `standard-rwo` for a PD-backed `/data`), `MCP_ENABLED`, `UI_ENABLED`,
`PULL_SECRET` / `GCR_KEY_FILE` (pull auth, see above), `FOUNDATION` /
`FOUNDATION_SPEC` (see below).

### Foundation — the backing YugabyteDB

`FOUNDATION` selects how the DB the controller keeps its state in is brought up:

| | |
|---|---|
| `yugabyted` (default) | the inlined single-node StatefulSet. RF-1, no rolling upgrade, no backups — fine for a dev or CI stack, and what every deploy path here still uses. |
| `operator` | a real universe managed by the YugabyteDB Kubernetes Operator, via the [`foundation/`](../../foundation) deployer ([architecture](../../foundation/ARCHITECTURE.md)): CRDs, the `yugaware` chart with the operator enabled, a `YBProvider` and `YBUniverse` CR, the tserver RPC Service and `yb_storage`. Needs `go` on PATH; `FOUNDATION_SPEC` points at the spec (default `foundation/examples/amp.yaml`). |

The spec stands up a **3-node RF3** universe on `yb-standard`, placed explicitly
on itest-dev's single zone, with the MAGE gflag seeded at create (it is
start-time on both processes, so gaining it later is a rolling restart). The
`provider` block carries the DB image registry, the per-zone storage class and
the namespace the DB lands in (`kubeNamespace`, defaulted to this one), none of
which the operator's auto-provider can infer. The DB image's pull secret is
YBA's to place: it derives one from its own (`yba.values.image.pullSecret`,
`quay-pull-secret` — what this script mints) and copies it beside the DB pods,
which is the only workable route since the provider CRD carries no pull-secret
content. That robot therefore needs pull access to the DB image repo as well as
the amp and YBA ones.

Under `operator`, `YB_HOST_PORTS` and `DB_URL` come from `foundation connstr`
rather than being duplicated here — it owns the Service name, the port and the
DSN shape. The
single `YB_HOST_PORTS` is the only address handed to compute, and both its lanes
come off it: the YSQL pool uses it as given, and the direct-RPC lane derives the
same host on 9100. It points at a ClusterIP the YugabyteDB chart creates from a
`serviceEndpoints` override in the CR, carrying both client ports. Its name is
YBA's to choose — `<universe>-<suffix>-yb-tserver-conn` — so the deployer finds
it by the `serviceName=yb-tserver-conn,scope=Namespaced` labels rather than
building the name. To point consumers elsewhere — a clusterset
name, an internal LoadBalancer, an external name — supply the address where the consumer is
configured (`redeploy.sh`'s `YB_HOST_PORTS`/`DB_URL`, the install hint's `YB=`), not in the
foundation spec.

The default is deliberately unchanged: a `yugaware` pod alone wants ~4.5 CPU and
tens of minutes to install, so the operator path soaks in its own nightly
([`amp-foundation-gke.yml`](../../.github/workflows/amp-foundation-gke.yml),
`/ci amp_deploy_gke_foundation`) before anything else is pointed at it.

### TLS and certificate authentication — `TLS=1`

`TLS=1 deploy/up-gke.sh` (and `up.sh`) replaces the yugabyted StatefulSet with the
upstream `yugabyte` chart on cert-manager certificates, behind a cluster-wide trust
root. With `FOUNDATION=operator` the same trust root is used but TLS is **not an
input**: both encryption flags are create-time only in the CRD, so the foundation
spec decides and this script reads the shape back from `foundation validate --json`
(an explicit `TLS=0` against an encrypted spec is refused rather than obeyed). The
universe's own certificates come from YBA under its cert-manager type: the CA is
copied from the trust bundle into Secret `YB_CA_SECRET` (default
`amp-ca-bundle-tls`), which the spec's `YBCertificate` must name. `TLS_ISSUER_KIND`
has to stay `ClusterIssuer` — YBA deploys the universe into its own namespace and
cannot reach a namespaced `Issuer`. See
[`../../foundation/examples/amp.yaml`](../../foundation/examples/amp.yaml).

- [`tls/trust-root.sh`](tls/trust-root.sh) installs cert-manager and trust-manager
  when absent (never touching an existing install), bootstraps a root CA and the
  `amp-ca` ClusterIssuer unless `TLS_ISSUER_KIND` / `TLS_ISSUER_NAME` /
  `TLS_ISSUER_GROUP` name one you already have, and distributes the CA as ConfigMap
  `amp-ca-bundle` into every namespace labelled `amp.yugabyte.com/trust=true`.
  It is the only installer of cert-manager: the controller chart declares no
  subchart for it and refuses to render with `tls.enabled` on a cluster that has
  none, naming this script.
- [`tls/yugabyte-tls.sh`](tls/yugabyte-tls.sh) installs the chart (`YB_CHART_VERSION`,
  `YB_STORAGE_CLASS`) with TLS on YSQL and on the tserver RPC port, client
  certificates required on RPC, and an HBA that logs the storage role
  (`YB_STORAGE_ROLE`, default `amp_storage`) in by certificate CN; then creates
  `yb_storage`.
- [`tls/yb-storage-role.sh`](tls/yb-storage-role.sh) creates that role, from either
  lane — the operator one gets its database from the foundation spec and would
  otherwise have no role at all.

The controller then gets a cert-manager certificate of its own (chart values
`tls.*`) and logs in with it, and renders one into every endpoint
(`controller.endpoint.ybTlsMode=verify-full`), which connects as `YB_STORAGE_ROLE`.
Endpoints that predate a TLS change are brought onto it with
`POST /api/v1/endpoint-rerenders`. Renewed certificates are picked up live by the
controller and the endpoints; the database copies its files at pod start, so
`yugabyte-tls.sh` also installs the `yb-cert-rollout` CronJob
(`YB_CERT_ROLLOUT_SCHEDULE`, `YB_CERT_ROLLOUT=0` to skip) that restarts a
StatefulSet whose certificate is newer than its pods.

`TLS_PERMISSIVE=1` (the default) keeps plaintext accepted beside TLS; once
`pg_stat_ssl` shows every amp connection on a certificate, redeploy with
`TLS_PERMISSIVE=0`. Operating it, checking it, and rotating certificates:
[`../runbooks/tls.md`](../runbooks/tls.md); the design:
[`../controller/architecture/ac-endpoint-tls.md`](../controller/architecture/ac-endpoint-tls.md).

### Running in CI (itest-dev)

The [`control-plane GKE (itest-dev)`](../../.github/workflows/amp-control-plane-gke.yml)
workflow runs `up-gke.sh` + [`smoke-test.sh`](#smoke-test--smoke-testsh) against
the **itest-dev** cluster — the "k8s" half of the control-plane deploy integration
(the docker-driver half is [`test/integration/controller`](../test/integration/controller)).

Because a cold run is expensive (~30-min endpoint build + a live GKE deploy) it is
**not** a per-PR check. It runs only: **nightly** on `main` (07:17 UTC, the same
slot as the slow integration suite), on **manual dispatch**, or on a PR you
explicitly opt in by adding a **`/ci amp_deploy_gke`** line to the PR description — the
same on-demand directive mechanism the slow suite uses (see
[`docs/ci-directives.md`](../docs/ci-directives.md)); the line re-runs the deploy on
every push while it is present. Fork PRs are always skipped (no secrets). It:

- builds + pushes the four images to **Quay** (`quay.io/yugabyte/amp-{controller,endpoint,controller-ui,mcp}`),
  tagged **`custom-<short-sha>`**; the ~30-min endpoint build uses a
  registry layer cache (`:buildcache`) so unchanged inputs finish in minutes;
- authenticates to itest-dev with a **static kubeconfig**, mints the Quay pull
  secret in a per-PR namespace (`ci-pr-<n>`), deploys with `CONTROLLER_EXPOSE=none`
  / `ENDPOINT_SERVICE_TYPE=ClusterIP`, runs the smoke test (which port-forwards),
  and **always** tears the namespace down.

A manual dispatch takes two more inputs: `tls` runs the deploy with `TLS=1`, and
`namespace` deploys into a standing namespace instead of `ci-pr-<run>` and skips the
teardown, which is how a branch is rehearsed on itest-dev before it merges.

A superset directive, **`/ci amp_deploy_gke_ycp`**, additionally proves the amp↔YCP
integration end to end. On top of the plain deploy it builds + pushes the YCP
control-plane image (`meko/api_server` → `quay.io/yugabyte/ycp-api-server`) and the
reconcile-agent image (`ycp/Dockerfile.reconcile-agent` →
`quay.io/yugabyte/ycp-reconciler-agent`) from the PR source, then
[`ycp-up.sh`](ycp-up.sh) stands up an ephemeral control plane
(`api_server --control-plane-mode` + a throwaway `emptyDir` Postgres, from
[`ycp/controlplane.yaml`](ycp/controlplane.yaml); set `YCP_POSTGRES_PVC_SIZE` to back
it with a PVC for a long-lived dev control plane instead) in the same `ci-pr-<n>` namespace,
registers a `kind=amp` environment bound to a generated api key id, and drops the
dev-mode credentials into the `amp-agent-api-key` Secret. `up-gke.sh` is then run with
`YCP_ENABLED=1`, which deploys the reconcile-agent (`agent.*` — `ycpUrl` to the
in-cluster Service, `envName` to the registered id, `reconcileInterval=5s`, and the
api-key secret).

Because `agent.enabled` marks the install as a YCP-managed environment, that same
block must also satisfy the chart's telemetry preflight: it creates the
`byoc-metrics-auth` and `datadog-api` Secrets and deploys with `otel.byoc.enabled=true`
against the door `ycp-up.sh` started the control plane with (`YCP_BYOC_METRICS_URL`,
one variable read by both, so the hint's endpoint and the enroll check cannot
disagree). It needs the `BYOC_METRICS_PASSWORD` and `DATADOG_API_KEY` repository
secrets; there is deliberately no flag that skips the check.
Instead of the API-driven smoke, [`ycp-reconcile-smoke.sh`](ycp-reconcile-smoke.sh)
runs: it declares a pool→cluster→branch→endpoint through YCP desired state and asserts
the authoritative agent creates the tree, reports it `READY` in
`GET /api/v1/environments`, and garbage-collects it when the cluster is withdrawn. (The
API-driven `smoke-test.sh` is skipped in this mode — the agent would GC any resource it
creates out-of-band.) The whole control plane is torn down with the namespace. This
directive is opt-in only — it does **not** run nightly.

[`ycp-controller-upgrade-test.sh`](ycp-controller-upgrade-test.sh) is a third
script over the same two preconditions, run by hand rather than by CI: it moves
the controller's own version through YCP desired state and asserts the release
advances one helm revision, the controller, agent and endpoint pin all land on
the new tag, the endpoints running before the upgrade survive it unchanged, and
endpoints created afterwards come up on the new pin. It needs a chart the agent
can pull — `YCP_CONTROLLER_CHART` into `agent.controllerChart`, published by
[`ycp/test/local/controller-chart-publish.sh`](../../ycp/test/local/controller-chart-publish.sh)
— because the default OCI reference is not published yet, which is also why this
is not in CI.

Stale `ci-pr-*` namespaces (from a run cancelled before teardown) are swept every
6 h by [`amp-control-plane-gke-cleanup.yml`](../../.github/workflows/amp-control-plane-gke-cleanup.yml).

Required repo secrets (both workflows):

- **`KUBECONFIG_ITEST_DEV`** — a **base64-encoded static kubeconfig** for itest-dev.
  It must be a **ServiceAccount-token** kubeconfig (self-contained), *not* the one
  `gcloud container clusters get-credentials` writes — that uses the
  `gke-gcloud-auth-plugin` exec plugin, which needs GCP creds at runtime and won't
  work in CI. Mint one against itest-dev (run with your own admin `kubectl`
  context pointed at itest-dev):
  ```sh
  kubectl create serviceaccount amp-ci -n kube-system
  # CI creates/deletes namespaces and (via the chart) cluster-scoped RBAC, so it
  # needs broad rights; scope this ClusterRole down if you prefer.
  kubectl create clusterrolebinding amp-ci --clusterrole=cluster-admin \
    --serviceaccount=kube-system:amp-ci

  SERVER=$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.server}')
  CA=$(kubectl config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  TOKEN=$(kubectl create token amp-ci -n kube-system --duration=8760h)   # k8s >= 1.24

  cat > kubeconfig.ci <<EOF
  apiVersion: v1
  kind: Config
  clusters: [{name: itest-dev, cluster: {server: $SERVER, certificate-authority-data: $CA}}]
  users: [{name: amp-ci, user: {token: $TOKEN}}]
  contexts: [{name: itest-dev, context: {cluster: itest-dev, user: amp-ci}}]
  current-context: itest-dev
  EOF
  base64 -w0 kubeconfig.ci   # paste this as the KUBECONFIG_ITEST_DEV secret
  ```
- **`QUAY_TOKEN`** — a Quay robot credential as a **`robot-user:token`** string.
  CI splits it to `docker login quay.io` (push) and derives the in-namespace pull
  secret from it (the `.dockerconfigjson` auth is `base64(QUAY_TOKEN)`). The robot
  needs **push + pull** on the four `amp-*` repos — plus **`ycp-api-server`** and
  **`ycp-reconciler-agent`** if you use `/ci amp_deploy_gke_ycp` (it builds + pulls the
  control-plane and reconcile-agent images there).

Fork PRs are skipped (secrets aren't exposed to them).

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
the deployment-side companion to the controller-on-Docker integration in
[`test/integration/controller`](../test/integration/controller): that suite proves the control
plane on Docker; this one proves an actual Kubernetes deployment, including the
write-ahead task contract (#113) and **real data I/O** through a created endpoint.

It port-forwards the controller API and walks the full async-task lifecycle —
`createCluster` / `createEndpoint` / `branch` / `deleteEndpoint` / `deleteCluster` /
`deleteBranch` each return `202 + a Task + a Location` header and run to
`status=succeeded` (polled via `GET /tasks/{id}`) — then **connects to the new
endpoint with `psql` and writes + reads actual data**: full CRUD + `CHECKPOINT`
(forcing the writes through safekeeper → pageserver → YugabyteDB) and a readback,
and checks that the recorded connection address routes to a ready Service backend.
It also asserts the contract edges (`GET /tasks` filters + a bad
`status` → `400`, a duplicate name → a *failed* task, deleting a branch with a
child fork → `409`, and `delete_cluster` cascading). `createUser` /
`createStoragePool` are checked to stay synchronous `201`.

It tears down the cluster it creates and any stray endpoint releases, and exits
non-zero on any failure. It does **not** remove the pool or user rows — the API
has no delete for either — so it reuses one well-known pool (`pool-smoke`)
rather than leaking one per run.

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

One image runs every process that makes up an endpoint, supervised by
`endpoint-supervisor` (`compute/endpoint_supervisor`) started by `tini` as
PID 1:

| Service          | Role                                   | Backed by   |
|------------------|----------------------------------------|-------------|
| `safekeeper`       | WAL                                  | YugabyteDB  |
| `pageserver`    | page service                         | YugabyteDB  |
| `compute_ctl`      | supervises Postgres v18 + `neon` ext | (compute)   |

The supervisor renders each service's config from environment variables,
brings the stack up in order (safekeeper → pageserver →
compute), creates the safekeeper timeline, then monitors the children and
restarts them with backoff; if a service exceeds its restart budget the
supervisor exits non-zero so the pod is recreated.

### Why no control plane

Cluster control — branch and endpoint bookkeeping in YugabyteDB's `amp_registry`
schema — is a
**shared, cluster-level** concern owned by the `controller`. It is **not** part of
the endpoint pod. The pod talks only to YugabyteDB.

A consequence: the `yb_storage` schema must already exist before the pod starts.
The controller provisions it (all schemas) on startup, and is the only thing that
does — an endpoint never provisions its own.

## Building the image

yb-amp is Postgres v18 only. From the **repo root**:

```sh
docker build -t yb-amp/neon-endpoint:dev -f deploy/endpoint/Dockerfile .
```

This compiles patched Postgres v18, the Rust services, and the `neon`
extension, so the first build is slow. It builds against the self-built
`quay.io/yugabyte/amp-build-tools` base (same as the repo-root Dockerfile and the
extension ABI-anchor base). That base is built and published by
[`build-tools/build.sh`](build-tools/build.sh) /
`.github/workflows/amp-build-tools.yml` to a **private** Quay repo, so building
any consumer needs `docker login quay.io` first (CI logs in with the
`QUAY_AMP_TOKEN` robot before the endpoint/extension builds; locally, use your
own Quay login).

The endpoint image ships **only** PG18 + the `yb_amp` extension. Every other
extension (starting with `pgvector`) is built and shipped separately as an OCI
ImageVolume — see [`extensions/`](extensions) and
[`compute/architecture/extensions.md`](../compute/architecture/extensions.md):

```sh
deploy/extensions/build.sh            # build the ABI-anchor base + all extensions locally
deploy/extensions/build.sh --push pgvector   # build + push just pgvector
```

The chart mounts them via `endpoint.extensions`
(`{ name, image, preload, library }` — `library` is the
`shared_preload_libraries` entry when it differs from `name`, as documentdb's
one image ships `pg_documentdb_core.so` and `pg_documentdb.so` and has no
`documentdb.so`),
rendering one read-only ImageVolume per entry at `/extensions/<name>` and
deriving `extension_control_path` / `dynamic_library_path` (and any preload
entry for `SHARED_PRELOAD_LIBRARIES`) from the list. An image amp does **not**
build is not listed here at all: an operator registers it through
`PUT /extensions/{name}`, and the entry then also carries the layout, loader and
ABI-waiver fields the arch doc covers under "Registering an extension image".

On an **Apple Silicon Mac** the default qemu emulation crashes the Rust compile;
see [`building-images-apple-silicon.md`](building-images-apple-silicon.md) to build
locally via Rosetta.

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
| `YB_SSLCERT` | _(unset)_ | client certificate presented to YugabyteDB; set with `YB_SSLKEY` |
| `YB_SSLKEY` | _(unset)_ | its private key |
| `YB_PASSWORD` | _(unset)_ | YB password (from a Secret) |
| `ENDPOINT_PG_VERSION` | `18` | compute Postgres major version; locates `pg_install/v{N}` (the image ships v18 only) |
| `COMPUTE_PORT` | `55433` | compute Postgres port |
| `COMPUTE_PASSWORD` | _(unset)_ | password for the compute's default user (`cloud_admin`), set by the supervisor once Postgres answers so non-loopback clients can authenticate. May be a plaintext (Postgres hashes it, to SCRAM) or the verifier `pg_authid` stores — `SCRAM-SHA-256$...` — which is what the control plane sends, so it never hands the pod something that can log in. Unset leaves the role's password as the catalog has it; the supervisor also declines to overwrite a password that is already set |
| `SHARED_PRELOAD_LIBRARIES` | `yb_amp,pg_stat_statements` | compute `shared_preload_libraries` |
| `EXTENSION_LIBRARY_PATH` | _(unset)_ | colon-separated dirs of system shared libraries a registered extension bundles (e.g. `/extensions/postgis/system`); appended to `LD_LIBRARY_PATH` for every child |
| `TENANT_ID` / `TIMELINE_ID` | _(unset)_ | serve an existing tenant; unset ⇒ bootstrap a fresh one |

Internal ports (`PAGESERVER_PG_PORT`, `PAGESERVER_HTTP_PORT`,
`PAGESERVER_GRPC_PORT`, `SAFEKEEPER_PG_PORT`, `SAFEKEEPER_HTTP_PORT`,
`COMPUTE_CTL_HTTP_PORT`), the LFC prewarm knobs (`AUTOPREWARM`,
`OFFLOAD_LFC_INTERVAL_SECONDS`) and the supervisor knobs
(`SUPERVISOR_RESTART_BACKOFF_SECONDS`, `SUPERVISOR_MAX_RESTARTS`,
`SUPERVISOR_RESTART_WINDOW_SECONDS`) have sensible defaults.

## Running it locally (against a YugabyteDB on the host)

```sh
docker run --rm --name yb-endpoint \
  --network host \
  -e YB_HOST_PORTS=127.0.0.1:5433 \
  -e YB_USER=yugabyte -e YB_PASSWORD=yugabyte \
  -e COMPUTE_PASSWORD=pick-a-password \
  yb-amp/neon-endpoint:dev

# then, from the host:
psql -h 127.0.0.1 -p 55433 -U cloud_admin -d postgres   # password: pick-a-password
```

Without `COMPUTE_PASSWORD` the supervisor leaves `cloud_admin` with no password
at all, and only loopback (which `pg_hba` trusts) can connect — there is no
built-in default. Pass either a plaintext or the verifier `pg_authid` stores.

(`--network host` lets the container reach a host YugabyteDB on `127.0.0.1:5433`
and exposes the compute on the host's `55433`.)

## Helm chart

See [`helm/yb-amp-endpoint`](helm/yb-amp-endpoint). It renders a
Deployment (stateless — PGDATA is ephemeral, all durable state is in YB), a
ConfigMap carrying the YB connection block + endpoint settings, an optional
Secret for the YB password, and a Service exposing the compute port.

```sh
helm install ep deploy/helm/yb-amp-endpoint \
  --set yugabyte.hostPorts=yb-tserver.default.svc:5433 \
  --set yugabyte.password=$YB_PASSWORD \
  --set image.repository=yb-amp/neon-endpoint --set image.tag=dev

# unit tests (requires the helm-unittest plugin):
helm unittest deploy/helm/yb-amp-endpoint
```
