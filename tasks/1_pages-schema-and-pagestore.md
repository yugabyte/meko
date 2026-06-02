---
status: done
severity: low
area: yb-pageserver
opened: 2026-03-05
---

# Phase 1 — `pageserver.pages` schema + `PageStore` Rust module

- DDL for `pageserver.pages` and `pageserver.timeline_state` in
  YugabyteDB.
- `PageStore` (`pageserver/src/page_store.rs`) with full CRUD +
  generation helpers.
- 15 integration tests against a real YB cluster.
