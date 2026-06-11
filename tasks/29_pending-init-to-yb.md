---
status: pending
severity: medium
area: storage_ctl / yb_light_local
opened: 2026-06-10
---

# Move the pending-timeline init recipe out of local config into YB

## Goal

Durable cluster state must live in YugabyteDB, not on local disk — so
that the local launcher and a future k8/VM multi-node operator run the
*same* control-plane code and data paths, differing only in how a node
is materialized when an endpoint starts (a local process vs a k8 pod).

[Task 26](26_ps-sk-under-endpoint-dir.md) co-located the PS+SK under the
endpoint folder and deferred their provisioning to `endpoint create`. To
do that it parked the timeline's *init recipe* in `.yb_light/config` as
`pending_timeline_inits` (keyed by `timeline_id`):

```
PendingTimelineInit { tenant_id, branch_name,
    mode: Bootstrap { pg_version }
        | Branch { ancestor_timeline_id, start_lsn, pg_version } }
```

That recipe is genuinely cluster state (it says "this timeline exists and
here's how to bring it up") and should not be on local disk. This task
moves it into the storage controller's YB-backed schema.

## What is / isn't in scope

- **In scope — move to YB:** the pending init recipe above. After this,
  `tenant create` / `timeline branch` record the timeline as a *pending*
  row in `storage_controller.timelines`; `endpoint create` reads it back
  to materialize the PS+SK; the `pending_timeline_inits` section is
  deleted from `OnDiskConfig` / `LocalEnv`.
- **Out of scope — stays local (decided):** `pageserver_templates` /
  `safekeeper_templates` (port/auth/`remote_storage` policy). These are
  launcher/orchestrator config — the local equivalent of a k8 Deployment
  spec — not cluster state. Per-node ephemera (`pageserver.toml`,
  `identity.toml`, `pgdata`, logs, pidfiles) also stay local/pod-local;
  they're derived, not durable.

## Plan / sketch

1. **Schema (`storage_controller`, Diesel migration).** The `timelines`
   table already has `start_lsn`, `sk_set`, `branch_name`. It's missing
   the rest of the recipe. Add (either as columns or one `init jsonb`):
   - `pg_version int` (or in jsonb),
   - `ancestor_timeline_id varchar NULL`,
   - a "pending" marker — i.e. a row that exists before any pageserver is
     attached. Today a `timelines` row is created by
     `tenant_timeline_create` (bootstrap) / `timeline_register` (branch),
     both of which run *after* the PS is up. We need an earlier "intent"
     state. Options: a `materialized boolean` / `state` enum on
     `timelines`, or a separate `pending_timeline_inits` table keyed by
     `(tenant_id, timeline_id)` that the materialize step deletes.
   Keep the migration collapsed into the single bootstrap migration per
   [task 10](10_collapse-storage_ctl-diesel-migrations.md) conventions.

2. **Storage controller HTTP API.** Add:
   - record-pending: `POST` a pending timeline (tenant, timeline, branch
     name, mode, pg_version, ancestor, start_lsn).
   - read-pending: `GET` pending timeline(s) for a tenant, and resolve a
     branch name → timeline id even when not yet materialized (today
     `list_timeline_branch_names` only returns materialized timelines,
     which is why task 26 had to resolve unprovisioned branches from the
     local pending set).
   - The branch-name mapping registration (`set_timeline_branch_name`)
     currently rejects a timeline the controller doesn't know yet — the
     pending row fixes that, so the deferred-mapping dance in task 26
     (`provision_timeline_for_endpoint` registering the mapping after
     provisioning) can be simplified.

3. **`control_plane` rewiring (`yb_light_local.rs` / `local_env.rs`).**
   - `tenant create`: instead of `env.pending_timeline_inits.insert(...)`,
     call the SC record-pending API (Bootstrap).
   - `timeline branch`: same, Branch mode (still captures `start_lsn` +
     `pg_version` from the running ancestor as today).
   - `endpoint create` / `provision_timeline_for_endpoint`: resolve the
     branch and read the recipe from the SC instead of
     `env.pending_timeline_inits`; clear the pending state via the SC
     (mark materialized / delete the pending row) instead of mutating
     config.
   - Delete `PendingTimelineInit` / `PendingTimelineInitMode` and the
     `pending_timeline_inits` field from `LocalEnv` / `OnDiskConfig`;
     drop the round-trip unit test, add SC-side coverage.

## Notes / links

- Related north-star follow-up (not in this task, deferred): also stop
  persisting *provisioned* `safekeepers` in `.yb_light/config` and derive
  them from `storage_controller.safekeepers` + `timelines.sk_set` (the SK
  registry is already in YB — only the per-SK `wal_yb_config` / port
  policy would need a home). That would remove another chunk of local
  cluster state. Left out here to keep the change focused.
- `tools/cluster-ui` already reads topology straight from the
  `storage_controller` schema in `yb_storage`, so it needs no change for
  this — it'll pick up pending timelines once they land in `timelines`.
- Verification mirrors task 26: `cargo test -p control_plane` +
  storage_controller tests, then `test/e2e/run.sh all` (the layout +
  branch-endpoint tests already exercise the create/branch/endpoint
  lifecycle this rewires).
