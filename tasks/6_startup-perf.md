---
status: pending
severity: high
area: yb-pageserver
opened: 2026-05-15
---

# Phase 6 — Startup performance

Cold-start time-to-first-`getPage` is dominated by sequential setup
the layer-stack design baked in but yb-mode no longer needs.

## Status

- **(a) ✅** preload short-circuits to `PageStore::list_timelines`,
  skipping both the controller LIST and per-timeline
  `download_index_file`. `synth_index_part_from_row` builds an
  empty `IndexPart` from the row.
- **(c) ✅** `clean_up_timelines` gated on non-yb-mode, and the
  per-timeline `create_dir_all` in `load_remote_timeline` is gated
  off — no empty dentries created, nothing to purge.
- **(b)** deferred — measure first.
- **(d)** deferred — measure first.

## Target

Sub-second time-to-first-`getPage` on a clean restart for a tenant
with one timeline + a few hundred MB of recent writes.

## Current shape (as of `76c4b9fe9`)

`Tenant::preload` → `Tenant::attach` does roughly:

1. `remote_timeline_client::list_remote_timelines` — already
   YbStore-short-circuited (storage_controller `timelines`
   table, one query). Cheap.
2. `load_timelines_metadata` — spawns one `download_index_file`
   per timeline into a `JoinSet`. In yb-mode this is one
   YbStore blob `get` per timeline that almost always returns
   `NotFound`, triggering the `pageserver.timeline_state` synth
   path (`tenant.rs:1438`). One wasted round-trip per timeline.
3. `clean_up_timelines` — `read_dir` on `timelines_path` and
   purge dentries not in the just-fetched ID set. In yb-mode
   the only dentries are empty dirs we ourselves created at
   `load_remote_timeline:1695`.
4. `sorted_timelines` loop — serial `load_remote_timeline` per
   timeline. Each call: `create_dir_all(timeline_path)` then
   `timeline_init_and_sync` (allocates the `Timeline`,
   registers in the tenant map, starts no background tasks
   yet).
5. Activation — starts the walreceiver, which streams SK WAL
   from `disk_consistent_lsn` forward and replays into the
   cache / materialiser.

The synchronous chain that gates `time-to-first-getPage` is:
`list_timelines` → `download_index_file × N` →
`clean_up_timelines` → `load_remote_timeline × N` → activate →
walreceiver catches up to compute's `last_record_lsn`.

## Items

### (a) Drop the per-timeline `download_index_file` in yb-mode

The existing YbStore short-circuit returns IDs only. We then
issue N more `download_index_file` calls just to discover
(almost always) that `index_part.json` doesn't exist and
synthesise from `pageserver.timeline_state`. Two-step query
for what could be one.

**Fix sketch:** `PageStore::list_timelines(tenant_id)` already
exists (`page_store.rs:1286`) and returns the full
`Vec<TimelineRow>` in one SQL round-trip. Plumb the rows into
`TenantPreload` directly and skip both the controller LIST
and the per-timeline download in yb-mode. The
`MaybeDeletedIndexPart` machinery collapses to:
`IndexPart::empty(metadata_from_row)` for every row.

Net: from `1 + N` YB queries to `1`. Mechanical, no behaviour
change once yb-mode is on.

### (b) Parallelise per-timeline attach

`load_remote_timeline` is async but called serially in the
`sorted_timelines` loop. Ancestor must finish before
descendant, but siblings at the same depth can run
concurrently.

**Caveat:** the loop body locks `self.timelines` to look up
the ancestor (`tenant.rs:1700`) — already short. Real work
is `Timeline::new` + map insert. In the yb-light common case
(1 timeline per tenant) wall-clock savings are zero; benefit
only shows for ancestor-branched tenants.

**Defer** until (a)/(c)/(d) land and we have a real
measurement of where the remaining time goes.

### (c) Skip the local-disk dentry-purge in yb-mode

`clean_up_timelines` walks `timelines_path` and deletes dirs
whose ID isn't in `existent_timelines`. In yb-mode the
authoritative existence set is `pageserver.timeline_state`,
and the only dentries on disk are empty dirs we make at
attach time. Nothing to reconcile.

**Fix sketch:** gate the call on whether `compute_page_store`
is `Some` and skip in that case. Also stop creating the per-
timeline dir at `tenant.rs:1695` — yb-mode never writes layer
files there, so it's unused. Saves a syscall per timeline
plus one full `read_dir` per attach.

### (d) Shorten walreceiver catch-up from `disk_consistent_lsn`

After restart the walreceiver re-streams from
`disk_consistent_lsn` forward and re-applies every record up
to compute's `last_record_lsn`. With `flush_size_bytes = 25
MiB` the gap is up to 25 MiB of WAL — under steady write
load this is the dominant restart latency (seconds to tens
of seconds).

Two angles, pick after (a)/(c) land and profiling shows the
gap:

- **Persist a higher `last_applied_lsn` checkpoint.** Today
  `disk_consistent_lsn` advances only on full PageWriter
  flush. If the per-key LRU (Phase 4f) holds the page
  images we already inserted into `pageserver.pages`, we
  can advance a `last_applied_lsn` column on every
  successful insert batch and start replay from there
  instead of the flush watermark. Cuts the typical replay
  window from 25 MiB to ~64 KiB.
- **Lazy replay (deferred).** On `getPage(key, lsn)` for a
  key the LRU has at LSN ≥ requested, serve from cache
  and skip WAL apply for that key entirely. The walreceiver
  continues ingesting in the background. Riskier — needs
  careful proof the LRU's "as-of" LSN is correct under
  concurrent inserts.

Blocked on: a clean profile of restart latency with (a)+(c)
applied, to see if the walreceiver gap is even the
bottleneck.

## Suggested sequence

1. (a) and (c) together — small, mechanical, no behaviour
   change once yb-mode is on. Land + run e2e.
2. Profile a fresh restart against a busy tenant. Decide if
   (d) is worth the design work or if we're already
   sub-second.
3. (b) and (d) — only if step 2 says we still need them.

## Notes

- The `tokio::fs::create_dir_all(timeline_path)` at
  `tenant.rs:1695` is the only thing that puts a dir on disk
  per timeline in yb-mode. Removing it makes (c) trivial.
- `download_index_file` is the last live caller of
  `RemoteTimelineClient::download_index_file`. If (a) lands,
  `RemoteTimelineClient` collapses further — see
  [16](16_rtc-dead-code-unwind.md).
