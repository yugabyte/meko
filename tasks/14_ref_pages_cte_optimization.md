# Pageserver `pages` CTE query optimization

Analysis of the two hot `pageserver.pages` read queries from `pg_stat_statements`
(yb_storage DB). Both follow the same "find anchor image, return versions since"
pattern and share the same root inefficiency.

Table schema:

```
PRIMARY KEY ((tenant_id, timeline_id, spcnode, dbnode, relnode, forknum) HASH,
             blknum ASC, is_image ASC, lsn DESC)
```

Test data used for measurements: single hash key, 200 blocks × 1000 lsn versions
each (200k rows), one image (`is_image=true`) every 100 versions.

---

## Query 1 — single block (`WITH anchor`)

~76k calls, mean ~0.5 ms. For one block, find the latest image at `lsn <= $8`,
then return all versions from that image's lsn up to `$8`, ordered `lsn DESC`.

```sql
WITH anchor AS (
  SELECT lsn AS anchor_lsn FROM "pageserver".pages
  WHERE tenant_id=$1 AND timeline_id=$2 AND spcnode=$3 AND dbnode=$4 AND relnode=$5 AND forknum=$6
    AND blknum=$7 AND is_image=$9 AND lsn <= $8
  ORDER BY lsn DESC LIMIT $10
)
SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn, p.is_image, p.will_init, p.payload
FROM "pageserver".pages p LEFT JOIN anchor ON $11
WHERE p.tenant_id=$1 AND ... AND p.forknum=$6
  AND p.blknum=$7 AND (p.lsn >= anchor.anchor_lsn OR anchor.anchor_lsn IS NULL) AND p.lsn <= $8
ORDER BY p.lsn DESC;
```

### Findings

| Variant | Outer rows scanned | Sort? | Anchor lookup | ~Exec time |
|---|---|---|---|---|
| **Current** (orig PK + LEFT JOIN) | **951** | yes | optimal (1 seek) | ~4.2 ms |
| Reorder only (lsn before is_image) | 951 | no | degraded (~150 nexts) | ~3.6 ms |
| **Rewrite only** (current PK, InitPlan) | **51** | yes | optimal (1 seek) | **~1.55 ms** |
| Rewrite + reorder | 51 | no | degraded | ~1.3–2.0 ms |

**Root cause is query structure, not column order.** The outer query scans 951
rows to return 51 because `LEFT JOIN anchor ON true` + `(p.lsn >= anchor_lsn OR
anchor_lsn IS NULL)` prevents the planner from using `anchor_lsn` as an index
**lower bound**. Only `lsn <= $8` is pushed; everything down to lsn 0 is scanned
and the surplus dropped by a post-join filter.

**Column reorder (`lsn` before `is_image`) is a secondary effect** — it only removes
the `ORDER BY lsn DESC` sort, AND it *degrades* the anchor CTE (with `is_image`
trailing, `WHERE is_image=true ORDER BY lsn DESC LIMIT 1` can no longer seek
straight to the image; it walks lsn descending and filters). The CTE and outer
query want opposite orderings of `is_image` vs `lsn`, so no single PK is optimal
for both. Not worth a table-recreate migration.

### Recommended fix — rewrite, no schema change (~2.7× faster, 951→51 rows scanned)

Make the anchor a scalar `InitPlan` so its value is pushable as an index lower bound:

```sql
SELECT p.spcnode, p.dbnode, p.relnode, p.forknum, p.blknum, p.lsn, p.is_image, p.will_init, p.payload
FROM "pageserver".pages p
WHERE p.tenant_id=$1 AND ... AND p.forknum=$6
  AND p.blknum=$7 AND p.lsn <= $8
  AND p.lsn >= COALESCE((
        SELECT a.lsn FROM "pageserver".pages a
        WHERE a.tenant_id=$1 AND ... AND a.forknum=$6
          AND a.blknum=$7 AND a.is_image=$9 AND a.lsn <= $8
        ORDER BY a.lsn DESC LIMIT $10), 0)
ORDER BY p.lsn DESC;
```

Semantically equivalent: `COALESCE(..., 0)` reproduces the "no image ⇒ return all
versions ≤ $8" branch (lsn ≥ 0 always). Verified the lower bound is pushed into
the DocDB scan (no "Rows Removed by Filter"; rows scanned = rows returned).

---

## Query 2 — multi-block range (`WITH per_block_image`)

~59k calls, mean ~1.5 ms, ~88 s total (the more expensive of the two). For a
contiguous block range `[$7, $8)`, find each block's latest image at `lsn <= $9`,
then return all versions from that image up to `$9`, ordered `blknum, lsn DESC`.

```sql
WITH per_block_image AS (
  SELECT blknum, MAX(lsn) AS image_lsn FROM "pageserver".pages
  WHERE tenant_id=$1 AND ... AND forknum=$6
    AND blknum >= $7 AND blknum < $8 AND is_image=$10 AND lsn <= $9
  GROUP BY blknum
)
SELECT p.spcnode, ..., p.payload
FROM "pageserver".pages p LEFT JOIN per_block_image bi ON p.blknum = bi.blknum
WHERE p.tenant_id=$1 AND ... AND p.forknum=$6
  AND p.blknum >= $7 AND p.blknum < $8 AND p.lsn <= $9
  AND (bi.image_lsn IS NULL OR p.lsn >= bi.image_lsn)
ORDER BY p.blknum, p.lsn DESC;
```

### Findings — same root cause, amplified per block

The per-block `image_lsn` lower bound is applied as a **post-join hash filter**,
so the outer scan reads **every version `<= $9` for every block** and drops the
surplus. Measured on the 200×1000 dataset:

| Range | Variant | Rows scanned | ~Exec time |
|---|---|---|---|
| [50,150) 100 blks | **Current** | **100,000** | ~135 ms |
| [50,150) 100 blks | LATERAL rewrite | **10,000** | ~77 ms |
| [50,60) 10 blks | **Current** | **10,000** | ~19 ms |
| [50,60) 10 blks | LATERAL rewrite | **1,010** | ~11 ms |

**~10× fewer rows scanned, ~1.8× faster.** (The final `blknum, lsn DESC` sort
remains, but the *current* query pays that too — it's inherent to the result
size, not added by the rewrite.) Column reorder again only removes a tiny
per-block inner sort and degrades the anchor lookup — not worth it.

### Recommended fix — DISTINCT-driven LATERAL (no schema change)

Iterate the blocks that actually exist (YB serves `SELECT DISTINCT blknum` with a
**Distinct Index Scan** / loose scan — 1 row per block, cheap), and run the
query-1 pushdown per block via `LATERAL`:

```sql
WITH blocks AS (
  SELECT DISTINCT blknum FROM "pageserver".pages
  WHERE tenant_id=$1 AND ... AND forknum=$6
    AND blknum >= $7 AND blknum < $8 AND lsn <= $9
)
SELECT p.spcnode, ..., p.payload
FROM blocks b
CROSS JOIN LATERAL (
  SELECT p.spcnode, ..., p.payload
  FROM "pageserver".pages p
  WHERE p.tenant_id=$1 AND ... AND p.forknum=$6
    AND p.blknum = b.blknum AND p.lsn <= $9
    AND p.lsn >= COALESCE((
          SELECT a.lsn FROM "pageserver".pages a
          WHERE a.tenant_id=$1 AND ... AND a.forknum=$6
            AND a.blknum = b.blknum AND a.is_image=$10 AND a.lsn <= $9
          ORDER BY a.lsn DESC LIMIT 1), 0)
  ORDER BY p.lsn DESC
) p
ORDER BY p.blknum, p.lsn DESC;
```

Semantically identical to the original (same block set, same per-block lsn range).
The `DISTINCT`-driven form is preferred over `generate_series($7,$8-1)` because it
only visits blocks that exist — robust whether the range is dense or sparse,
whereas `generate_series` probes every integer in `[$7,$8)`. For dense ranges
`generate_series` is marginally faster but riskier on wide sparse ranges.

---

## Summary

Both queries are slow for the **same structural reason**: the anchor/image lower
bound comes through a `LEFT JOIN ... ON true` + `OR ... IS NULL`, which the planner
cannot turn into an index lower bound, so it over-scans every version down to the
ceiling and filters. The fix in both cases is to make the lower bound **pushable**:

- Query 1: scalar `InitPlan` + `COALESCE(..., 0)` — ~2.7×, 951→51 rows scanned.
- Query 2: `DISTINCT`-driven `LATERAL` applying the same per-block pushdown — ~1.8×, 10× fewer rows scanned.

Neither needs a schema change.

---

## Revisited: dense images (every 20–40 versions) + free migration

Re-measured with images every ~30 versions (34 images/block over 1000 versions),
which matches the real workload. Median execution time over 12–15 runs each:

### Query 1 (single block, ~30 rows returned)

| Form | median | vs current |
|---|---|---|
| current (orig PK, LEFT JOIN) | 4.31 ms | 1.0× |
| **rewrite** (orig PK, InitPlan) | 1.88 ms | **2.3×** |
| **rewrite + reorder** (lsn before is_image) | **1.51 ms** | **2.85×** |

The reorder adds **~24%** on top of the rewrite. The anchor-lookup degradation we
worried about is empirically negligible with dense images — the `is_image=true`
walk is only ~30 rows (the `Estimated Seeks: 1002` in the plan is a bogus planner
estimate; actual anchor time ~0.65 ms). Removing the outer `lsn DESC` sort more
than pays for it on this small/cheap query.

### Query 2 (100 blocks)

| Form | median | vs current |
|---|---|---|
| current (orig PK, hash join) | 146.7 ms | 1.0× |
| **rewrite** (orig PK, DISTINCT LATERAL) | 49.1 ms | **3.0×** |
| rewrite + reorder | 49.2 ms | 3.0× (no change) |

The reorder gives **nothing** here. After the rewrite, Q2's residual cost is the
~100 per-block round-trips (the nested loop, ~45 ms), which dwarfs sorting — the
reorder only removes tiny per-block inner sorts. Reducing Q2 further would require
cutting per-block round-trips (batched storage reads), not a column reorder.

### Verdict (dense images, migration free)

1. **Do both rewrites** — the dominant, schema-free wins (Q1 2.3×, Q2 3.0×).
2. **Reorder `lsn` before `is_image` is a worthwhile add-on, but only for Query 1**
   (~24% extra on the highest-frequency query, 76k calls). It's neutral for Query 2
   and harmless for the insert path (same key, same hash). With dense images the
   old anchor-degradation objection no longer applies.
3. Net: rewrite both queries; optionally reorder the PK to squeeze the extra ~24%
   out of Q1. The reorder is no longer "not recommended" given free migration +
   dense images — it's just a secondary win concentrated in Q1.

---
---

# Write-path analysis (`INSERT` into `pages`, fast path, GUCs, COPY)

Separate investigation of the hot `INSERT INTO "pageserver".pages ... VALUES (...)`
path (500-row batched insert; ~13k calls/variant, ~6.6M rows). Test table is an
exact copy of `pages` (same hash + range PK). Cluster is **RF1**.

## Single-shard "fast path" vs distributed transaction

YB has three write modes (`src/yb/yql/pggate/ybc_pg_typedefs.h`):

```c
YB_SINGLE_SHARD_TRANSACTION,  // fast path, FULL ACID — only valid for ONE tablet
YB_NON_TRANSACTIONAL,         // drop ACID to avoid distributed-txn overhead
YB_TRANSACTIONAL              // distributed txn (the common case)
```

**The EXPLAIN plan tells you which** — run `EXPLAIN (ANALYZE, DIST) ...` and look
for the `Transaction:` line:

| EXPLAIN output | Meaning |
|---|---|
| `Transaction: Fast Path` present | single-shard / "direct" txn, no coordinator |
| no `Transaction:` line | distributed transaction |

Asymmetric: the backend only ever prints the string `"Fast Path"`
(`explain.c:1784`, guarded by `YBCCurrentTransactionUsesFastPath()`); a distributed
txn is signalled by the *absence* of the line.

**What decides it (plan shape, not tablet count).** `ModifyTableIsSingleRowWrite()`
in `src/postgres/src/backend/optimizer/util/ybplan.c` requires the node feeding the
`ModifyTable` to be a bare `Result` (emits exactly one tuple, no DocDB round-trip):

```c
Plan *plan = outerPlan(&modifyTable->plan);
if (!IsA(plan, Result) || outerPlan(plan)) return false;   // not single-row
```

Also disqualifying: >1 result relation, `ON CONFLICT`, init plans/subplans,
transactional expressions in target/RETURNING lists.

Verified empirically with the distributed-txn pool counter
(`transaction_pool_cache_queries` on the tserver `:9000/prometheus-metrics`, which
increments once per distributed txn started):

| Insert | pool delta | `Transaction:` line | Type |
|---|---|---|---|
| 1 row | 0 | `Fast Path` | single-shard (`Result` plan) |
| 2 rows, **same** hash key (1 tablet) | 1 | — | distributed |
| 500 rows, same hash key | 1 | — | distributed |
| 500 rows, varying hash keys | 1 | — | distributed |

**Conclusion:** the production multi-row `INSERT ... VALUES (...500 rows...)` is
**always a distributed transaction**, even though every row lands on one tablet —
because the plan is a `Values Scan`, not a `Result`. Only a single-row insert
qualifies for the fast path. This is by design: a write spanning >1 row can't be
proven single-tablet at plan time, and atomicity across rows needs a distributed
txn. RF doesn't change the type — RF1 only makes both the flush and commit cheaper
(no follower Raft acks).

`EXPLAIN (ANALYZE, DIST, COMMIT)` surfaces the otherwise-hidden commit phase:
fast-path commit ≈ 0.01 ms (fused into the write); the 500-row distributed commit
≈ 0.7–0.8 ms (a separate RPC to the transaction status tablet).

## `yb_enable_upsert_mode` (read-before-write elimination)

`SET yb_enable_upsert_mode = on` makes inserts blind upserts (skips the duplicate-
key / constraint read). 500-row insert, `EXPLAIN (ANALYZE, DIST)`:

| | upsert **on** | upsert **off** (default) |
|---|---|---|
| Execution Time | **20.2 ms** | **88.9 ms** |
| Storage Flush Exec Time | 7.5 ms | 79.1 ms |
| Storage Write Requests | 500 | 500 |
| Storage Read Requests | 0 | 0 |

~**4× faster**. The difference isn't extra read *RPCs* (both report 0) — it's the
flush cost: with upsert off, each write enforces PK uniqueness via a read-modify-
write baked into the DocDB write path. Tradeoff: no duplicate-key detection (it
silently overwrites) — fine for an idempotent/replayable page-write workload.

## "Force all writes to fast path?" — use `yb_disable_transactional_writes`

The GUC already exists; **no new GUC needed.** `SET yb_disable_transactional_writes
= on` rewrites each non-system write from `YB_TRANSACTIONAL` to
`YB_NON_TRANSACTIONAL` (`YBCFixTransactionSetting`, `ybModifyTable.c:458`).
Verified: the 500-row insert then reports **`Transaction: Fast Path`**, pool
delta = **0**, commit ≈ 0.05 ms.

**Important:** this is `YB_NON_TRANSACTIONAL`, NOT the ACID
`YB_SINGLE_SHARD_TRANSACTION`. A multi-tablet write cannot be both fast and
single-shard-ACID, so the GUC drops atomicity instead:

- **No atomicity / rollback** — if the statement errors midway (or the node dies),
  already-flushed rows stay durably written.
- **Secondary-index corruption risk** — base row and each index entry are separate
  non-txn ops on possibly different tablets; a failure between them orphans entries.
  (`pages` has only the PK, so this specific risk is low for that table; it makes
  the GUC unsafe to force cluster-wide.)
- No snapshot atomicity for readers.

Core only uses `YB_NON_TRANSACTIONAL` in controlled contexts: index backfill,
bulk COPY, and opt-in via this GUC. For the `pages` ingest path it's a legitimate
win **if** the writer retries idempotently — scope it narrowly with
`SET LOCAL yb_disable_transactional_writes = on` (+ `yb_enable_upsert_mode = on`)
on the ingest connection, not globally.

## COPY behavior

`COPY ... FROM` on a **non-colocated** (hash-sharded) table runs as **batched
distributed transactions**, default `rows_per_transaction = 20000`
(`DEFAULT_BATCH_ROWS_PER_TRANSACTION`, `copy.h:81`), committing each batch
independently via `YBCommitTransactionIntermediate()` (`copyfrom.c:1410`).
Verified (50k-row COPY, pool delta = batches):

| COPY of 50k rows | distributed txns |
|---|---|
| default (RPT 20000) | 3 (⌈50000/20000⌉) |
| `ROWS_PER_TRANSACTION 10000` | 5 |
| `ROWS_PER_TRANSACTION 0` | 1 (whole file, one txn) |

The **non-transactional** COPY fast path is **colocated-only** (`copyfrom.c:758`,
gated on `is_colocated && !triggers/rules/FK && !txn block`): it sets
`yb_disable_transactional_writes=true`, `batch_size=0`, streaming pipelined writes
with no commits. Non-colocated can't use it because rows + their index entries are
hash-distributed across tablets and need a distributed txn for consistency.

**Caveat:** default non-colocated COPY is **not atomic** — each 20000-row batch
commits independently, so a mid-file failure leaves earlier batches committed. Use
`ROWS_PER_TRANSACTION 0` for an all-or-nothing COPY (one large distributed txn,
higher intent/memory pressure).

### Write-mode summary

| Operation | Transaction behavior |
|---|---|
| single-row INSERT | single-shard fast path (ACID, no distributed txn) |
| multi-row INSERT ... VALUES | one distributed txn (whole statement) |
| INSERT + `yb_disable_transactional_writes=on` | non-transactional (fast, no atomicity) |
| COPY, colocated | non-transactional pipelined writes (no distributed txn) |
| COPY, non-colocated | distributed txn per `ROWS_PER_TRANSACTION` batch (default 20000; `0` ⇒ one txn) |

