# Prepare your infrastructure for BYOC amp

The preflight an SE walks with a customer before the first `yb-amp-controller` install:
what the cluster must have, what permissions the installer needs, what has to be
reachable, what the nodes must be able to hold, and how to prove it afterwards.

It follows the same order as YugabyteDB Anywhere's
[Prepare your infrastructure](https://docs.yugabyte.com/stable/yugabyte-platform/prepare/)
— permissions, networking, nodes — because that is the sequence the field already runs.
The content is amp's own: every requirement below is taken from the chart, the controller
and the runbooks in this repo, not from YBA.

Architecture behind it: [`controller/architecture/ac-k8s-deploy.md`](../controller/architecture/ac-k8s-deploy.md)
(how endpoints are deployed), [`controller/architecture/ac-ycp.md`](../controller/architecture/ac-ycp.md)
(how the environment is driven from YCP), [`ycp/ARCHITECTURE.md`](../../ycp/ARCHITECTURE.md)
(the control-plane contract). Values reference:
[`deploy/helm/yb-amp-controller/README.md`](../deploy/helm/yb-amp-controller/README.md).

## Track your progress

- [ ] **§1** Kubernetes ≥ 1.33, on EKS/GKE/AKS, x86-64 nodes
- [ ] **§1** Block StorageClass, OpenTelemetry Operator, PSA not enforcing baseline
- [ ] **§1** Backing YugabyteDB with a `yb_storage` database, reachable in-cluster
- [ ] **§2** Installer can create the namespace, the release, and ClusterRoles
- [ ] **§2** Customer has accepted the release's cluster-scoped grants, or chosen the namespace-only install
- [ ] **§2** Cloud identities for LB, CSI and image pull confirmed working; day-2 owners named
- [ ] **§3** Egress allowlist for the YCP URL and the image registry
- [ ] **§3** In-cluster paths open; controller/UI **not** internet-facing
- [ ] **§3** If publishing privately: NAT CIDR reserved and in the endpoint's `allowed_cidrs`
- [ ] **§4** Node pool sized to hold a whole endpoint (Guaranteed QoS), `/data` backing decided
- [ ] **§5** Pull secret, agent API key, backing-YB credentials in hand
- [ ] **§6** Install values settled, environment registered in YCP, install hint captured
- [ ] **§7** Post-install verification passed, including a real endpoint and a resize

## 0. What the install is, and who owns what

One namespace, one **environment**, one Helm release:

| Pod | Replicas | Notes |
|---|---|---|
| `amp-controller` | 1 | The control plane. Holds in-pod state (its managed controller subprocess) — **do not scale**. No liveness probe by design. |
| `amp-controller-ui` | 1+ | nginx + the SPA, reverse-proxying `/api/` in-cluster. Stateless. |
| `amp-controller-mcp` | 1+ | Stateless REST→MCP wrapper. Optional (`mcp.enabled`). |
| `amp-reconcile-agent` | 1 | The sole YCP client for this environment. Optional (`agent.enabled`), required for a YCP-managed install. |
| otel collectors | 2 deployments + 1 daemonset | Optional (`otel.enabled`), rendered only if the OpenTelemetry Operator CRDs exist. |
| `yb-ep-*` | one per endpoint | Tenant computes, created by the controller at runtime — not part of the release. |

Two things the chart does **not** create and that must pre-exist: the **namespace** and
the **backing YugabyteDB**. All durable tenant state lives in that database (`yb_storage`);
an endpoint pod's `/data` is scratch.

The ownership split, which is the part customers most often assume the other way round:

| Yugabyte operates | The customer owns |
|---|---|
| YCP (the control plane), the images, the chart, what an environment is asked to run | The cluster, its nodes and storage, the backing YugabyteDB, network policy and exposure, upgrades of the release |

Control flow is **pull-only**: the reconcile-agent dials out to YCP over HTTPS, reads
desired state and reports inventory back. YCP never dials into the cluster, and no
inbound path from Yugabyte is required or used.

## 1. Prepare the cluster

| Requirement | Why | Check |
|---|---|---|
| **Kubernetes ≥ 1.33** | In-place endpoint resize uses the `pods/resize` subresource (beta, on by default in 1.33). Externalized extensions mount as **ImageVolume** (1.33+ beta, `ImageVolume` feature gate). On older clusters resize is rejected and extension mounts do not render. | `kubectl version` |
| **EKS, GKE or AKS** | The `nlb_*` endpoint connectivity modes render that provider's load-balancer annotations. On a cluster whose cloud the controller cannot determine, an `nlb_internal` create **fails** rather than falling back to an internet-facing LB. | `kubectl get node -o jsonpath='{.items[0].spec.providerID}'` |
| **x86-64 node pools** | The amp images are `linux/amd64` only. An arm64 pool (Graviton, Ampere, Axion) pulls the image cleanly and then dies with `exec format error`. | `kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.architecture}'` |
| **A cloud LoadBalancer controller** | The controller/UI Services default to an internal LB; `nlb_*` endpoints each provision their own. | `kubectl get svc -A` for an existing LB |
| **A block StorageClass** (EBS gp3 / pd-ssd / managed-csi) | Endpoint `/data` PVCs, and the backing YugabyteDB's volumes. The install fails naming the class it wanted if it is absent. | `kubectl get storageclass` |
| **OpenTelemetry Operator** | Provides the `OpenTelemetryCollector` CRD the chart's collectors are. Without it the collectors are silently skipped (NOTES.txt says so) and no metrics or logs are produced. | `kubectl get crd opentelemetrycollectors.opentelemetry.io` |
| **A metrics + logs store** | Collectors remote-write to a Prometheus-compatible endpoint and OTLP-push logs. The `amp-monitoring` chart ships VictoriaMetrics/VictoriaLogs/Grafana if the customer has none. | — |
| **Pod Security Admission not enforcing `baseline`/`restricted`** on the namespace | The logs daemonset runs `runAsUser: 0` and mounts two **hostPath**s (`/var/log/pods`, `/var/lib/otelcol/…`), which baseline forbids. Either label the namespace `pod-security.kubernetes.io/enforce=privileged` or set `otel.collectors.logs.enabled=false`. | `kubectl get ns <ns> --show-labels` |
| **Backing YugabyteDB with a `yb_storage` database** | The controller provisions its own schemas (`controller`, `amp_registry`, `amp_storage`) inside it; it does not create the database. Every tenant's pages and WAL live here — size and protect it like a production database, not like a metadata store. | `ysqlsh -c '\l'` |
| **helm 3 + kubectl on the installer's machine** | The chart install itself. (The controller carries its own helm ≥ 3.13 and the endpoint chart inside its image.) | `helm version` |

Run the lot in one pass:

```sh
NS=amp                      # the namespace the release will go in

# Server version — 1.33 is the floor.
kubectl version -o json | jq -r .serverVersion.gitVersion

# Cloud (from providerID) and architecture, per node. Any arm64 here is fatal.
kubectl get nodes -o json |
  jq -r '.items[] | "\(.metadata.name)  \(.status.nodeInfo.architecture)  \(.spec.providerID)"'

# Storage classes and their binding mode.
kubectl get storageclass -o custom-columns='NAME:.metadata.name,PROVISIONER:.provisioner,BINDING:.volumeBindingMode'

# The OTel Operator's CRD. No output = the collectors will be skipped, silently.
kubectl get crd opentelemetrycollectors.opentelemetry.io -o name

# An existing LoadBalancer Service with an address proves the cloud LB controller works.
kubectl get svc -A -o json | jq -r '.items[]
  | select(.spec.type=="LoadBalancer")
  | "\(.metadata.namespace)/\(.metadata.name)  \(.status.loadBalancer.ingress[0].ip // .status.loadBalancer.ingress[0].hostname // "PENDING")"'

# PSA on the target namespace. enforce=baseline or restricted blocks the logs daemonset.
kubectl get ns "$NS" -o jsonpath='{.metadata.labels}'; echo
```

## 2. Prepare permissions

Four different things are called "roles" on a BYOC install, and they are held by
different parties. In order: what the human/service account running `helm install`
needs, what the release grants itself, what must exist in the customer's cloud
account, and who owns what afterwards.

### 2a. The installing principal

The installer is whoever runs `helm upgrade --install` — an SE's own credentials for a
hand-driven install, or a CI service account. It needs the namespace, everything Helm
lays down in it, and the right to create the release's cluster-scoped grants.

```yaml
# Namespaced: everything the release renders.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: amp-installer, namespace: amp}
rules:
  - apiGroups: [""]
    resources: [serviceaccounts, secrets, configmaps, services, persistentvolumeclaims]
    verbs: ["*"]
  - apiGroups: ["apps"]
    resources: [deployments]
    verbs: ["*"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: [roles, rolebindings]
    verbs: ["*"]
  # Only when otel.enabled=true.
  - apiGroups: ["opentelemetry.io"]
    resources: [opentelemetrycollectors]
    verbs: ["*"]
---
# Cluster-scoped: the namespace itself, the release's two ClusterRoles, and the
# CRD read that tells the chart whether the OTel Operator is installed.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: amp-installer}
rules:
  - apiGroups: [""]
    resources: [namespaces]
    verbs: [get, list, create]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: [clusterroles, clusterrolebindings]
    verbs: [get, list, create, update, delete]
  - apiGroups: ["apiextensions.k8s.io"]
    resources: [customresourcedefinitions]
    verbs: [get, list]
```

Check what the installer actually holds before the install rather than halfway through it:

```sh
NS=amp

for r in serviceaccounts secrets configmaps services deployments roles rolebindings; do
  printf '%-18s %s\n' "$r" "$(kubectl auth can-i create "$r" -n "$NS")"
done
kubectl auth can-i create namespaces
kubectl auth can-i create clusterroles
kubectl auth can-i create clusterrolebindings
kubectl auth can-i escalate roles -n "$NS"     # the way past the rule below
```

Apply the two objects above and bind them to whoever installs:

```sh
kubectl apply -f amp-installer-rbac.yaml
kubectl -n "$NS" create rolebinding amp-installer --role=amp-installer --user='<installer>'
kubectl create clusterrolebinding amp-installer --clusterrole=amp-installer --user='<installer>'
```

**The escalation rule is the catch.** Kubernetes refuses to let a principal create a
Role granting permissions it does not itself hold. The release's Role grants `pods: *`
and `pods/resize: patch`; the installer's Role above does not. So the installer must
*additionally* hold everything in §2b — or hold `escalate` and `bind` on
`roles`/`clusterroles` (`escalate` for the Roles, `bind` for the bindings to them),
or simply be cluster-admin for the install. In practice: hand the install to a
cluster-admin, or bind the installer to both its own Role and the release's.

A customer who will not grant that can instead have their own admin pre-create the
ServiceAccount, Role, RoleBinding and ClusterRoles from the chart's rendered output
(`helm template`), and let the installer run with `--set rbac.nodeRead=false
--set rbac.clusterScoped=false` against objects that already exist.

### 2b. What the release grants itself

| Subject | Scope | Rules | Default |
|---|---|---|---|
| `amp-controller` SA | **Role**, release namespace | `pods, services, configmaps, secrets, persistentvolumeclaims, endpoints, events, serviceaccounts: *`; `apps/deployments, replicasets, statefulsets: *`; `batch/jobs: *` (Voyager migrations); `pods/resize: patch` | always |
| `amp-controller` SA | **ClusterRole** `nodes: get, list` | Read `spec.providerID` to detect the cloud (drives `nlb_*` annotations) and resolve NodePort node IPs. Read-only; cluster-wide only because Node is a cluster-scoped kind. | `rbac.nodeRead: true` |
| `amp-controller` SA | **ClusterRole** `namespaces: get, list, create` | Helm's `--create-namespace` probing. **This is the one grant that can change the cluster.** | `rbac.clusterScoped: false` |
| otel collector SA | **ClusterRole** `nodes`, `nodes/proxy`, `nodes/metrics` | cAdvisor per-pod resource metrics via the kubelet. | `otel.podMetrics: true` |
| `reconcile-agent` SA | **Role**, release namespace | `secrets: get, create, patch` — its own identity Secret (`reconcile-agent-state`) only. Its credential lives in a separate operator-planted Secret it never writes. | with `agent.enabled` |

The namespaced Role is broad because the controller drives Helm to create and delete
endpoint workloads there; it holds nothing outside that namespace beyond the two read-only
cluster-scoped grants. Cluster-scoped object names are namespace-prefixed, so several
environments can share one cluster.

Verify them after the install — the two that matter are the ones whose absence surfaces
much later, as a failed resize and a failed `nlb_internal` create:

```sh
NS=amp
SA="system:serviceaccount:${NS}:amp-controller"

kubectl -n "$NS" get sa,role,rolebinding amp-controller
kubectl get clusterrole,clusterrolebinding | grep "${NS}-amp-controller"

kubectl auth can-i patch pods/resize -n "$NS" --as="$SA"   # in-place resize
kubectl auth can-i get nodes --as="$SA"                    # cloud detection
```

### 2c. Cloud IAM

**The operator needs almost none.** Everything amp does in the cloud, it does *through*
the Kubernetes API — the cloud calls are made by the cluster's own controllers under
their existing identities. What to verify is that those identities are already
provisioned, because an install that lands fine and then cannot create a load balancer
fails at the first `nlb_internal` endpoint, not at install time.

| Capability | Who exercises it | Verify |
|---|---|---|
| Pull the amp images | kubelet, via the node identity or the pull Secret | pull an image by hand on a node, or check the pull Secret |
| Create/delete the per-endpoint L4 load balancer | the cluster's cloud controller manager (GKE/AKS) or AWS Load Balancer Controller (EKS) | an existing LoadBalancer Service in the cluster |
| Provision endpoint `/data` volumes | the CSI driver's identity (EBS CSI / PD CSI / Azure Disk CSI) | bind a test PVC of the chosen class |
| Scale the endpoint node pool | cluster autoscaler, if enabled | — |
| Ship metrics off-cluster | none — it is an outbound HTTPS call with a bearer credential, not a cloud API call | §3a egress |

So the operator's own cloud identity needs exactly one thing: **the ability to obtain
kubeconfig credentials for the cluster**, plus enough Kubernetes RBAC from §2a. That is
`roles/container.clusterViewer` plus a k8s RBAC binding on GKE; an EKS access entry (or
an `aws-auth` mapping) on EKS; `Azure Kubernetes Service Cluster User Role` plus a k8s
RBAC binding on AKS. Names vary by the customer's own IAM conventions — treat these as
the shape to ask for, not as literals to paste.

Two things that are the customer's cloud-side homework, and that block later rather than at install:

- **Load balancer quota** per region — one per `nlb_*` endpoint.
- **Subnet and address headroom** — internal LBs consume addresses in the cluster's
  subnets, and private-endpoint publishing consumes more (§3e).

### 2d. Day-2 roles — who owns what after the install

| Role | Held by | Owns |
|---|---|---|
| Fleet operator | Yugabyte | Registering the environment in YCP, declaring desired state, the YCP admin secret, the data-plane release version |
| Environment API key holder | Yugabyte, planted once into the customer cluster | The one key that binds this environment to its agent. Shown once; rotating it means re-planting the Secret |
| Cluster admin | Customer | The cluster, its node pools, PSA and NetworkPolicy, the RBAC in §2a/§2b, cloud quota |
| Database operator | Customer | The backing YugabyteDB — sizing, backups, upgrades, its on-call |
| Application team | Customer | The endpoints they ask for, their connectivity mode and `allowed_cidrs`, their Postgres credentials |

The load-bearing one is **database operator**: the backing YugabyteDB holds every
tenant's pages and WAL, is not created or managed by the chart, and has no Yugabyte-side
operator in the BYOC model. If nobody on the customer side owns it, nobody owns the data.

### 2e. The namespace-only install

For a tenant that may hold no cluster-scoped binding at all:

```sh
--set rbac.nodeRead=false \
--set rbac.clusterScoped=false \
--set otel.podMetrics=false \
--set-string controller.endpoint.cloud=aws     # required: nothing can detect it now
```

Cost: the "Pod resources" dashboard reports no data, and `controller.endpoint.cloud` must
be right or `nlb_internal` will not work.

## 3. Configure networking

### 3a. Egress from the customer cluster (allowlist these)

| Destination | Port | Who | Required? |
|---|---|---|---|
| The YCP base URL — the YBM per-account pass-through, or `https://devcloud.yugabyte.com/ycp/api/v1` | 443 | reconcile-agent | yes, for a YCP-managed environment |
| `quay.io/yugabyte` (+ its CDN) | 443 | kubelet image pulls | yes, unless images are mirrored into a private registry |
| `byoc.devcloud.yugabyte.com` (OTLP metrics → Thanos, over AWS PrivateLink) | 443 | otel metrics collectors | only with `otel.byoc.enabled=true` |
| `us3.datadoghq.com` | 443 | otel logs daemonset | only with `otel.byoc.enabled=true` |
| `grafana.com` | 443 | Grafana pod | only if the `amp-monitoring` chart is installed — the VictoriaLogs datasource plugin is downloaded at pod start |

**No inbound rule is needed.** The agent's outbound heartbeat is how desired state,
inventory and the task log move; there is no path from YCP into the cluster.

Test it from inside the cluster, in the namespace the release will live in — egress policy
and proxy settings differ per namespace, so a laptop `curl` proves nothing:

```sh
NS=amp
YCP_HOST=devcloud.yugabyte.com      # or the customer's YBM pass-through host

kubectl -n "$NS" run egress-check --rm -i --restart=Never \
  --image=curlimages/curl:8.11.1 -- sh -c "
    for h in $YCP_HOST quay.io; do
      code=\$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://\$h/ || echo UNREACHABLE)
      printf '%-32s %s\n' \$h \$code
    done"
```

Any HTTP status is a pass — it proves the TLS connection completed. `UNREACHABLE` is the
failure, and on a cluster with an egress proxy it is the expected one until the proxy is
configured.

### 3b. In-cluster paths (NetworkPolicy)

| From | To | Port |
|---|---|---|
| controller | endpoint pod Service DNS (pageserver / safekeeper HTTP) | 9898, 7676 |
| endpoint pods | backing YugabyteDB tserver | 5433 |
| reconcile-agent | `amp-controller` Service | 8080 |
| UI (nginx), MCP | `amp-controller` Service | 8080 |
| otel collectors | controller, endpoint pods (pageserver / safekeeper / compute_ctl / postgres_exporter) | 8080, 9898, 7676, 3080, 9187 |
| clients | endpoint compute Postgres | 55433 |

### 3c. Client → Postgres, per endpoint

`connectivity.mode` decides reachability at create time — the amp analogue of an Aeon IP
allow list plus VPC choice:

| Mode | Reach | Notes |
|---|---|---|
| `cluster_ip` (default) | in-cluster only | plus the tailnet, if the endpoint Service is tailnet-exposed |
| `nlb_internal` | surrounding VPC/VNet only | one cloud LB **per endpoint**; requires EKS/GKE/AKS + cloud detection |
| `nlb_external` | internet | one cloud LB per endpoint |

`allowed_cidrs` (→ `loadBalancerSourceRanges`) applies to the `nlb_*` modes, and each entry
must carry a prefix length — a bare IP is a `400`. **Empty means unrestricted**, so on
`nlb_external` the Postgres password is the only barrier.

The pageserver, safekeeper and supervisor ports are never published on an `nlb_*` load
balancer — the supervisor's settings API is unauthenticated and can restart Postgres.

### 3d. What must not be exposed

**The controller REST API and UI have no authentication**, and `createUser` can mint an
admin. Keep `controller.service.type` and `ui.service.type` on `internal-lb` (or `none`
plus port-forward / tailnet) and let the customer put their own front door in front if
they need one. A public LoadBalancer on the controller is a full compromise of the
environment.

Avoid `NodePort` for endpoint Services in production: a NodePort Service publishes *every*
port it declares, pageserver and safekeeper HTTP included.

### 3e. Private connectivity (PSC / PrivateLink)

Both `nlb_*` modes put the endpoint on a load balancer in the **customer's own VPC**. A
consumer in a *different* VPC or project reaches it through a **customer-created service
attachment** in front of that load balancer — amp creates nothing here, and cannot:
nothing in the control plane holds credentials in the customer's cloud account.

Procedure: [`runbooks/psc-endpoint-publishing.md`](../runbooks/psc-endpoint-publishing.md).
The three things to raise on the call before anyone commits to it:

- **Put the NAT CIDR in the endpoint's allowlist.** PSC traffic arrives sourced from the
  NAT subnet, not the consumer's real address, so it has to be in `allowed_cidrs`. The
  list can be replaced in place (`POST …/endpoints/{endpointId}/connectivity`) without
  touching the forwarding rule; the connectivity **mode** cannot, and changing it means
  recreating the endpoint, which invalidates the attachment built on it.
- **One NAT subnet per published endpoint.** A NAT subnet cannot be shared between service
  attachments, so N published endpoints means N subnets, each created and destroyed by
  hand in the right order.
- **It is unreconciled.** An endpoint recreated by amp leaves a dangling attachment and
  nothing detects it. Retire the attachment *before* deleting the endpoint.

Fine for a handful of deliberately published databases. For many, this is the wrong shape
and the answer is a shared front end — a design decision, not a runbook.

## 4. Prepare nodes

**Endpoint pods are Guaranteed QoS**: CPU and memory are applied as request == limit, so
each endpoint's full reservation must fit on one node, and in-place resize cannot change
QoS class.

The shape is policy, not a per-request free-for-all:

| Dimension | Rule | Default endpoint (2 vCPU) |
|---|---|---|
| Memory | **8 GiB per vCPU** — fills in memory when a create names only CPU, and is a **floor** (less is a `400`) | 16 GiB |
| `/data` disk | **20 GiB per vCPU**, with `controller.endpoint.diskSize` as a floor, uncapped above | 40 GiB |
| CPU | as requested | 2 cores |

So node sizing is *(the largest endpoint you will sell)* + kubelet/system overhead, and
one endpoint per that many cores. A 2 vCPU / 16 GiB endpoint does not fit a node with
16 GiB total; an 8 vCPU endpoint wants 64 GiB.

Decide the `/data` backing before install — it is a deployment-wide setting, not a
per-endpoint one:

- **PVC** (`controller.endpoint.storageClass` set) — network block volume, survives
  reschedule, counts against per-node volume-attachment limits.
- **emptyDir** (storage class empty) — node ephemeral storage, which is **whatever
  disk the kubelet's root is on**: local NVMe on a GKE local-SSD pool, the EBS boot
  volume on a stock EKS node. On the boot volume the endpoint works and the LFC runs at
  that volume's IOPS (#1612). Set `controller.endpoint.scratchBacking: local-nvme` and
  point `controller.endpoint.nodeSelector` at a pool whose ephemeral storage is the
  instance NVMe (GKE: `cloud.google.com/gke-ephemeral-storage-local-ssd=true`; EKS: an
  AL2023 node group whose nodeadm config sets `localStorage.strategy: RAID0`, labelled
  `amp/local-nvme=true` — recipe in `amp/deploy/README.md`). The YCP install hint
  carries both, with the label as its `NODE_SELECTOR` header variable. Without the
  selector the chart fails to render and the controller refuses to start, rather than
  quietly scheduling onto a boot disk. A
  rescheduled pod gets an empty `/data`, which is fine — the durable copy is in
  YugabyteDB. Node ephemeral capacity is then the real limit: a GKE node on a 100 GB
  boot disk has ~43.8 GiB allocatable, i.e. **one** 2-vCPU endpoint.

Also plan for:

- **A separate endpoint node pool.** If it is tainted, set `otel.logsTolerations` or that
  pool's logs are silently missing.
- **Cold image pulls.** The endpoint image is large; on a multi-node or cold-cache cluster
  the first pull per node can exceed the controller's 10 m deploy timeout. Raise
  `controller.deployTimeout` — on the Kubernetes driver it also sets the pod's
  startupProbe budget and `progressDeadlineSeconds`, so it genuinely buys bootstrap time.
- **Load-balancer quota.** Each `nlb_*` endpoint provisions its own cloud LB and counts
  against the account's per-region quota.

Check the cluster can actually hold the shape you are selling:

```sh
# Allocatable per node. Compare against (largest endpoint CPU) x 8 GiB.
kubectl get nodes -o custom-columns='NODE:.metadata.name,CPU:.status.allocatable.cpu,MEM:.status.allocatable.memory,EPHEMERAL:.status.allocatable.ephemeral-storage'

# Taints on the endpoint pool — these are what otel.logsTolerations has to match.
kubectl get nodes -o json |
  jq -r '.items[] | "\(.metadata.name)  \([.spec.taints[]? | "\(.key)=\(.value):\(.effect)"] | join(" "))"'

# Does the chosen class actually bind?
kubectl -n "$NS" apply -f - <<'YAML'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sc-probe}
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
  storageClassName: <class>
YAML
kubectl -n "$NS" get pvc sc-probe
kubectl -n "$NS" delete pvc sc-probe
```

A class whose `volumeBindingMode` is `WaitForFirstConsumer` leaves the probe **Pending**
until a pod mounts it — correct, not a failure. `Pending` on an `Immediate` class is a real
failure; `kubectl -n "$NS" describe pvc sc-probe` says why.

## 5. Prepare credentials

| Secret | Contents | Needed when |
|---|---|---|
| image pull secret | Quay robot credentials (`imagePullSecrets[0].name`; also becomes `AMP_DEPLOY_PULL_SECRET` for endpoint pods) | unless the node identity can pull |
| `amp-agent-api-key` | key `api-key` = the YBM API key, **shown once** at creation. One key drives exactly one environment. | `agent.enabled=true` |
| backing-YB credentials | in `controller.databaseUrl`, and `--deploy-yb-existing-secret` for the endpoints' own path | always |
| `byoc-metrics-auth` | key `password` — BasicAuth for the metrics door | `otel.byoc.enabled=true` |
| `datadog-api` | key `api-key` | `otel.byoc.enabled=true` — a missing or empty key fails collector startup and takes the **whole** log pipeline down, VictoriaLogs export included |

Create them before the install — the chart holds no credentials and creates none:

```sh
NS=amp

kubectl -n "$NS" create secret docker-registry amp-pull \
  --docker-server=quay.io --docker-username='<robot>' --docker-password='<token>'

# The YCP API key YBM showed once. One key drives exactly one environment.
kubectl -n "$NS" create secret generic amp-agent-api-key --from-literal=api-key='<key>'

# Only with otel.byoc.enabled=true — both, or the collectors fail to start.
kubectl -n "$NS" create secret generic byoc-metrics-auth --from-literal=password='<password>'
kubectl -n "$NS" create secret generic datadog-api      --from-literal=api-key='<key>'

kubectl -n "$NS" get secret
```

## 6. Settle the install values

| Value | Decide |
|---|---|
| `imageTag` | Required in practice — the fallback is the chart appVersion, which is not published. Covers all four amp images and the agent. |
| `imageRegistry` | `quay.io/yugabyte`, or the customer's mirror. A repository carrying its own host wins. |
| `controller.databaseUrl` | The backing YugabyteDB DSN. Use `--set-string`: `--set` mis-tokenizes a DSN's `?` and `=`. |
| `controller.endpoint.storageClass` / `scratchBacking` / `nodeSelector` | PVC vs local-NVMe emptyDir (§4). With the class empty, set `scratchBacking: local-nvme` and the selector together; leaving all three empty only earns a startup warning. |
| `controller.endpoint.service.type` | Leave `ClusterIP`; per-endpoint exposure is the `connectivity` mode, not this. |
| `controller.service.type` / `ui.service.type` | `internal-lb` (§3d). |
| `controller.deployTimeout` | Raise above 10 m for cold multi-node clusters. |
| `agent.ycpUrl`, `agent.envName`, `agent.placement` | From the YCP install hint, **verbatim** — `ycpUrl` already carries the API base path, `envName` must match the registered environment, `placement` must match what it was registered with. |
| `otel.environmentId` / `otel.accountId` | The install hint sets them; they label every series the collectors scrape. |

Order of operations: YCP registers the environment (name + the id of the API key its agent
will use) → the console renders the install hint → the operator plants the API key Secret →
helm install. An environment name is claimed permanently on first registration; registering
it again is a `409`, and nothing is rebound.

Then install, and leave the rendered values where the next person can read them:

```sh
NS=amp

helm registry login quay.io   # the chart is private, like the images
helm upgrade --install yb-amp-controller oci://quay.io/yugabyte/charts/yb-amp-controller -n "$NS" \
  --set-string imageTag='<tag>' \
  --set 'imagePullSecrets[0].name=amp-pull' \
  --set-string controller.databaseUrl='postgres://yugabyte@<host>:5433/yb_storage?sslmode=disable' \
  --set-string controller.endpoint.storageClass='<class>' \
  --set agent.enabled=true \
  --set-string agent.ycpUrl='<from the install hint>' \
  --set-string agent.envName='<from the install hint>' \
  --set-string otel.environmentId='<from the install hint>'

helm -n "$NS" status yb-amp-controller
helm -n "$NS" get values yb-amp-controller
```

Swap `upgrade --install` for `template` to hand the customer manifests to review before
anything is applied — that is also how §2a's pre-created RBAC is produced.

## 7. Verify after install

```sh
NS=<namespace>

kubectl -n $NS rollout status deploy/amp-controller
kubectl -n $NS get pods                       # controller, ui, mcp, agent, collectors

# The controller is serving and reached its store. A 503 on every call means it cannot.
kubectl -n $NS exec deploy/amp-controller -- wget -qO- localhost:8080/readyz

# The agent enrolled and is converging. A quiet loop after "enrolled" is the healthy state.
kubectl -n $NS logs deploy/amp-reconcile-agent | tail -20
kubectl -n $NS get secret reconcile-agent-state -o jsonpath='{.data}' | head -c 200

# End to end: declare storage-pool -> cluster -> branch -> endpoint in YCP desired state,
# then confirm the endpoint reaches Ready and answers SQL.
kubectl -n $NS get pods -l app.kubernetes.io/name=yb-amp-endpoint
```

Then walk the four things that are easy to assume and easy to get wrong:

```sh
# 1. Endpoint pods are Guaranteed QoS.
kubectl -n $NS get pods -o custom-columns='POD:.metadata.name,QOS:.status.qosClass' | grep yb-ep

# 2. A resize lands IN PLACE. Read restartCount and resources, resize through the API,
#    read both again: the SAME restart count with NEW limits is the proof — it is what
#    distinguishes an in-place resize from a pod that was replaced.
EP=<endpoint pod>
kubectl -n $NS get pod $EP -o jsonpath='{.status.containerStatuses[0].restartCount}{"\n"}'
kubectl -n $NS get pod $EP -o jsonpath='{.spec.containers[0].resources}{"\n"}'

# 3. An nlb_internal endpoint has a real address — proves cloud detection. Otherwise the
#    deploy task fails with "needs service.cloud to be one of aws, gcp, azure".
kubectl -n $NS get svc -o json | jq -r '.items[]
  | select(.spec.type=="LoadBalancer")
  | "\(.metadata.name)  \(.status.loadBalancer.ingress[0].ip // "PENDING")"'

# 4. Both collectors exist — the pod-resource one needs the cluster-scoped grant.
kubectl -n $NS get opentelemetrycollector
```

Grafana should then show controller **and** pod-resource panels; the second set is what
proves `otel.podMetrics` got its ClusterRole.

## 8. Sharp edges to pre-empt on the call

- **A failed `nlb_internal` still returns `202`.** Nothing on the request path knows the
  cloud, so it is the deploy task that fails. Read the task error, not the HTTP status.
- **The controller has no liveness probe on purpose** — startup migrations run before
  anything answers `/healthz`, and a liveness kill mid-migration leaves the schema dirty.
- **`503` on every API call** means the controller cannot reach its store; **exit 66 with
  `pglease: lease … lost`** means it was fenced out by another writer.
- **A pod that can never become Ready blocks its own replacement** on a StatefulSet (the
  backing YugabyteDB); a corrected image tag does not self-heal on upgrade — delete the pod.
- **A half-initialised `yugabyted` cannot be restarted out of.** If its first start dies
  part-way — a full disk is the case seen in practice — auth ends up on with the default
  password and every restart repeats it.
- **Placeholders in a copied spec reach the pod.** A pasted `…`, `<registry>` or `<tag>`
  produces `InvalidImageName`, visible only in pod status.

Full triage: [`runbooks/controller_runbook.md`](../runbooks/controller_runbook.md).

## 9. Gaps — say these before the customer finds them

- **No HA for the control plane.** `amp-controller` is one pod holding in-pod state. Losing
  its node is an outage of the control path; endpoints already running keep serving.
- **No authentication on the controller API or UI** (§3d).
- **Backing-YugabyteDB sizing and HA guidance for a production tenant fleet is not written
  down anywhere in this tree.** It is the most consequential unanswered prerequisite here.
- **Upgrade and rollback of a customer environment** (chart + image tags, endpoint rolls)
  is operator-driven; there is no customer-facing procedure yet.
