---
status: in_progress
severity: medium
area: yb-pageserver
opened: 2026-06-03
---

## Progress

- **17.a — Dead `TenantConfig` field deletion**: ✅ done (commit
  `a2eed007f`). 24 fields removed across the `TenantConfig` /
  `TenantConfigToml` / `TenantConfigPatch` structs + getters +
  storage_controller mirrors + control_plane parsing + tests.
- **17.b — Drop `AttachmentMode` + `LocationMode`**: ✅ done (commit
  `569319883`). `LocationConfigMode` enum + `LocationConfigSecondary`
  struct gone; `LocationConf` collapses to
  `{ generation, shard, tenant_conf }`; storage_controller
  migration dance + secondary-location plumbing unwound.
- **17.c — `pageserver.tenant_config` YB table**: ⏳ schema landed
  (this commit). Remaining: storage_controller-side YB writes,
  pageserver-side YB reads on attach + startup, drop `tenant_conf`
  from `LocationConfig` wire + local `config-v1`.

## Follow-up for 17.c

1. Storage_controller `Persistence::write_tenant_config(tenant_id,
   TenantConfig)` using `diesel::sql_query` (cross-schema). Call it
   from `tenant_create` and `update_tenant_config` /
   `patch_tenant_config` paths.
2. Pageserver `PageStore::load_tenant_config(tenant_id) ->
   Option<TenantConfig>`. Use the existing `fpu_page_store` pool
   (one SELECT per attach, negligible load).
3. `Tenant::persist_tenant_config_at` stops writing `tenant_conf`
   to the local `config-v1` (only generation + shard remain).
4. PUT `location_config` handler ignores `request.config.tenant_conf`
   and re-reads from YB.
5. Drop `tenant_conf` field from `LocationConfig` wire and from
   internal `LocationConf`.
6. Tenant startup (`init_load_tenant_configs` in `tenant/mgr.rs`)
   reads tenant_conf from YB after loading the on-disk `config-v1`.

# Move per-tenant `TenantConfig` into YB, shrink `LocationConf`

## Motivation

Each attached tenant has a local `<workdir>/tenants/<id>/config-v1`
toml that holds a `LocationConf`:

- `generation: Generation` — fencing primitive, set by
  storage_controller, must be readable before any YB connection
  opens. Stays on disk.
- `attach_mode: AttachmentMode` — `Single` / `Multi` / `Stale`. Only
  gates two upstream behaviours
  (`may_delete_layers_hint`, `may_upload_layers_hint`) that protect
  S3 layer files during migration windows. In yb-mode there are no
  layer files; the YB write fence on the generation column handles
  cross-pageserver races directly. Dead in this fork.
- `shard: ShardIdentity` — see [task 18](18_drop-sharding.md).
- `tenant_conf: TenantConfig` — pan-cluster per-tenant tunables (gc
  period, compaction settings, PITR, walreceiver knobs, …). Same on
  every pageserver that attaches the tenant.

`tenant_conf` is logically a property of the tenant, not of the
attachment. It should live in YB so storage_controller can update it
in one place and every pageserver picks it up on attach, instead of
each pageserver carrying its own toml copy. This also kills a class
of "config drift between PSes" bugs.

## Scope

1. New table `pageserver.tenant_config` keyed by `tenant_id`, value
   is the `TenantConfig` payload (likely JSONB to absorb future
   field additions without migrations).
2. storage_controller's tenant-create / tenant-config-update writes
   to that table; the location_config HTTP endpoint stops carrying
   `tenant_conf`.
3. Pageserver on attach reads `tenant_config` from YB instead of
   from the local toml. Cache in memory; invalidate on
   storage_controller-triggered reload.
4. Audit `TenantConfig`'s 30+ fields and drop the ones that only
   make sense in upstream's layer-stack world
   (`compaction_target_size`, `compaction_period`,
   `compaction_threshold`, `compaction_algorithm`,
   `l0_flush_delay_threshold`, `l0_flush_stall_threshold`,
   `gc_horizon`, `gc_period`, `image_layer_creation_*`, etc.).
   Keep PITR, walreceiver, eviction-policy if those are still
   meaningful.
5. Drop `AttachmentMode` and `LocationMode` entirely. The two
   `may_*_hint` callsites are dead in yb-mode. `LocationConf`
   collapses to `{ generation }`.
6. Once `LocationConf = { generation }`, decide whether the per-
   tenant toml file should stay or be renamed `generation` (or
   eliminated by stashing generation in a single
   `<workdir>/tenants/generations.json`-style file). Probably out of
   scope for this task; flag as follow-up if it falls out cleanly.

## Notes

Touches: `pageserver/src/tenant/config.rs`, `pageserver/src/tenant.rs`
(persist/load helpers around line 3200), `pageserver/src/tenant/mgr.rs`
(startup scan), the storage_controller tenant config + diesel
migration, `libs/pageserver_api/src/models.rs` (`TenantConfig`).

Cross-link with [task 18](18_drop-sharding.md) — both shrink
`LocationConf`. Order doesn't matter; the diffs touch disjoint fields.
