---
status: pending
severity: high
area: yblc
opened: 2026-06-11
---

# Create-tenant flow: tenant → first PG cluster/timeline → r/w endpoint

## Goal

Implement the first end-to-end user workflow (§6, workflows 1 + 2 + 4): create a
**tenant** in a tenant pool, create its first **PG cluster (fresh timeline)**, and
bring up a **read-write endpoint** serving it. This is the headline path that
exercises tenancy (§4), timeline scoping (§4.3), the single-writer invariant (§4.4),
and endpoint provisioning (§5) together — the integration milestone for the yblc
area.

## Scope

### In scope

- **Create tenant** in a pool (§6.1): a tenant owned by the pool (§4.1 / §10.1),
  name unique within the pool.
- **Create first PG cluster (fresh timeline)** (§6.2): allocate a new timeline keyed
  by `(tenant_id, timeline_id)` (§4.3), create the PG cluster bound to it, and drive
  the storage layer (the storage-controller registry + generation authority,
  §11.1 / §11.2) to materialize the timeline — reusing the pending-init recipe path
  ([task 29](29_pending-init-to-yb.md)) the storage controller already implements.
- **Create a read-write endpoint** (§6.4): exactly one write endpoint, enforced at
  the API layer (§4.4) ahead of the partial unique index (§10.2). Build the endpoint
  **spec** — the durable artifact (§5.3): YBDB connection, `(tenant_id, timeline_id)`
  binding, `read_write` role, endpoint_storage address + scoped token, otel config,
  TLS/JWKS — and deploy it via [task 34](34_k8s-endpoint-deploy.md)'s k8s mechanism
  (ConfigMap → `--config`, §5.5), then reconcile against `compute_ctl`'s `/status`
  until `running` (see [`compute_ctl.md`](../architecture/compute_ctl.md)).
- **Tasks** (§8): each step is an idempotent subtask; "create endpoint" is the
  reusable subtask later shared by the fork and scale-up flows.

### Out of scope

- **Forking/branching a timeline** (§6.3) and **scale-up/down** beyond the first r/w
  endpoint (§6.4) — follow-ups that reuse the same subtasks.
- **Read-only endpoints**, **upgrades / promotion** (§7).
- **Timeline data GC / teardown** (§12).

## Plan / sketch

1. API + task: create tenant (validate the pool, enforce unique name).
2. Create timeline + PG cluster: allocate ids, record the init recipe, drive the
   storage controller to materialize the PS + SK pair (reuse
   [task 29](29_pending-init-to-yb.md)'s pending-init +
   [`storage-controller.md`](../architecture/storage-controller.md)).
3. Build the `ComputeConfig` spec (§5.3 / §5.5) and provision the endpoint via
   [task 34](34_k8s-endpoint-deploy.md) (pod + ConfigMap); reconcile to `running`.
4. Enforce single-writer at the API layer (§4.4) in addition to the DB constraint
   (§10.2).
5. Return connection details (host / port / TLS) to the caller (§4.2).

## Notes / links

- Design: [yblight-controller](../yblight_controller/yblight-controller.md) §6 (core
  workflows), §4 (resource model), §4.3 (timelines), §4.4 (single writer), §5.3
  (spec as durable artifact), §5.5 (spec delivery), §11.1 / §11.2 (storage-controller
  reuse).
- compute-side bring-up mechanics: [`compute_ctl.md`](../architecture/compute_ctl.md)
  (spec → safekeeper sync → basebackup → start → apply → `running`).
- Depends on [task 36](36_yblc-init.md) (init/schema), [task 37](37_yblc-first-time-login.md)
  (a pool + admin to own the tenant), [task 34](34_k8s-endpoint-deploy.md) (endpoint
  deploy), and reuses [task 29](29_pending-init-to-yb.md) (pending-init recipe) + the
  storage controller.
- Keep at high severity until a tenant + cluster + r/w endpoint comes up end-to-end.
