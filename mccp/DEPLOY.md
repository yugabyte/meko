# Deploying MCCP

This guide covers running MCCP locally and deploying the demo to a Kubernetes
cluster. For the component/architecture overview see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

> The `deploy/mccp-demo.yaml` manifest is a deliberate **no-registry demo**: pods
> run a stock public `alpine` image that waits for a binary injected via
> `kubectl cp`, then execs it. This keeps the demo runnable without a container
> registry or image build. It is **not** how the agent ships in production — that
> is a Helm chart in `meko-devops`.

## Prerequisites

- Go 1.25+ (to build the binaries).
- `kubectl` with access to the target cluster (for the k8s deploy).
- Optionally a Postgres DSN for durable control-plane state.

## 1. Run locally

```bash
go run ./cmd/mccp -addr :8080          # console at http://localhost:8080
```

State is in-memory by default (a demo fleet is seeded on first use). For durable
state, point at Postgres:

```bash
go run ./cmd/mccp -addr :8080 -db "postgres://user:pass@host:5432/mccp?sslmode=disable"
# or: export MCCP_DATABASE_URL=... && go run ./cmd/mccp
```

To exercise the real enrollment handshake locally, seed an environment's token:

```bash
MCCP_SEED_ENV_NAME=itest-mcs MCCP_SEED_ENV_TOKEN=mko_enr_local_demo go run ./cmd/mccp
```

Run the per-datapack MCP server on its own:

```bash
go run ./cmd/dpmcp -addr :3000          # POST /mcp (JSON-RPC), GET /healthz
```

## 2. Build static linux/amd64 binaries

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/mccp-linux-amd64  ./cmd/mccp
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/agent-linux-amd64 ./cmd/agent
```

## 3. Deploy to a cluster

`deploy/mccp-demo.yaml` creates: the `mccp-demo` namespace, the `meko-agent`
ServiceAccount, read-only + namespace-management RBAC, a shared enrollment-token
Secret, the `mccp` control-plane Deployment + Service, the `meko-agent`
Deployment, and demo Postgres for the control plane and a datapack.

```bash
CTX=gke_yugabyte_us-west1_itest-mcs
kubectl --context $CTX apply -f deploy/mccp-demo.yaml
```

Inject the binaries into the waiting alpine pods:

```bash
MCCP_POD=$(kubectl --context $CTX -n mccp-demo get pod -l app=mccp -o jsonpath='{.items[0].metadata.name}')
AGENT_POD=$(kubectl --context $CTX -n mccp-demo get pod -l app=meko-agent -o jsonpath='{.items[0].metadata.name}')
kubectl --context $CTX -n mccp-demo cp dist/mccp-linux-amd64  $MCCP_POD:/app/mccp
kubectl --context $CTX -n mccp-demo cp dist/agent-linux-amd64 $AGENT_POD:/app/agent
```

> `kubectl cp` is not persistent — if a pod restarts it re-enters its wait loop,
> so re-copy its binary.

## 4. Reach the console

The `mccp` Service is an **internal** LoadBalancer (GKE annotation
`networking.gke.io/load-balancer-type: Internal`), so GCP assigns a **private VPC
IP** — reachable from inside the VPC (bastion / VM / VPN), never the public
internet.

```bash
kubectl --context $CTX -n mccp-demo get svc mccp -w    # wait for EXTERNAL-IP (a private address)
IP=$(kubectl --context $CTX -n mccp-demo get svc mccp -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "console: http://$IP"     # reach it from a host inside the VPC
```

From a laptop outside the VPC, port-forward instead:

```bash
kubectl --context $CTX -n mccp-demo port-forward svc/mccp 8080:80
# open http://localhost:8080
```

> **Switching an existing external LB to internal:** GCP will not flip a live
> external forwarding rule to internal in place. Delete and recreate the Service:
> `kubectl -n mccp-demo delete svc mccp && kubectl apply -f deploy/mccp-demo.yaml`.
> Provisioning a fresh internal LB takes a few minutes.

## 5. Verify

```bash
# control plane is serving
curl -s http://$IP/healthz              # -> ok   (from inside the VPC)

# the itest-mcs row is LIVE (real facts reported by the agent)
kubectl --context $CTX -n mccp-demo logs deploy/meko-agent | tail
kubectl --context $CTX -n mccp-demo logs deploy/mccp | grep -i "agent enrolled"
```

The seeded environment's enrollment token lives in the `mccp-tokens` Secret; the
agent presents it to complete the handshake. On success the `mccp` logs show
`agent enrolled: env=itest-mcs …` and the console's `itest-mcs` row goes live.

## 6. Tear down

```bash
kubectl --context $CTX delete -f deploy/mccp-demo.yaml
```

This removes the internal LoadBalancer (and its GCP forwarding rule) along with
the namespace and all demo workloads.

## Variant: meko-dev-eks, exposed on the tailnet

`deploy/mccp-dev.yaml` is the same demo (control plane + agent, binaries injected
via `kubectl cp`) targeted at the `meko-dev-eks` cluster in a fresh `mccp`
namespace, but instead of a cloud LoadBalancer the console is published on the
**tailnet** via the cluster's Tailscale operator — the `mccp` Service carries:

```yaml
annotations:
  tailscale.com/expose: "true"
  tailscale.com/hostname: "mccp-dev"
  tailscale.com/tags: "tag:k8s-meko"
```

```bash
CTX=meko-dev-eks
kubectl --context $CTX apply -f deploy/mccp-dev.yaml

# inject the linux/amd64 binaries (same wait-then-exec pattern)
MCCP_POD=$(kubectl --context $CTX -n mccp get pod -l app=mccp -o jsonpath='{.items[0].metadata.name}')
AGENT_POD=$(kubectl --context $CTX -n mccp get pod -l app=meko-agent -o jsonpath='{.items[0].metadata.name}')
kubectl --context $CTX -n mccp cp dist/mccp-linux-amd64  $MCCP_POD:/app/mccp
kubectl --context $CTX -n mccp cp dist/agent-linux-amd64 $AGENT_POD:/app/agent
```

The operator provisions a proxy (`ts-mccp-*` in the `tailscale` namespace) and the
console becomes reachable at `mccp-dev.<tailnet>.ts.net` — tailnet-only, not the
public internet. The agent enrolls the meko-dev cluster, so its row goes live.

## Configuration reference

**`mccp`** (control plane)

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `-addr` | `:8080` | Listen address. |
| `-db` / `MCCP_DATABASE_URL` | _(empty)_ | Postgres DSN; empty = in-memory store. |
| `MCCP_SEED_ENV_NAME` | _(unset)_ | Pre-register this environment name for the demo handshake. |
| `MCCP_SEED_ENV_TOKEN` | _(unset)_ | Enrollment token to seed for `MCCP_SEED_ENV_NAME` (re-armed every 10m). |

**`dpmcp`** (per-datapack MCP server)

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `-addr` | `:3000` | Listen address. |
| `-db` / `DP_DB_URL` | _(empty)_ | Datapack Postgres DSN; empty = in-memory store. |

The `meko-agent` is configured via environment (MCCP URL, enrollment token,
environment name) set in `deploy/mccp-demo.yaml`; all its traffic is outbound.
