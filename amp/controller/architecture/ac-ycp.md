# ac — YCP integration (reconcile-agent)

**Status:** Draft
**Last updated:** 2026-08-18

How an amp controller is driven by **YCP** (the Yugabyte Control Plane — meko's
`api_server` run with `--control-plane-mode`), so a fleet operator declares an amp
environment's resources in one console and sees the inventory converge there.

The controller does **not** talk to YCP itself. A standalone **reconcile-agent**
(a separate pod, code in `ycp/cmd/reconcile-agent` + `ycp/reconcile`) is the sole
YCP client for the environment: it enrolls, pulls YCP's desired state, and drives
this controller over its REST API to converge, reporting the resulting inventory
back on each heartbeat. The controller's only YCP-specific surface is the
write-once binding endpoint the agent pins itself to.

```
  reconcile-agent (in the customer cluster)     amp controller            YCP (control-plane mode)
  ┌─────────────────────────────────┐           ┌──────────────┐          ┌────────────────────────┐
  │ enroll (kind=amp) ──────────────┼───────────┼──────────────┼─────────►│ POST /agent/enroll     │
  │ pin binding ────────────────────┼──────────►│ POST /ycp/    │          │  -> status token       │
  │                                 │           │      binding  │          │                        │
  │ every reconcile interval:       │           │              │          │ GET  /agent/desired... │
  │  1. GET desired state ──────────┼───────────┼──────────────┼─────────►│                        │
  │  2. diff vs. controller List ───┼──────────►│ REST /api/v1 │          │                        │
  │  3. create/update/delete ───────┼──────────►│  (async task)│          │                        │
  │  4. report inventory ───────────┼───────────┼──────────────┼─────────►│ POST /agent/status     │
  └─────────────────────────────────┘           └──────────────┘          └────────────────────────┘
                                                  ycp_binding                environments.kind='amp'
                                                  (migration 0007)           environments.resources=[…]
```

The engine, the amp backend (how desired-state kinds map onto amp's
pool → cluster → branch → endpoint hierarchy), and the enroll/report client all
live in the `ycp` module (`ycp/reconcile`, `ycp/reconcile/ampbackend`,
`ycp/internal/ycpclient`). The agent is authoritative: it garbage-collects any
controller resource not present in desired state.

## Convergence status

Each heartbeat carries per-resource health, not just the inventory, so an
operator sees *why* a resource has not converged without reading agent or
controller logs. The engine distinguishes three cases and the agent folds each
onto the resource's `ResourceStatus.Error` (`ycp/internal/api`):

- **Failed** — a retryable rejection (the controller refused a create/resize, a
  task failed). Retried on backoff; makes the environment `DEGRADED`.
- **Stuck** — a resource sat non-Ready in the same backend state past
  `Options.StuckAfter` (default 10m), e.g. an endpoint that never provisions.
  Also `DEGRADED`, with a "no progress" reason.
- **Permanent drift** — a verb the kind structurally does not support: a storage
  pool has no delete, and pools/clusters/branches have no update. The engine
  reports it once and stops re-issuing it (no backoff flap) rather than treating
  an unfixable call as a failure; it does **not** by itself degrade the
  environment. Backends flag these with `reconcile.Permanent`.

The agent binds the controller with capped retry/backoff at startup (a binding
*conflict* is still fatal), so a fresh install where the controller is not yet up
does not crash-loop the agent pod.

## Environment `kind`

An environment is registered as **`amp`** or **`meko`**. The kind is fixed at
registration. A meko id stays a bare `slug(name)`; an amp id is prefixed **`amp-`**,
so an amp and a meko environment of the same name never collide and the two coexist
in one fleet. Enrollment carries the kind and YCP rejects a mismatch (409), so the
agent cannot attach to a meko environment. An amp environment seeds **no** desired
state at registration — an operator declares its resources afterwards through
`POST /environments/{id}/desired`.

## Write-once binding

The controller records the single YCP environment it belongs to and refuses to be
rebound to a different one, so a second agent pointed at another control plane
cannot quietly take it over.

- **`POST /api/v1/ycp/binding`** `{ycp_url, environment_id}` — the reconcile-agent
  pins the controller after it enrolls. First write wins; an identical re-bind is
  idempotent (200); a bind that differs from the incumbent is rejected (409, body
  carries the incumbent). **`GET`** returns the current binding or 404.
- State lives in `ycp_binding` (migration 0007), a singleton row in the
  controller's own schema. The routes sit outside the OpenAPI spec, like
  `/healthz` and `/metrics` — the binding is operational state, not a resource.

Request-time enforcement (gating mutating calls on the binding) is deferred; today
the binding is a durable record and a bind-time tripwire.

## History: the retired outbound loop

The controller used to enroll and report to YCP itself, on a background goroutine
(`ycp.Loop`) with its identity persisted in `ycp_identity` (migration 0004). That
was the "register + report" half of a design that always intended reconciliation
to follow. When the standalone agent landed — which must be the *sole* YCP client
(YCP mints one status token per environment) — the controller loop was retired
(migration 0008 drops `ycp_identity`; the `--ycp-*` flags and `controller.ycp.*`
Helm values are gone). The binding endpoint is all that remains on the controller.

## Deploying YCP itself

The control plane is `api_server --control-plane-mode`. Its image is built and
pushed on demand (Quay: `quay.io/yugabyte/ycp-api-server`). The reconcile-agent
image is `quay.io/yugabyte/ycp-reconciler-agent` (`ycp/Dockerfile.reconcile-agent`).

## Testing

The controller-side unit coverage is the binding store + endpoint
(`internal/ycp/binding.go`, `internal/api/ycp_binding_test.go`) and, on the
control-plane side, the `kind`-qualified id, enroll kind-mismatch, and desired /
`resources` round-trip (`meko/api_server/internal/service` and `.../repository`).
The engine, backend mapping, and agent live in the `ycp` module and are tested
there.

The end-to-end guard is the opt-in **`/ci amp_deploy_gke_ycp`** directive
([`amp-control-plane-gke.yml`](../../../.github/workflows/amp-control-plane-gke.yml),
see [`amp/docs/ci-directives.md`](../../docs/ci-directives.md)): it stands up a real
control plane next to a real amp deploy on GKE, deploys the reconcile-agent, and
runs [`ycp-reconcile-smoke.sh`](../../deploy/ycp-reconcile-smoke.sh) — declare a
pool → cluster → branch → endpoint through YCP desired state, assert the agent
creates the tree, reports it `READY`, and GCs it on withdrawal — against the
shipped images.

## Deferred

- Request-time binding enforcement (gating mutating controller calls on the bind).
- Cluster facts beyond the basics in the agent's status report.
- A meko backend for the same agent (selected by environment kind), retiring the
  standalone meko stack agent.
