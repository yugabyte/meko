# YCP — current state

Checkpoint as of 2026-07-29. Snapshot of what is deployed and what is open; for how
to deploy see [`charts/meko/YCP-RUNBOOK.md`](https://github.com/yugabyte/meko-devops/blob/main/charts/meko/YCP-RUNBOOK.md)
in meko-devops and [`DEPLOY.md`](./DEPLOY.md), and for the design see
[`ARCHITECTURE.md`](./ARCHITECTURE.md).

## There is one implementation

`api_server --control-plane-mode` is YCP. It is deployed by the `meko` chart in
meko-devops (`-f values-ycp.yaml`) and serves the fleet API, the agent API and the
operator console.

There were two until this change. `ycp/cmd/ycp` was a reference implementation of
the same contract — no Cognito/S3/Langfuse, in-memory registry — used by the demo
manifests and the kind e2e. It is gone, along with `internal/server`,
`internal/store`, `web/`, `Dockerfile` and `deploy/`. Two implementations of one
contract earned their keep exactly once and cost more than that: they drifted on
the response envelope (`{"data":…}` vs flat), which broke the agent
([meko#399](https://github.com/yugabyte/meko/issues/399)) while the e2e stayed
green — because the e2e was testing the copy nobody deploys.

`test/e2e/run.sh` now stands up `api_server --control-plane-mode` on a plain
Postgres in kind and drives register → enroll → READY → **restart without
re-enrolling**. That last assertion is the one that would have caught
[meko#401](https://github.com/yugabyte/meko/issues/401).

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
- Control-plane state is a bundled Postgres, not the shared dev YugabyteDB. The
  api_server system migrations need YugabyteDB-only DDL, so control-plane mode runs
  its own portable set (`migrations/controlplane`, `-only-controlplane-migrate`).
- The tailnet Service is chart-managed now, via `apiServer.service.annotations`
  (meko-devops#43) — it survives a rebuild from the chart.
- Both releases were last deployed from branch builds of what is now merged. Rebuild
  the api-server image from `main` before treating the deploy as a release.

## Images

`meko/meko-agent` and `meko/dpmcp` publish to
`161908593886.dkr.ecr.us-east-2.amazonaws.com` automatically on merge to `main`
(`.github/workflows/ycp-images.yml`, OIDC — no secrets). `meko/ycp` is no longer
built; nothing deploys it.

`meko/meko-api-server` does **not** publish from CI: nothing in the repo builds it,
and meko-devops only deploys and promotes. It is hand-built. That already caused two
false starts — an image without `--control-plane-mode`, then one without
`-only-controlplane-migrate`. The e2e now builds it from source on every PR touching
`ycp/`, so the flags at least cannot silently disappear.

## Open

| | |
|---|---|
| meko-devops#42 | Chart ergonomics, remainder. `mode: agent`, the mode presets and the runbook landed (#40, #41, #43, #47); Secrets are still out-of-band and the storage class is still per-cluster |
| meko#401 follow-ups | Closed for the lockout itself (the agent persists its status token, #403). Still open in the design: no CA pinning, no status-token rotation, and `cert_fingerprint` is decorative |
| api-server image builds | Nothing publishes `meko/meko-api-server`. The control plane is the one YCP component whose image is hand-built |

Known gaps not yet filed:

- The `dpmcp` image cannot be pulled in the `ycp-dp-*` namespaces the agent creates
  (no pull credentials there, and ECR has no anonymous pull), so `agent.dpmcpImage`
  stays a public placeholder.
- `console.html` is duplicated between the retired `ycp/web/` and
  `api_server/internal/handlers/web/` — with `ycp/web/` deleted this is now a single
  copy, which is the fix.
- Agent defaults are demo residue: `ENV_NAME=itest-mcs`, `ENV_PROVIDER=gke`,
  `ENV_REGION=us-west1`, `AGENT_VERSION=v1.4.2` hardcoded, and `DPMCP_IMAGE`
  defaulting to `postgrest/postgrest`. The chart sets all of them, so nothing is
  broken; an unset one enrolls as the wrong environment rather than failing.
- On a transient control-plane error the agent used to garbage-collect every
  `ycp-dp-*` namespace and drop every `dp_*` database; the desired-state guard now
  skips the tick instead. The GC is still unconditional on a *successful* empty
  desired state, which is correct but unforgiving.

## Next

1. Rebuild and push `meko/meko-api-server` from `main`, then redeploy `ycp-system`
   off a real tag instead of a branch build.
2. Wire an api-server image build into CI so (1) is not a manual step.
3. meko-devops#42 remainder — Secrets, and a storage class that is not per-cluster
   guesswork.
