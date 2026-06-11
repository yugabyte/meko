---
status: pending
severity: high
area: yblc
opened: 2026-06-11
---

# yblight-controller init process

## Goal

Implement yblc's first-boot / init: stand up the placement group's control plane.
Per §9.3 (bootstrapping) and §10 (state tracking), once the YBDB universe is
reachable, init must connect to YBDB over YSQL, create yblc's control-plane
database + schema and run its migrations, bring up its managed global services
(`endpoint_storage` and the storage controller, §5.1 / §11), and leave the
placement group ready for tenant pools.

## Scope

### In scope

- **YBDB connectivity.** Take the required YSQL connection string (§9 — always
  provided as deployment config) and the control-plane signing key (§9.4 — a file
  path, or self-generate-and-store in dev). Refuse to start without a connection
  string.
- **Schema + migrations.** Create yblc's own control-plane DB/schema, isolated from
  tenant data by RBAC (§10), and run its migrations. The managed storage controller
  brings its own schema (the collapsed Diesel migration — see
  [`storage-controller.md`](../architecture/storage-controller.md)); init
  orchestrates provisioning so the boot races don't recur.
- **Bring up global services.** Start and health-check `endpoint_storage` + the
  storage controller as managed services under yblc (deploy / supervise, §11) — one
  of each per placement group, all persisting to the same universe over YSQL.
- **Idempotent & re-entrant** (principle 5 / §8): re-running init converges; matches
  the "first boot creates schema" posture (§9.3, §10).
- **Direct mode and YBA mode** (§9.1 / §9.2): init must work with no YBA (direct
  mode, the dev/test default); YBA access is strictly additive.

### Out of scope

- **First admin user / empty tenant pool** — [task 37](37_yblc-first-time-login.md).
- **The full task framework internals** (§8) — init can be a bootstrap path that is
  later expressed as tasks.
- **YBA universe creation** — operator/runbook-driven (§9.3); init assumes the
  universe exists and is reachable.

## Plan / sketch

1. Parse + validate the connection config; reuse the TLS-aware admin-connect pattern
   from `control_plane/src/storage_controller.rs` (`connect_external`,
   `ensure_external_database`).
2. Provision yblc's DB + schema and run migrations. Decide: share a DB with
   `storage_controller` / `yb_storage` or keep yblc's schema in a separate DB for
   clean lifecycle (cf. the cross-DB question raised in
   [task 14](14_data-modeling-review.md), and the §10 isolation requirement).
3. Provision universe-side prerequisites **synchronously** before launching the
   managed services — structurally fixing the SC-before-SK ordering race
   (AGENTS.md sharp edge; cf. [task 4.k](4.k_yb-light-local-start-wedge.md)): a
   synchronous pre-step rather than an unordered `JoinSet`.
4. Start + health-check `endpoint_storage` and the storage controller.
5. Load or generate the signing key (§9.4); record only public halves + lifecycle
   metadata in the state store — never the private key.

## Notes / links

- Design: [yblight-controller](../yblight_controller/yblight-controller.md) §9
  (deployment config), §9.3 (bootstrapping), §9.4 (signing key), §10 (state
  tracking), §5.1 / §11 (managed services).
- Bootstrapping is runbook-driven (§9.3) — this task delivers the yblc side; the
  runbook is a separate deliverable.
- Reuse: [`storage-controller.md`](../architecture/storage-controller.md)
  auto-provision flow + `control_plane/src/storage_controller.rs`. Cross-link
  [task 4.k](4.k_yb-light-local-start-wedge.md) (SC startup wedge) and the AGENTS.md
  SC/SK ordering sharp edge.
- Depends on [task 35](35_yblc-build-bundle.md) (the binaries to launch). Precedes
  [task 37](37_yblc-first-time-login.md) and [task 38](38_yblc-create-tenant-flow.md).
