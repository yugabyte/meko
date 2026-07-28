# Deploying YCP

This guide covers running YCP locally and deploying the demo to a Kubernetes
cluster. For the component/architecture overview see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

> The `deploy/ycp-demo.yaml` manifest is a deliberate **no-registry demo**: pods
> run a stock public `alpine` image that waits for a binary injected via
> `kubectl cp`, then execs it. This keeps the demo runnable without a container
> registry or image build. It is **not** how the agent ships in production — that
> is a Helm chart in `meko-devops`.

## Prerequisites

- Go 1.25+ (to build the binaries).
- `kubectl` with access to the target cluster (for the k8s deploy).
- Optionally a Postgres DSN for durable control-plane state.
- For the build-and-deploy test only: `docker`, `kind` and `curl`.

## 1. Run locally

```bash
go run ./cmd/ycp -addr :8080          # console at http://localhost:8080
```

State is in-memory by default (a demo fleet is seeded on first use). For durable
state, point at Postgres:

```bash
go run ./cmd/ycp -addr :8080 -db "postgres://user:pass@host:5432/ycp?sslmode=disable"
# or: export YCP_DATABASE_URL=... && go run ./cmd/ycp
```

To exercise the real enrollment handshake locally, seed an environment's token:

```bash
YCP_SEED_ENV_NAME=itest-mcs YCP_SEED_ENV_TOKEN=mko_enr_local_demo go run ./cmd/ycp
```

Run the per-datapack MCP server on its own:

```bash
go run ./cmd/dpmcp -addr :3000          # POST /mcp (JSON-RPC), GET /healthz
```

## 2. Build static linux/amd64 binaries

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/ycp-linux-amd64  ./cmd/ycp
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/agent-linux-amd64 ./cmd/agent
```

## Container images

Three images, one per binary. They are built and pushed to
the platform ECR registry on every merge to `main` by
[`.github/workflows/ycp-images.yml`](../.github/workflows/ycp-images.yml), tagged
with both the commit SHA (pin this in a deployment) and `latest`. PRs build them
but never push.

| Dockerfile | Binary | Image |
|------------|--------|-------|
| `Dockerfile` | `cmd/ycp` (control plane) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/ycp` |
| `Dockerfile.agent` | `cmd/agent` (BYOC pull agent) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/meko-agent` |
| `Dockerfile.dpmcp` | `cmd/dpmcp` (per-datapack MCP) | `161908593886.dkr.ecr.us-east-2.amazonaws.com/meko/dpmcp` |

All three are `gcr.io/distroless/static:nonroot` at runtime — no shell, no
package manager, so `kubectl exec` into them will not give you a prompt.

To build locally:

```bash
docker build -f Dockerfile       -t ycp:dev        .
docker build -f Dockerfile.agent -t meko-agent:dev .
docker build -f Dockerfile.dpmcp -t dpmcp:dev      .
```

The `deploy/*.yaml` manifests deliberately do **not** use these images — they use
the registry-free `alpine` + `kubectl cp` shim described above. To deploy the real
images instead, apply the manifest through an overlay that swaps them in, the way
[`test/e2e/kustomization.yaml`](./test/e2e/kustomization.yaml) does.

## 3. Deploy to a cluster

`deploy/ycp-demo.yaml` creates: the `ycp-demo` namespace, the `meko-agent`
ServiceAccount, read-only + namespace-management RBAC, a shared enrollment-token
Secret, the `ycp` control-plane Deployment + Service, the `meko-agent`
Deployment, and demo Postgres for the control plane and a datapack.

```bash
CTX=gke_yugabyte_us-west1_itest-mcs
kubectl --context $CTX apply -f deploy/ycp-demo.yaml
```

Inject the binaries into the waiting alpine pods:

```bash
YCP_POD=$(kubectl --context $CTX -n ycp-demo get pod -l app=ycp -o jsonpath='{.items[0].metadata.name}')
AGENT_POD=$(kubectl --context $CTX -n ycp-demo get pod -l app=meko-agent -o jsonpath='{.items[0].metadata.name}')
kubectl --context $CTX -n ycp-demo cp dist/ycp-linux-amd64  $YCP_POD:/app/ycp
kubectl --context $CTX -n ycp-demo cp dist/agent-linux-amd64 $AGENT_POD:/app/agent
```

> `kubectl cp` is not persistent — if a pod restarts it re-enters its wait loop,
> so re-copy its binary.

## 4. Reach the console

The `ycp` Service is an **internal** LoadBalancer (GKE annotation
`networking.gke.io/load-balancer-type: Internal`), so GCP assigns a **private VPC
IP** — reachable from inside the VPC (bastion / VM / VPN), never the public
internet.

```bash
kubectl --context $CTX -n ycp-demo get svc ycp -w    # wait for EXTERNAL-IP (a private address)
IP=$(kubectl --context $CTX -n ycp-demo get svc ycp -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "console: http://$IP"     # reach it from a host inside the VPC
```

From a laptop outside the VPC, port-forward instead:

```bash
kubectl --context $CTX -n ycp-demo port-forward svc/ycp 8080:80
# open http://localhost:8080
```

> **Switching an existing external LB to internal:** GCP will not flip a live
> external forwarding rule to internal in place. Delete and recreate the Service:
> `kubectl -n ycp-demo delete svc ycp && kubectl apply -f deploy/ycp-demo.yaml`.
> Provisioning a fresh internal LB takes a few minutes.

## 5. Verify

```bash
# control plane is serving
curl -s http://$IP/healthz              # -> ok   (from inside the VPC)

# the itest-mcs row is LIVE (real facts reported by the agent)
kubectl --context $CTX -n ycp-demo logs deploy/meko-agent | tail
kubectl --context $CTX -n ycp-demo logs deploy/ycp | grep -i "agent enrolled"
```

The seeded environment's enrollment token lives in the `ycp-tokens` Secret; the
agent presents it to complete the handshake. On success the `ycp` logs show
`agent enrolled: env=itest-mcs …` and the console's `itest-mcs` row goes live.

## 6. Tear down

```bash
kubectl --context $CTX delete -f deploy/ycp-demo.yaml
```

This removes the internal LoadBalancer (and its GCP forwarding rule) along with
the namespace and all demo workloads.

## Variant: meko-dev-eks, exposed on the tailnet

`deploy/ycp-dev.yaml` runs the control plane + agent on the `meko-dev-eks` cluster
in the `ycp` namespace. Unlike the demo above it uses the **real ECR images**, so
there is no `kubectl cp` step — `apply` is the whole deploy. Instead of a cloud
LoadBalancer the console is published on the **tailnet** via the cluster's
Tailscale operator — the `ycp` Service carries:

```yaml
annotations:
  tailscale.com/expose: "true"
  tailscale.com/hostname: "ycp-dev"
  tailscale.com/tags: "tag:k8s-meko"
```

```bash
CTX=meko-dev-eks
kubectl --context $CTX apply -f deploy/ycp-dev.yaml
kubectl --context $CTX -n ycp rollout status deploy/ycp deploy/meko-agent
```

The operator provisions a proxy (`ts-ycp-*` in the `tailscale` namespace) and the
console becomes reachable at `ycp-dev.<tailnet>.ts.net` — tailnet-only, not the
public internet. The agent enrolls the meko-dev cluster, so its row goes live:

```bash
kubectl --context $CTX -n ycp logs deploy/ycp | grep "agent enrolled"
# agent enrolled: env=meko-dev provider=aws k8s=v1.33.13-eks-8f14419 nodes=17

kubectl --context $CTX -n ycp port-forward svc/ycp 8080:80   # or use the tailnet host
curl -s localhost:8080/api/v1/environments | jq '.items[] | {id, state, live}'
```

The images are `:latest` with `imagePullPolicy: Always`, so a restart picks up
whatever `ycp-images.yml` last pushed. Pin the SHA tag in the manifest for
anything that has to be reproducible.

Nothing here touches `meko-system` — the dev data plane (its own `meko-api-server`,
`meko-ui`, `inference-gateway`) lives there and is deployed separately.

## Tests

Unit tests need nothing running:

```bash
go test ./...
```

The build-and-deploy test is the end-to-end one. It builds all three images from
source, loads them into a throwaway `kind` cluster, applies
`deploy/ycp-demo.yaml` through the `test/e2e` overlay, and asserts that every
workload rolls out, `/healthz` returns `ok`, the agent completes the real
enrollment handshake, and the environment shows up on `GET /api/v1/environments`:

```bash
./test/e2e/run.sh                  # create, test, tear down
YCP_E2E_KEEP=1 ./test/e2e/run.sh   # leave the cluster up to poke at
```

It creates and deletes its own cluster (`kind-ycp-e2e`) and pins every `kubectl`
call to that context, refusing to run if the context does not resolve to the kind
node it just created — so it cannot touch whatever cluster your kubeconfig
happens to point at. On failure it dumps pod state, container logs and events.

Knobs: `YCP_E2E_CLUSTER`, `YCP_E2E_TAG`, `YCP_E2E_PORT`, `YCP_E2E_TIMEOUT`,
`YCP_E2E_KEEP`.

Both run in CI on any PR touching `ycp/` — see the `meko_ycp` and `meko_ycp_e2e`
jobs in [`pr-build.yml`](../.github/workflows/pr-build.yml).

## Configuration reference

**`ycp`** (control plane)

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `-addr` | `:8080` | Listen address. |
| `-db` / `YCP_DATABASE_URL` | _(empty)_ | Postgres DSN; empty = in-memory store. |
| `YCP_SEED_ENV_NAME` | _(unset)_ | Pre-register this environment name for the demo handshake. |
| `YCP_SEED_ENV_TOKEN` | _(unset)_ | Enrollment token to seed for `YCP_SEED_ENV_NAME` (re-armed every 10m). |

**`dpmcp`** (per-datapack MCP server)

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `-addr` | `:3000` | Listen address. |
| `-db` / `DP_DB_URL` | _(empty)_ | Datapack Postgres DSN; empty = in-memory store. |

The `meko-agent` is configured via environment (YCP URL, enrollment token,
environment name) set in `deploy/ycp-demo.yaml`; all its traffic is outbound.
