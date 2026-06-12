---
status: pending
severity: medium
area: yb-pageserver
opened: 2026-06-11
---

# Skip or optimize writing zeroed gap pages to YB

## Problem

Relation-extension gap-fill (`handle_rel_extend` / `ingest_batch` →
`put_rel_page_image_zero`) writes a full 8192-byte all-zero payload
row into `pageserver.pages` for every gap block. Even in healthy
operation that is one 8 KiB zero blob per block of every insert-heavy
relation — the task-28 forensics showed the legit creation-time zero
image on essentially every block of rel 16414 (~4.1 M blocks ≈ tens of
GB of literal zeros in YB), all of it shipped over the wire per flush,
stored, replicated, and re-read by chain assembly forever after.

## Options (sketch, decreasing ambition)

1. **Don't write gap rows at all; synthesize zeros on read.** A read
   of blk B at LSN T returns a zero page when B < rel_size@T and no
   row ≤ T exists. Most efficient, but changes load-bearing
   semantics: "no row" currently distinguishes a WAL hole (corruption
   detector) from a legit empty page, and the truncate-then-regrow
   case REQUIRES a zero to shadow the stale pre-truncate rows — the
   synthesized zero must therefore be derived from the LSN-versioned
   size key (e.g. "no row in (truncation_lsn, T]" logic), not just
   row absence. Touches read path, chain assembly, and the task-28
   shadowing assertions in e2e. Highest risk, do not start here.
2. **Sentinel row with empty payload.** Keep one row per gap block
   (`is_image=true, will_init=true`) but with a zero-length payload;
   chain assembly / walredo feed expands it to a full zero page on
   read. Preserves all presence semantics (hole detection,
   truncate-regrow shadowing, the zero-shadow e2e assertions keyed on
   all-zero payloads need updating to match the sentinel) at ~0.1% of
   the bytes. Probably the sweet spot.
3. **Compression.** Zero pages compress to almost nothing under any
   codec; enable payload compression (or rely on YB-side compression
   if available) for image rows. No semantic change, smallest win on
   CPU-bound paths since the 8 KiB still crosses the Rust↔YB wire
   uncompressed unless done client-side.

Also worth measuring first: how much of `pageserver.pages` by bytes is
all-zero images on a real W10 dataset (one `sum(length(payload))
filter (where payload = ...zeros)` query) — to size the prize before
choosing.

## Notes / links

- Gap-fill call sites: `walingest.rs:1441` (`handle_rel_extend`),
  `pgdatadir_mapping.rs` `ingest_batch`/`zero_gaps`,
  `put_rel_page_image_zero`.
- [task 28](28_walredo-corruption-catch-me-if-you-can.md) — the
  zero-shadowing corruption made these rows pathological (thousands
  per block); fixed, but the healthy-path volume remains. The
  `restart_relsize_zero_shadow` e2e assertions match on all-zero
  payloads and must evolve with whichever option is chosen.
