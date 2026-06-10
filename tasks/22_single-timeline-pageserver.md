---
status: in_progress
severity: high
area: yb-pageserver
opened: 2026-06-09
---

# Single-(tenant,timeline) pageserver

Each compute endpoint will get its own pageserver + safekeeper. This
task refactors the pageserver so one process serves exactly one
`(tenant_id, timeline_id)` for its entire lifetime, and strips the
multi-tenant/multi-timeline machinery. Design + stage breakdown in
`architecture/yb-pageserver.md` ("Single-timeline process model" and
"Lineage-aware reads" sections).

## Landed (stages A + B — e2e fast green)

- **Identity from startup config**: `tenant_id` / `timeline_id` /
  `generation` + `[timeline_init]` (bootstrap | branch) in
  `pageserver.toml` (`pageserver_api::config::ConfigToml`,
  `PageServerConf::timeline_identity`). All-or-nothing validation.
- **Open-or-create at startup**: `preload` loads only the configured
  timeline's row; `attach` runs create-if-missing — bootstrap
  (initdb + import, reusing a previously-uploaded initdb tar for
  crash-retry idempotency) or branch (`PageStore::create_branch_row`,
  row-only validation, `consistent_lsn = ancestor_lsn`). Refuses rows
  with `deleted_at`. Re-attach upcall deleted
  (`controller_upcall_client.rs` removed).
- **Lineage-aware reads**: `PageStore::load_lineage` +
  `*_lineage` variants of all six read paths (pages/SLRU/meta,
  point + range); `PageWriter` threads the lineage everywhere incl.
  `read_chain_durable`; segment-crossing reads force a read-side
  materialise dispatch (copy-up convergence). Branch reads work
  without hosting the ancestor. Unit-tested against YB.
- **Launcher-owned generation**: `PageServerNode::start` =
  node_register + attach_hook (bumps `tenants.generation`, returns
  it) + rewrite toml + spawn. Every spawn is a fenced attach.
- **storage_ctl**: reconciler no-op'd in yb-mode;
  `tenant_timeline_create_pageservers` poll-observes
  `timeline_detail` until Active instead of POSTing timeline_create;
  safekeeper leg unchanged.
- **neon_local**: `tenant create` bakes identity → starts PS → PS
  bootstraps → storage_ctl observes + SK leg; start-all skips
  identity-less pageservers; `timeline create`/`branch` error until
  per-endpoint orchestration lands.

## Landed (stage C — the strip, e2e fast+slow green)

- HTTP surface pruned to the single-timeline set (~26 routes deleted:
  tenant/timeline CRUD, location_config, archival, detach_ancestor,
  block/unblock_gc, wait_lsn, keyspace, top_tenants, import, aux
  files, page_trace, …) plus their handler/error plumbing and the
  import_basebackup/import_wal ingestion paths.
- page_service + gRPC resolve against the configured identity
  (misrouted requests rejected); shard-split rerouting
  (`maybe_split_get_page`) deleted; `handle.rs` machinery kept.
- `tenant/mgr.rs` rewritten as a single-slot holder (1878 → ~490
  lines): TenantsMap/TenantSlot/slot guards, upsert_location,
  detach/delete, resolve_attached_shard, slot metrics all gone.
- Lazy-activation machinery deleted (`SpawnMode::Lazy`, warmup
  semaphore wait, `AttachType`); attach is always eager.
- Dead multi-timeline code deleted from `tenant.rs`/`timeline.rs`:
  create_timeline/branch_timeline (in-process), DeleteTimelineFlow
  remnants, archival checks, set_broken, gc_block summary/insert/
  remove, collect_keyspace, ancestor_timeline accessor, etc.

## Landed (per-endpoint topology — `timeline branch` end-to-end)

- `timeline branch` provisions a dedicated pageserver + safekeeper
  pair per branch: next node ids, ports offset from the lowest-id
  node's, ancestor's `pageserver.toml` cloned + patched
  (`PageServerNode::initialize_from_template`), branch identity +
  `[timeline_init] mode="branch"` baked, SK timeline created at the
  branch LSN (single-member mconf), both processes started, Active
  polled.
- `LocalEnv` carries per-node timeline identities
  (`PageServerConf.{tenant_id,timeline_id}` read from
  `pageserver.toml`, `SafekeeperConf.timeline_id` persisted in
  `.yb_light/config`) with `pageserver_conf_for_timeline` /
  `safekeepers_for_timeline` lookups.
- Endpoints resolve PS conn-info and `neon.safekeepers` from their
  timeline's dedicated pair; storage_ctl `tenant_locate` is unused.
- e2e: `test/e2e/fast/branch-endpoint.sh` — branch endpoint reads
  pre-branch data via lineage, writes isolated both directions,
  survives a branch-pageserver restart. Uses `VACUUM FREEZE` on the
  seed to sidestep the pre-existing task-24 CLOG hole.
- Branch-point fix found by the e2e test: the default branch LSN is
  the ancestor safekeeper's `flush_lsn` (durable WAL end), not the
  ancestor pageserver's `last_record_lsn` — the PS ingest position
  lags the compute's flushed WAL, so branching there right after a
  commit cut the seed transaction out of the branch (the branch saw a
  consistent-but-mid-transaction snapshot: table present, 0 rows).
- Lineage correctness fix found by the e2e test: the chain walk must
  only stop at an own-segment anchor that sits AT OR ABOVE the next
  segment's cap — a materialiser copy-up image below the cap must
  still have ancestor deltas in `(anchor_lsn, cap]` layered on top
  (`read_chain_lineage`, `read_slru_chain_lineage`, and the
  PageWriter cache fast-path gate `cache_anchor_covers_lineage`).
  Regression test:
  `read_chain_lineage_layers_ancestor_deltas_above_sub_cap_anchor`.
  NB: a branch whose pageserver served stale reads before this fix
  may have flushed FPIs of stale pages into its own timeline
  (post-cap own images) — such branches are poisoned and must be
  recreated; only branches created with the fix are sound.

## Remaining (cosmetic, future)

- timelines `HashMap` → single `Arc<Timeline>` field +
  `TenantShard` → `PageServer` rename + synchronous open-before-
  listeners startup (drops `TenantState` machine and the in-protocol
  active-waits).
- TenantConfig collapse (drop `pageserver.tenant_config` read path;
  effective config = toml), dead ConfigToml fields
  (`concurrent_tenant_warmup`, …).

## Deferred (future tasks)

- GC ancestor-pin enforcement: ancestor pruning must respect
  `min(child.ancestor_lsn)` — an ancestor row at `lsn ≤ cap` may be a
  branch's only anchor source (noted in `7_gc.md`).
- storage_ctl slimming (delete reconciler/migration machinery
  wholesale).
