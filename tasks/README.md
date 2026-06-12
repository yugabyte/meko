# tasks/

Lightweight tracker for cross-session work. One Markdown file per
issue or task. Treat each file as a Jira ticket: pick a stable
filename slug, write the body free-form, mark done by flipping the
frontmatter `status` (or move to `tasks/archive/` once truly cold).

## Conventions

- **Filename:** `<id>_<short-kebab-case-slug>.md`. The `<id>` is one
  of:
  - `<num>` for top-level tasks with no siblings (`6`, `10`).
  - `<num>.<letter>` when a parent number has multiple subtasks
    (`4.a`, `4.k`, `2.b`).
  - `<num>.<letter>.<num>` for further nesting under a subtask
    (`3.a.1`, `3.c.1` — sub-subtasks of `3.a` / `3.c`).
  Drop the trailing component whenever there's only one child;
  don't add letters or numbers for siblings that don't exist.
  Don't reuse IDs even when the underlying file is deleted, so
  references stay stable.
- **Frontmatter** at the top of each file (keep it brief):
  ```
  ---
  status: pending    # pending | in_progress | blocked | done | deferred
  severity: medium   # highest | high | medium | low
  area: yb-pageserver
  opened: 2026-06-02
  ---
  ```
  Index renders status as: ✅ done · 🚧 in_progress · ⏳ pending ·
  🚫 blocked · 💤 deferred.
- Sections to include when relevant: **Symptom**, **Root cause /
  hypothesis**, **Repro**, **Fix sketch / Plan**, **Notes / links**
  (commits, log lines, PR refs).
- Cross-link to other files with
  `[see 4.k](4.k_yb-light-local-start-wedge.md)`.
- One issue per file; correlations go in **Notes**.

## Areas

Use one of these in the `area:` frontmatter field:

- `yb-pageserver` — pageserver binary, page store, materialiser,
  write/read paths.
- `safekeeper` — safekeeper binary, WAL durability, walproposer
  protocol.
- `storage_ctl` — storage_controller binary, schema bootstrap.
- `page_service / compute` — pageserver ↔ compute wire protocol,
  libpagestore, communicator.
- `yb_light_local` — local-dev orchestrator; CLI + child-process
  management.

Multi-area tasks: `area: storage_ctl / yb_light_local` is fine.

## Index

| ID | Title | Status | Severity | Area |
|---|---|---|---|---|
| 0 | [Fork pageserver binary](0_fork-pageserver-binary.md) | ✅ done | medium | yb-pageserver |
| 1 | [`pageserver.pages` schema + PageStore](1_pages-schema-and-pagestore.md) | ✅ done | medium | yb-pageserver |
| 2.a | [`ImageRule` 10-delta enforcement](2.a_image-rule.md) | ✅ done | medium | yb-pageserver |
| 2.b | [`PageWriter` buffered write path](2.b_pagewriter-buffered-write-path.md) | ✅ done | medium | yb-pageserver |
| 3.a | [`Materializer` trait refactor](3.a_materializer-trait-refactor.md) | ✅ done | medium | yb-pageserver |
| 3.a.1 | [Primitives feeding Phase 3b](3.a.1_primitives.md) | ✅ done | medium | yb-pageserver |
| 3.b | [Tenant / Timeline plumbing for dual-write](3.b_tenant-timeline-plumbing.md) | ✅ done | medium | yb-pageserver |
| 3.c | [Read flip to PageStore](3.c_read-flip-to-pagestore.md) | ✅ done | medium | yb-pageserver |
| 3.c.1 | [`yb_page_writer` config block](3.c.1_config-block.md) | ✅ done | medium | yb-pageserver |
| 3.d | [Schema reshape + design pivot](3.d_schema-reshape-design-pivot.md) | ✅ done | medium | yb-pageserver |
| 3.e | [Background image materialiser](3.e_background-image-materialiser.md) | ✅ done | medium | yb-pageserver |
| 3.f | [`UnifiedPages`](3.f_unified-pages.md) | ✅ done | medium | yb-pageserver |
| 4.a | [Stop dual-writing rel pages](4.a_stop-dual-writing-rel-pages.md) | ✅ done | medium | yb-pageserver |
| 4.b | [SLRU pages table](4.b_slru-pages-table.md) | ✅ done | medium | yb-pageserver |
| 4.c | [Meta blobs table](4.c_meta-blobs-table.md) | ✅ done | medium | yb-pageserver |
| 4.d | [Layer-stack code deletion](4.d_layer-stack-code-deletion.md) | ✅ done | medium | yb-pageserver |
| 4.e | [Write-path hardening + perf](4.e_write-path-hardening-perf.md) | ✅ done | medium | yb-pageserver |
| 4.f | [Per-key LRU eviction](4.f_per-key-lru-eviction.md) | ✅ done | high | yb-pageserver |
| 4.g | [Cache-only fast path](4.g_cache-only-fast-path.md) | ✅ done | medium | yb-pageserver |
| 4.h | [Dedicated YSQL connection](4.h_dedicated-ysql-connection.md) | ✅ done | medium | yb-pageserver |
| 4.i | [Hybrid materialiser threshold](4.i_hybrid-materialiser-threshold.md) | ✅ done | medium | yb-pageserver |
| 4.j | [Microbench threshold tuning](4.j_microbench-threshold-tuning.md) | ⏳ pending | medium | yb-pageserver |
| 4.k | [`yb_light_local start` wedge](4.k_yb-light-local-start-wedge.md) | ✅ done | high | storage_ctl / yb_light_local |
| 4.l | [`s3_upload_limit_bytes` config](4.l_s3-upload-limit-config.md) | ✅ done | medium | yb-pageserver |
| 4.m | [YB write concurrency + read collapse](4.m_yb-write-concurrency-read-collapse.md) | ✅ done | medium | yb-pageserver |
| 5 | [Pageserver restart from timeline_state](5_pageserver-restart-from-timeline-state.md) | ✅ done | medium | yb-pageserver |
| 6 | [Startup performance](6_startup-perf.md) | ✅ done | low | yb-pageserver |
| 7 | [Garbage collection](7_gc.md) | 💤 deferred | medium | yb-pageserver |
| 8 | [Page-service going-away frame](8_going-away-frame.md) | ✅ done | high | page_service / compute |
| 9 | [Safekeeper Paxos removal](9_safekeeper-paxos-removal.md) | ✅ done | high | safekeeper |
| 10 | [Collapse storage_ctl Diesel migrations](10_collapse-storage_ctl-diesel-migrations.md) | ✅ done | medium | storage_ctl |
| 11 | [Reduce YB reads when cold pages are modified](11_reduce-yb-reads-cold-page-modify.md) | ⏳ pending | medium | yb-pageserver |
| 12 | [End-to-end test infra](12_e2e-test-infra.md) | ✅ done | high | yb-pageserver |
| 14 | [Data-modeling review](14_data-modeling-review.md) | ⏳ pending | high | yb-pageserver |
| 15 | [`pageserver.meta` read hotness](15_pageserver-meta-read-hotness.md) | ⏳ pending | high | yb-pageserver |
| 16 | [Unwind dead RTC plumbing](16_rtc-dead-code-unwind.md) | ✅ done | medium | yb-pageserver |
| 17 | [Move tenant_conf to YB + shrink LocationConf](17_move-tenant-conf-to-yb.md) | ✅ done | medium | yb-pageserver |
| 18 | [Drop tenant-sharding machinery](18_drop-sharding.md) | ✅ done | medium | yb-pageserver |
| 19 | [SK control file → YB columns](19_sk-control-file-to-yb-columns.md) | ✅ done | medium | safekeeper |
| 20 | [Remove storage broker](20_remove-storage-broker.md) | ✅ done | medium | safekeeper |
| 21 | [Drop `commit_lsn` / `peer_horizon_lsn` from wire](21_drop-commit-lsn-peer-horizon-from-wire.md) | ✅ done | low | safekeeper |
| 22 | [Single-(tenant,timeline) pageserver](22_single-timeline-pageserver.md) | ✅ done | high | yb-pageserver |
| 23 | [Flaky: SK kill-9 recovery hits "YB WAL hole"](23_sk-kill9-wal-hole-flake.md) | ✅ done | high | safekeeper |
| 24 | [Early-window CLOG SLRU deltas never persist](24_early-clog-slru-deltas-lost.md) | ✅ done | highest | yb-pageserver |
| 25 | [Validate the output of `.sql` e2e tests](25_e2e-sql-output-validation.md) | ✅ done | medium | yb-pageserver |
| 26 | [PS + SK under the endpoint folder](26_ps-sk-under-endpoint-dir.md) | ✅ done | medium | yb_light_local |
| 27 | [PS cache-walk regression (`scan_range` `cache_ms`)](27_ps-cache-walk-regression.md) | ⏳ pending | high | yb-pageserver |
| 28 | ["Catch me if you can" walredo corruption](28_walredo-corruption-catch-me-if-you-can.md) | 🚧 in_progress | highest | yb-pageserver |
| 29 | [Move pending-timeline init recipe into YB](29_pending-init-to-yb.md) | ✅ done | medium | storage_ctl / yb_light_local |
| 30 | [Convert e2e suite from shell to Rust](30_e2e-tests-sh-to-rust.md) | ✅ done | medium | yb-pageserver |
| 31 | [Bundle runtime libs for the walredo child](31_bundle-libs-for-walredo-child.md) | ⏳ pending | high | yb_light_local |
| 32 | [`yb-dump-pages` LSN sort uses rendered text](32_yb-dump-pages-lsn-sort.md) | ⏳ pending | medium | yb-pageserver |
| 33 | [Skip or optimize zero-page writes to YB](33_skip-or-compress-zero-page-writes.md) | ⏳ pending | medium | yb-pageserver |
