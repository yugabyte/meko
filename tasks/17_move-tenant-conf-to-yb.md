---
status: done
severity: medium
area: yb-pageserver
opened: 2026-06-03
closed: 2026-06-03
---

## Resolution

Landed across 7 commits:

- `a2eed007f` — **17.a** Drop 24 dead `TenantConfig` fields
  (`compaction_*`, `l0_flush_*`, `image_*`, `gc_compaction_*`,
  `eviction_*`, `checkpoint_*`, `timeline_offloading`,
  `min_resident_size_override`). Compaction/image-layer/eviction
  knobs were all dead in yb-mode.
- `569319883` — **17.b** Drop `AttachmentMode` + `LocationMode` +
  `LocationConfigMode` enums. `LocationConf` shrinks; live_migrate
  collapses from a 6-step dance to bump-attach-notify-detach.
- `cf28ecb86` — **17.c.1** Add `pageserver.tenant_config` table
  to the existing 001 migration (no separate file).
- `31d6e72b7` — **17.c.2** `PageStore::load_tenant_config` /
  `save_tenant_config` (JSONB-keyed by tenant_id).
- `c48cced3b` — **17.c.3** Storage_controller `Persistence::save_tenant_config`
  (cross-schema `diesel::sql_query`) wired into `do_tenant_create`,
  `set_tenant_config_and_reconcile`, `tenant_location_config`.
- `a8121983e` — **17.c.4** Pageserver reads tenant_config from
  YB on startup (`init_load_tenant_configs`) and on PUT
  `location_config` (`upsert_location`). Falls back to
  `TenantConfig::default()` if the YB row is absent.
- `235999eee` — **17.c.5** Drop `tenant_conf` from the
  `LocationConfig` wire and from internal `LocationConf`.
  `AttachedTenantConf::try_from` takes an explicit `tenant_conf`
  arg sourced from YB.

End state: `pageserver.tenant_config` is the sole source of truth
for `TenantConfig`. storage_controller writes it on tenant create /
update; pageservers re-read on every PUT location_config and at
startup. The wire and local `config-v1` no longer carry it.

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
