# YCP — the BYOC data-plane side

YCP (Yugabyte Control Plane) is Meko's BYOC control plane. **The control plane
itself is `api_server` run with `--control-plane-mode`**, not code in this
directory. What lives here is the data-plane half of that contract:

- **`cmd/agent`** — `meko-agent`: reads real cluster facts from the in-cluster
  Kubernetes REST API (no client-go), enrolls the environment into YCP, reconciles
  the environment's declared stack, and reports status on an interval. All traffic is outbound.
- **`internal/api`** — the wire types and lifecycle states both sides share.
- **`test/e2e`** — build-and-deploy smoke test on kind, against the real control
  plane.

A reference implementation of the control plane used to sit in `cmd/ycp`; it was
removed. [`ARCHITECTURE.md`](./ARCHITECTURE.md) explains why and where each piece
of the contract is implemented now.

## Build and test

```bash
go build ./...
go test ./...                 # no database or cluster needed
ycp/test/e2e/run.sh           # build + deploy on kind; needs docker + kind
```

## Deploy

Both images ship to the platform ECR registry and deploy through the `meko` Helm
chart at [`meko/deploy/helm/meko`](../meko/deploy/helm/meko):

| What | How |
|------|-----|
| The control plane | `-f values-ycp.yaml` — see [`YCP-RUNBOOK.md`](../meko/deploy/helm/meko/YCP-RUNBOOK.md) |
| An agent in a data plane | `-f values-agent.yaml` — same runbook, §4 |

[`DEPLOY.md`](./DEPLOY.md) covers what is specific to this directory: which
Dockerfile builds which image, how the agent is configured, and how it is
operated once attached.

> **See also:** [`ARCHITECTURE.md`](./ARCHITECTURE.md) for the pull-loop design
> and the api-key auth model, [`BYOC-ON-GKE.md`](./BYOC-ON-GKE.md) to stand a whole data
> plane up on a real cluster start to finish, and [`STATUS.md`](./STATUS.md) for
> what works today and what is still open.
