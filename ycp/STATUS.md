# YCP — current state

Checkpoint as of 2026-07-29. Snapshot of what is deployed and what is open; for how
to deploy see [`GETTING_STARTED.md`](./GETTING_STARTED.md) and
[`DEPLOY.md`](./DEPLOY.md), and for the design see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

## There are two implementations

| | What it is |
|---|---|
| `api_server --control-plane-mode` | **Production YCP.** Deployed by the `meko` chart in meko-devops (`-f values-ycp.yaml`). Serves the fleet API and, as of meko#400, the console |
| `ycp/cmd/ycp` | **Reference implementation.** Same contract, no Cognito/S3/Langfuse. What the kind e2e runs and what a standalone demo uses. Not for real tenants |

They drifted once already — `api_server` wraps responses as `{"data":…,"error":…}`
while `cmd/ycp` returns them flat, which broke the agent (meko#399). Anything
touching the agent contract needs checking against both.

## Deployed (meko-dev-eks)

| Namespace | Contents |
|---|---|
| `ycp-system` | `meko-api-server` (control-plane mode) + `ycp-postgres` StatefulSet, Helm release `ycp` |
| `ycp-agent` | `meko-agent`, Helm release `meko-agent`, enrolled as environment `meko-dev` |

- Console + API: **http://ycp.tail5955.ts.net** — tailnet only (device tagged
  `tag:k8s-meko`), because the console has no auth of its own. Off-tailnet:
  `kubectl -n ycp-system port-forward svc/meko-api-server-service 8080:80`.
- Fleet endpoints need `X-Meko-Admin-Secret`; the value is the `meko-admin-api-key`
  Secret in `ycp-system`. Without it they return 401.
- Both releases currently run **`pr400-86b3b52`** — branch builds of unmerged
  meko#400, deliberately not version-tagged. Roll back the control plane with
  `--set-string apiServer.image.tag=1.0.35-20260728`.
- The `ycp-tailnet` Service is **hand-created**, not chart-managed (the chart has no
  annotation hook on the ClusterIP Service). It will not come back from a
  rebuild-from-chart. See meko-devops#42.
- Control-plane state is a bundled Postgres, not the shared dev YugabyteDB. The
  api_server system migrations need YugabyteDB-only DDL, so control-plane mode runs
  its own portable set (`migrations/controlplane`, `-only-controlplane-migrate`).

## Images

`meko/ycp`, `meko/meko-agent` and `meko/dpmcp` publish to
`161908593886.dkr.ecr.us-east-2.amazonaws.com` automatically on merge to `main`
(`.github/workflows/ycp-images.yml`, OIDC — no secrets).

`meko/meko-api-server` does **not**: nothing in CI builds it, and meko-devops only
deploys and promotes. It is hand-built. That already caused two false starts — an
image without `--control-plane-mode`, then one without `-only-controlplane-migrate`.

## Open

| | |
|---|---|
| meko#400 | Console + fleet endpoints + the agent envelope fix. Green, unmerged. Closes meko#399 |
| meko#401 | **Agent cannot re-enroll >30min after registration** — enrollment-token TTL. A pod restart is unrecoverable without an operator. Worth fixing first |
| meko-devops#42 | Chart ergonomics: no `mode: agent`, namespaces/cognito/storage-class need `--set`, Secrets are out-of-band |

Known gaps not yet filed: the `dpmcp` image can't be pulled in the `ycp-dp-*`
namespaces the agent creates (no pull credentials there, and ECR has no anonymous
pull), so `agent.dpmcpImage` stays a public placeholder; and `console.html` is
duplicated between `ycp/web/` and `api_server/internal/handlers/web/` because
`go:embed` cannot cross module boundaries.

## Next

1. meko#401 — the only correctness bug of the three.
2. Merge meko#400, then rebuild the api-server image from `main` so the deploy is
   not running branch images.
3. meko-devops#42 — `mode: agent` first; it is what makes the install commands
   short enough to put in a runbook.
