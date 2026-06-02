# tasks/

Lightweight tracker for cross-session work. One Markdown file per
issue or task. Treat each file as a Jira ticket: pick a stable
filename slug, write the body free-form, mark done by flipping the
frontmatter `status` (or move to `tasks/archive/` once truly cold).

## Conventions

- **Filename:** `<id>_<short-kebab-case-slug>.md`. The `<id>` is
  either a bare integer (`6`, `10`) or `<num>.<letter>` for grouped
  work (`4.a`, `4.k`, `2.b`). Use `<num>.<letter>` only when a
  parent number has multiple subtasks; otherwise drop the letter.
  Don't reuse IDs even when the underlying file is deleted, so
  references stay stable.
- **Frontmatter** at the top of each file (keep it brief):
  ```
  ---
  status: open       # open | in_progress | pending | blocked | done | deferred
  severity: high     # high | medium | low
  area: yb-pageserver
  opened: 2026-06-02
  ---
  ```
- Sections to include when relevant: **Symptom**, **Root cause /
  hypothesis**, **Repro**, **Fix sketch / Plan**, **Notes / links**
  (commits, log lines, PR refs).
- Cross-link to other files with
  `[see 4.k](4.k_neon-local-start-wedge.md)`.
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
- `neon_local` — local-dev orchestrator; CLI + child-process
  management.

Multi-area tasks: `area: storage_ctl / neon_local` is fine.

## Index

| ID | Title | Status | Severity | Area |
|---|---|---|---|---|
| 0 | [Fork pageserver binary](0_fork-pageserver-binary.md) | done | low | yb-pageserver |
| 1 | [`pageserver.pages` schema + PageStore](1_pages-schema-and-pagestore.md) | done | low | yb-pageserver |
| 2.a | [`ImageRule` 10-delta enforcement](2.a_image-rule.md) | done | low | yb-pageserver |
| 2.b | [`PageWriter` buffered write path](2.b_pagewriter-buffered-write-path.md) | done | low | yb-pageserver |
| 3.a | [`Materializer` trait refactor](3.a_materializer-trait-refactor.md) | done | low | yb-pageserver |
| 3.a+ | [Primitives feeding Phase 3b](3.a+_primitives.md) | done | low | yb-pageserver |
| 3.b | [Tenant / Timeline plumbing for dual-write](3.b_tenant-timeline-plumbing.md) | done | low | yb-pageserver |
| 3.c | [Read flip to PageStore](3.c_read-flip-to-pagestore.md) | done | low | yb-pageserver |
| 3.c+ | [`yb_page_writer` config block](3.c+_config-block.md) | done | low | yb-pageserver |
| 3.d | [Schema reshape + design pivot](3.d_schema-reshape-design-pivot.md) | done | low | yb-pageserver |
| 3.e | [Background image materialiser](3.e_background-image-materialiser.md) | done | low | yb-pageserver |
| 3.f | [`UnifiedPages`](3.f_unified-pages.md) | done | low | yb-pageserver |
| 4.a | [Stop dual-writing rel pages](4.a_stop-dual-writing-rel-pages.md) | done | low | yb-pageserver |
| 4.b | [SLRU pages table](4.b_slru-pages-table.md) | done | low | yb-pageserver |
| 4.c | [Meta blobs table](4.c_meta-blobs-table.md) | done | low | yb-pageserver |
| 4.d | [Layer-stack code deletion](4.d_layer-stack-code-deletion.md) | in_progress | low | yb-pageserver |
| 4.e | [Write-path hardening + perf](4.e_write-path-hardening-perf.md) | done | low | yb-pageserver |
| 4.f | [Per-key LRU eviction](4.f_per-key-lru-eviction.md) | pending | medium | yb-pageserver |
| 4.g | [Cache-only fast path](4.g_cache-only-fast-path.md) | done | low | yb-pageserver |
| 4.h | [Dedicated YSQL connection](4.h_dedicated-ysql-connection.md) | done | low | yb-pageserver |
| 4.i | [Hybrid materialiser threshold](4.i_hybrid-materialiser-threshold.md) | done | low | yb-pageserver |
| 4.j | [Microbench threshold tuning](4.j_microbench-threshold-tuning.md) | pending | low | yb-pageserver |
| 4.k | [`neon_local start` wedge](4.k_neon-local-start-wedge.md) | open | medium | storage_ctl / neon_local |
| 4.l | [`s3_upload_limit_bytes` config](4.l_s3-upload-limit-config.md) | done | low | yb-pageserver |
| 4.m | [YB write concurrency + read collapse](4.m_yb-write-concurrency-read-collapse.md) | done | low | yb-pageserver |
| 5 | [Pageserver restart from timeline_state](5_pageserver-restart-from-timeline-state.md) | done | low | yb-pageserver |
| 6 | [Startup performance](6_startup-perf.md) | pending | medium | yb-pageserver |
| 7 | [Garbage collection](7_gc.md) | deferred | low | yb-pageserver |
| 8 | [Page-service going-away frame](8_going-away-frame.md) | pending | medium | page_service / compute |
| 9 | [Safekeeper Paxos removal](9_safekeeper-paxos-removal.md) | pending | medium | safekeeper |
| 10 | [Collapse storage_ctl Diesel migrations](10_collapse-storage_ctl-diesel-migrations.md) | open | low | storage_ctl |
