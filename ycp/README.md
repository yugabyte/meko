# YCP — the BYOC data-plane side

YCP (Yugabyte Control Plane) is Meko's BYOC control plane. **The control plane
itself is `api_server` run with `--control-plane-mode`**, not code in this
directory. What lives here is the data-plane half of that contract:

- **`cmd/agent`** — `meko-agent`: reads real cluster facts from the in-cluster
  Kubernetes REST API (no client-go), enrolls the environment into YCP, reconciles
  desired datapacks, and reports status on an interval. All traffic is outbound.
- **`cmd/dpmcp`** — the per-datapack MCP server the agent provisions.
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
chart in [meko-devops](https://github.com/yugabyte/meko-devops):

| What | How |
|------|-----|
| The control plane | `charts/meko -f values-ycp.yaml` — see `charts/meko/YCP-RUNBOOK.md` |
| An agent in a data plane | `charts/meko -f values-agent.yaml` — same runbook, §4 |

[`DEPLOY.md`](./DEPLOY.md) covers what is specific to this directory: which
Dockerfile builds which image, how the agent is configured, and how it is
operated once attached.

> **See also:** [`ARCHITECTURE.md`](./ARCHITECTURE.md) for the pull-loop design
> and the token model, and [`STATUS.md`](./STATUS.md) for what works today and
> what is still open.
