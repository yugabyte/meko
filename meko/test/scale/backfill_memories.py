"""Copy meko_mem0_collection (and _entities) into the datapack-keyed tables from data
migration 000006, one source partition at a time.

Idempotent: ON CONFLICT DO NOTHING on the shared primary key, so a rerun, or a
catch-up pass after MCP has switched collections, copies only what is missing.
Rows updated in the old table after they were copied are NOT re-copied; the
catch-up pass is for inserts made by MCP pods still on the old collection.

  python backfill_memories.py --parallel 8 [--since '2026-09-29T00:00Z']

Then compact the new tables (run.sh compact): until then their vector index answers
about 15x slower.
"""

import argparse
import concurrent.futures as cf
import os
import time

import psycopg

COLS = (
    "meko_datapack_id, meko_user_id, meko_agent_id, id, meko_conversation_id, "
    "embeddings, payload, meko_created_at, meko_promotion_user_action"
)
PAIRS = (
    ("meko_mem0_collection", "meko_mem0_memories"),
    ("meko_mem0_collection_entities", "meko_mem0_memories_entities"),
)


def copy_partition(dsn: str, src: str, dst: str, since: str | None) -> tuple[str, int, float]:
    where = "WHERE meko_created_at >= %(since)s" if since else ""
    t = time.time()
    with psycopg.connect(dsn, autocommit=True) as conn:
        cur = conn.execute(
            f"INSERT INTO datapacks.{dst} ({COLS}) SELECT {COLS} FROM datapacks.{src} {where}"
            " ON CONFLICT DO NOTHING",
            {"since": since},
        )
        return src, cur.rowcount, time.time() - t


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--dsn", default=os.environ.get("DSN", "postgresql://yugabyte@yb-tservers:5433/yugabyte")
    )
    ap.add_argument("--parallel", type=int, default=8)
    ap.add_argument(
        "--since", help="only rows with meko_created_at at or after this (catch-up pass)"
    )
    a = ap.parse_args()

    jobs = [(f"{src}_{i:03d}", dst) for src, dst in PAIRS for i in range(128)]
    t0, total = time.time(), 0
    with cf.ThreadPoolExecutor(a.parallel) as ex:
        for src, n, secs in ex.map(lambda j: copy_partition(a.dsn, j[0], j[1], a.since), jobs):
            total += n
            print(
                f"{src}: {n} rows in {secs:.1f}s (total {total}, {time.time() - t0:.0f}s)",
                flush=True,
            )
    print(f"done: {total} rows in {time.time() - t0:.0f}s", flush=True)


if __name__ == "__main__":
    main()
