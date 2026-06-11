---
status: pending
severity: high
area: yblc
opened: 2026-06-11
---

# Kubernetes deployment mechanism for endpoints

## Goal

Per yblight-controller principle 3 (Kubernetes-first, multi-runtime by design)
and §5.2 / §5.4, an endpoint is a single self-contained blob — compute
(Postgres), safekeeper, pageserver, `compute_ctl`, and an otel collector —
deployed as **one pod per endpoint** on Kubernetes. This task delivers the two
artifacts that make that real:

1. an **endpoint container image** bundling all per-endpoint services, and
2. a **Helm chart** that deploys the endpoint pod, with a **ConfigMap** carrying
   the `--config` spec values for `compute_ctl`.

## Scope

### In scope

- **Endpoint image (Dockerfile).** A single image bundling the per-endpoint
  services (§5.2): `compute_ctl` + the patched Postgres v17 (`vendor/postgres-v17`
  + the `pgxn/neon` extension), `safekeeper`, `pageserver`, and an otel collector.
  `compute_ctl` is the in-pod supervisor and single point of contact (see
  [`compute_ctl.md`](../architecture/compute_ctl.md)) — but note it does **not**
  fork the SK/PS today, so the supervision model is a decision point: `compute_ctl`
  spawns the siblings, a thin init supervises them, or they run as sidecar
  containers in the same pod.
- **Helm chart.** Templates a Deployment/StatefulSet (one pod per endpoint), a
  Service exposing the Postgres port + `compute_ctl`'s external HTTP API (3080),
  and a ConfigMap providing the compute spec. `values.yaml` covers: image tag,
  endpoint id, `(tenant_id, timeline_id)` binding, role (`read_write` vs
  `read_only`), resource requests, the YBDB connection, the endpoint_storage
  address, and the JWKS/TLS references.
- **ConfigMap-as-spec.** The `--config` value is one `ComputeConfig`
  (`{ spec, compute_ctl_config }`) — the same schema delivered by file / push /
  hosted-pull (§5.5, [`compute_ctl.md`](../architecture/compute_ctl.md)). The
  chart renders it into a ConfigMap mounted at the `--config` path; updating the
  ConfigMap + triggering a `compute_ctl` reconfigure is the k8s-native reconfigure
  path.

### Out of scope

- **VM packaging** (ybops + node-agent) — a separate runtime, designed-for not
  built (§7). The chart/image is the Kubernetes-first artifact.
- **The provisioning interface inside yblight-controller** that calls k8s/Helm —
  that's controller task-framework work; this task delivers the deployable
  artifacts that interface will target.
- **Secret management / cert issuance** internals (§7 cert tracking).

## Plan / sketch

1. Add an endpoint-image target to (or alongside) the top-level multi-stage
   `Dockerfile` that selects only the per-endpoint binaries + patched Postgres +
   `neon` ext + an otel collector. Settle the supervision model.
2. Helm chart under a new `deploy/` (or `charts/`) dir: `Chart.yaml`,
   `values.yaml`, templates for the pod, Service, and ConfigMap. One pod per
   endpoint; the pod is disposable (stateless, §5.3) — replacement is recovery.
3. Wire ConfigMap → `--config`; confirm `compute_ctl` reads the mounted spec file,
   and that a ConfigMap update + `POST /configure` reconfigures cleanly.
4. Smoke test: deploy one endpoint pod against a dev YB + an already-provisioned
   timeline; connect through the Service.

## Notes / links

- Design: [yblight-controller](../yblight_controller/yblight-controller.md) §5.2 /
  §5.4 (per-endpoint services, self-contained blob), §5.5 (spec delivery), and
  [`compute_ctl.md`](../architecture/compute_ctl.md).
- Depends on [task 35](35_yblc-build-bundle.md) for a stable release build of the
  endpoint binaries (the image consumes the same build).
- Consumed by [task 38](38_yblc-create-tenant-flow.md) — creating a r/w endpoint
  deploys through this mechanism — and ultimately by the controller's provisioner
  interface.
- Today `yb_light_local` runs these as local processes under the endpoint dir
  ([task 26](26_ps-sk-under-endpoint-dir.md)); Kubernetes is the production
  runtime per principle 3.
