# ac — YCP fleet registration & reporting

**Status:** Draft
**Last updated:** 2026-08-03

How the amp controller registers itself with **YCP** (the Yugabyte Control Plane
— meko's `api_server` run with `--control-plane-mode`) and reports the resources
it owns, so a fleet operator sees every amp cluster and its inventory in one
console.

This is the **register + report** half only. Reconciling YCP-declared desired
state back into the cluster (YCP telling an amp controller to create a pool or a
branch) is a **later phase** and nothing here reads desired state.

## Shape

YCP already runs a fleet registry and an agent protocol for meko environments
(the datapack/stack agent enrolls, gets a status token, and heartbeats). The amp
controller reuses that same protocol rather than inventing one:

```
  amp controller (in the customer cluster)          YCP (api_server --control-plane-mode)
  ┌───────────────────────────────┐                 ┌──────────────────────────────┐
  │ ycp.Loop (background goroutine)│                 │ POST /api/v1/agent/enroll     │
  │  1. ensureIdentity ────────────┼── enroll ──────►│   (kind=amp, enrollment token)│
  │       (resume, else enroll)    │◄── status token │                               │
  │  2. every --ycp-report-interval│                 │ POST /api/v1/agent/status     │
  │     BuildInventory ────────────┼── report ──────►│   (resources[])               │
  └───────────────────────────────┘                 └──────────────────────────────┘
      state store: ycp_identity                          environments.kind = 'amp'
      (migration 0004)                                    environments.resources = [...]
```

### Environment `kind`

An environment is registered as **`amp`** or **`meko`**. The kind is fixed at
registration. A meko id stays a bare `slug(name)` (unchanged — the existing meko
agent/e2e/console depend on it); an amp id is prefixed **`amp-`**, so an amp and a
meko environment of the same name never collide and the two can coexist in one
fleet (the `kind` column disambiguates regardless). Enrollment carries the kind and YCP rejects a mismatch (409), so an amp
controller cannot attach to a meko environment or vice versa. An amp environment
seeds **no** desired state at registration (there is no meko-stack to bring up —
it is a running controller reporting what it already owns).

### Enroll → status token, and resuming

`ycp.Loop.ensureIdentity` resumes a persisted identity if one exists, and
enrolls otherwise:

- **Enroll** exchanges the one-time enrollment token (30-min TTL, delivered via a
  mounted Secret) for a long-lived, environment-scoped **status token**, and
  persists both in the controller's own schema (`ycp_identity`, migration 0004).
- **Restart** loads that identity and reports directly. It does **not** re-enroll
  — the enrollment token is short-lived and may be gone. A 401 on a report (the
  status token was rotated/invalidated) is the one case that triggers a re-enroll.

### Reporting the inventory

On each tick `ycp.BuildInventory` walks the resource model through `core.Backend`
— storage pools → projects ("databases") → branches → compute endpoints — and
flattens it into a list of `ResourceStatus` (kind, id, name, parent, state,
attrs), linked by `parent` id. That list rides the status heartbeat; YCP stores
it verbatim on `environments.resources`, and the console renders it as a tree.

## Configuration

Off by default. `--ycp` (env `AMP_YCP_ENABLED`) turns it on; then `--ycp-url` and
`--ycp-env-name` are required. `--ycp-enrollment-token` is delivered via a Secret
and is optional once the status token has been persisted. The `yb-amp-controller`
Helm chart exposes this as `controller.ycp.*` and mounts the enrollment token
from a Secret (`controller.ycp.enrollTokenSecret`).

A YugabyteDB foundation cluster is a prerequisite regardless of YCP: it is the
controller's own state store, and where `ycp_identity` lives.

## Deploying YCP itself

The control plane is `api_server --control-plane-mode`. Its image is built and
pushed on demand by `meko/api_server/build-image.sh` (Quay:
`quay.io/yugabyte/ycp-api-server`), wrapped by the
`.github/workflows/meko-control-plane-image.yml` workflow on `main`.

## Testing

Unit coverage lives beside the code: the client envelope/error handling
(`client_test.go`), the inventory flattening (`inventory_test.go`), the
enroll/resume loop (`loop_test.go`), and the config validation
(`config_ycp_test.go`); on the control-plane side, the `kind`-qualified id, enroll
kind-mismatch, and `resources` round-trip (`meko/api_server/internal/service` and
`.../repository`).

The end-to-end guard is the opt-in **`/ci amp_deploy_gke_ycp`** directive
([`amp-control-plane-gke.yml`](../../../.github/workflows/amp-control-plane-gke.yml),
see [`amp/docs/ci-directives.md`](../../docs/ci-directives.md)): it stands up a real
control plane next to a real amp deploy on GKE, registers this environment, and
asserts that an endpoint the smoke test creates through the amp API shows up in the
control plane's `GET /api/v1/environments` inventory — the register → enroll →
report → store path, exercised against the shipped images.

## Deferred

- YCP → controller **desired state** (reconcile): YCP declaring resources for an
  amp environment and the controller converging to them.
- Cluster facts beyond the basics (the report currently sends an empty
  `ClusterFacts`).
