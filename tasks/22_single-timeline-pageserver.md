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

## Remaining (stage C — the strip)

- HTTP surface prune (~26 tenant/timeline CRUD + location_config +
  archival + import routes).
- page_service + gRPC resolver swap (`SingleTimelineResolver`,
  identity validation; keep `handle.rs` machinery).
- Dissolve `TenantManager` (`tenant/mgr.rs`), then the tenant state
  machine (lazy activation, `TenantState`, timelines HashMap →
  single `Arc<Timeline>`); rename `TenantShard` → `PageServer`.
- TenantConfig collapse (drop `pageserver.tenant_config` read path;
  effective config = toml), dead ConfigToml fields, metrics trim.
- Final: e2e fast + slow.

## Deferred (future tasks)

- Per-endpoint pageserver/safekeeper orchestration (multi-PS ports,
  datadirs, `timeline branch` end-to-end).
- GC ancestor-pin enforcement: ancestor pruning must respect
  `min(child.ancestor_lsn)` — an ancestor row at `lsn ≤ cap` may be a
  branch's only anchor source (noted in `7_gc.md`).
- storage_ctl slimming (delete reconciler/migration machinery
  wholesale).
