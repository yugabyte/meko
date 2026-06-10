---
status: deferred
severity: medium
area: yb-pageserver
opened: 2026-05-01
---

# Phase 7 — Garbage collection

Cron that prunes old `pages` rows. User said "figure out separately
later".

This task exists as a placeholder so the design isn't forgotten.
Reopen when retention semantics + GC frequency need to be decided.

**Branch ancestor pins (added with task 22, lineage-aware reads):**
ancestor pruning must respect `min(child.ancestor_lsn)` across all
branches of a timeline — an ancestor row at `lsn ≤ cap` may be a
branch's only anchor source. Walk `timeline_state` for descendants
before pruning a timeline's rows.
