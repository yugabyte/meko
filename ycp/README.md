# YCP — minimal Yugabyte Control Plane (demo)

A runnable, stdlib-only implementation of the BYOC control plane described in
`design/byoc-control-plane.md`, backing `design/ycp-openapi.yaml`.

- **`cmd/ycp`** — in-memory control-plane API + embedded console (served at `/`).
- **`cmd/agent`** — minimal `meko-agent`: reads real cluster facts from the
  in-cluster Kubernetes REST API (no client-go), enrolls the environment into
  YCP, and reports status on an interval. All traffic is outbound.
- **`web/console.html`** — the operator console, wired to the live API.
- **`deploy/ycp-demo.yaml`** — namespace + read-only RBAC + two deployments.

No external Go dependencies; no container registry needed. Pods run a stock
public `alpine` image that waits for a binary injected via `kubectl cp`, then
execs it.

> **See also:** [`ARCHITECTURE.md`](./ARCHITECTURE.md) for the component /
> pull-loop design, and [`DEPLOY.md`](./DEPLOY.md) for the full deployment guide
> (local, cluster, internal LoadBalancer, teardown, config reference). The quick
> steps below are a condensed version of `DEPLOY.md`.

## Run locally

```bash
go run ./cmd/ycp -addr :8080      # open http://localhost:8080
```

## Build static linux/amd64 binaries

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/ycp-linux-amd64  ./cmd/ycp
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/agent-linux-amd64 ./cmd/agent
```

## Deploy to a cluster (example: itest-mcs)

```bash
CTX=gke_yugabyte_us-west1_itest-mcs
kubectl --context $CTX apply -f deploy/ycp-demo.yaml

# inject binaries into the alpine pods (they wait-then-exec)
YCP_POD=$(kubectl --context $CTX -n ycp-demo get pod -l app=ycp -o jsonpath='{.items[0].metadata.name}')
AGENT_POD=$(kubectl --context $CTX -n ycp-demo get pod -l app=meko-agent -o jsonpath='{.items[0].metadata.name}')
kubectl --context $CTX -n ycp-demo cp dist/ycp-linux-amd64  $YCP_POD:/app/ycp
kubectl --context $CTX -n ycp-demo cp dist/agent-linux-amd64 $AGENT_POD:/app/agent
```

> Note: `kubectl cp` is not persistent — if a pod restarts, re-copy its binary
> (the container re-enters its wait loop). This is a deliberate no-registry
> shortcut for the demo, not how the real agent ships (that's a Helm chart in
> `meko-devops`).

## View the console

The `ycp` Service is an **internal** LoadBalancer (GKE annotation
`networking.gke.io/load-balancer-type: Internal`), so GCP assigns a private IP
reachable only from within the cluster's VPC — not the public internet:

```bash
kubectl --context $CTX -n ycp-demo get svc ycp -w   # wait for EXTERNAL-IP (a private VPC address)
IP=$(kubectl --context $CTX -n ycp-demo get svc ycp -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
# reach http://$IP from a host inside the VPC (bastion, VM, VPN); the itest-mcs row is LIVE
```

To reach it from your laptop (outside the VPC), port-forward instead:

```bash
kubectl --context $CTX -n ycp-demo port-forward svc/ycp 8080:80
# open http://localhost:8080
```

## Tear down

```bash
kubectl --context $CTX delete -f deploy/ycp-demo.yaml
```
