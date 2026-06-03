---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-06-03
---

# Unwind the dead RTC plumbing

## Symptom

`cargo build -p pageserver --bin yb-pageserver` produces ~19 dead-code
warnings, all in or downstream of `RemoteTimelineClient`
(`pageserver/src/tenant/remote_timeline_client.rs`) and its
upload-queue / metrics helpers:

```
warning: associated items `started_now_for`, `blocked_by`, `with_reason`, and `without_reason` are never used
warning: field `gauge` is never read
warning: fields `dirty`, `clean`, `visible_remote_consistent_lsn`, and `task_counter` are never read
warning: fields `process_remote_consistent_lsn_updates` and `block_deletions` are never read
warning: fields `runtime`, `upload_queue`, `deletion_queue_client`, `fpu_page_store`, `config`, and `cancel` are never read
warning: fields `upload_queue_for_deletion` and `deleted_at` are never read
warning: function `index_part_to_timeline_row` is never used
warning: function `preserve_initdb_archive` is never used
warning: method `build_timeline_remote_client` is never used
warning: method `call_end` is never used
warning: method `is_stopping` is never used
warning: methods `is_previous_ancestor_lsn` and `is_detached_from_ancestor` are never used
warning: methods `no_pending_work`, `get_last_remote_consistent_lsn_visible`, and `get_last_remote_consistent_lsn_projected` are never used
warning: methods `set` and `get` are never used
warning: method `stopped_mut` is never used
warning: method `timeline` is never used
warning: method `update_remote_consistent_lsn` is never used
warning: method `will_decrement_manually` is never used
warning: variants `NotRunning`, `InProgress`, and `Successful` are never constructed
```

## Why they exist

Phase 4.d removed most of RTC's responsibilities — `index_part.json`
uploads, layer-file management, the delete-flag dance — and moved
the timeline state into `pageserver.timeline_state`. The RTC
struct and its upload-queue / metrics machinery survived as a
compiling shell because:

- `download_index_file` is still used at tenant startup
  (`Tenant::load_timeline_metadata`).
- The pageserver's tests + the `is_archived`/`is_invisible`
  surface still reach into the RTC.

Everything else flagged above is reachable only from other
flagged items — a self-referential dead chain. Deleting any leaf
fails to compile because a (dead) caller still references it;
deleting the whole chain in one go is a sizable refactor.

## Plan

Two stages, both schema-free:

1. **Slim RTC to the live surface.** Keep `RemoteTimelineClient::new`
   + `download_index_file` + `is_archived` / `is_invisible` reads.
   Delete the upload queue + the `init_upload_queue*` family + the
   `remote_consistent_lsn_*` accessors + `update_config` +
   `preserve_initdb_archive` + `index_part_to_timeline_row`. The
   struct loses ~6 of its 9 fields; the constructor drops the
   corresponding params; `Tenant::build_timeline_client` /
   `build_timeline_remote_client` simplify.
2. **Delete `tenant/upload_queue.rs` entirely** once RTC stops
   referencing `UploadQueue`/`UploadQueueInitialized`/
   `UploadQueueStopped*`. Drop the matching RTC-only metrics
   (`PerTimelineRemotePhysicalSizeGauge`,
   `RemoteTimelineClientCallMetricGuard::will_decrement_manually`,
   `RemoteTimelineClientMetrics::call_end`).

After both stages, `cargo build` should be warning-clean without
any `#[allow(dead_code)]` annotations.

## Notes

- Cross-link to [4.d layer-stack code deletion](4.d_layer-stack-code-deletion.md)
  — this is the final mile of that refactor; we shipped the
  intermediate state to keep the change set bounded.
- Touch lightly until task 12 (e2e infra) is solid — RTC changes
  the timeline-startup path, which needs fast end-to-end
  validation.
