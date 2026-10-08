#!/usr/bin/env python3
"""Moves mem0 memories from meko_mem0_collection to meko_mem0_memories (data
migration 000006), and back for a rollback. Covers the memory table and its
entity store ({name}_entities).

Exactly one mode per run:
  --copy    copy every source row the destination lacks. Online: run it while
            Meko is up, then compact the destination's tables.
  --sync    make the destination match the source exactly: insert missing
            rows, overwrite changed ones, delete rows the source no longer
            has. Only while nothing writes either table (the cutover pause),
            or it races live writes.
  --verify  compare row counts. Changes nothing.

--reverse swaps source and destination, for a rollback to the old table.

Every comparison is one primary-key lookup per row, driven from one partition
at a time. A plain join makes the planner hash the other table whole, which
spilled past temp_file_limit and failed on a 3-node test universe.

Requires psycopg 3 (pip install "psycopg[binary]"). The DSN has no default, so
the script never connects to a database nobody named:

  DSN=postgresql://... python sync.py --copy
  DSN=postgresql://... python sync.py --sync --parallel 8
  DSN=postgresql://... python sync.py --verify
"""

import argparse
import concurrent.futures as cf
import os
import sys
import time

import psycopg

COLS = (
    "meko_datapack_id, meko_user_id, meko_agent_id, id, meko_conversation_id, "
    "embeddings, payload, meko_created_at, meko_promotion_user_action"
)
PK = ("meko_datapack_id", "meko_user_id", "meko_agent_id", "meko_created_at", "id")
DATA = ("meko_conversation_id", "embeddings", "payload", "meko_promotion_user_action")


def pk_match(a: str, b: str) -> str:
    return " AND ".join(f"{a}.{c} = {b}.{c}" for c in PK)


def connect(dsn: str) -> psycopg.Connection:
    conn = psycopg.connect(dsn, autocommit=True)
    # Belt and braces for the lookups below: never let a comparison hash a table.
    conn.execute("SET statement_timeout = 0")
    conn.execute("SET enable_hashjoin = off")
    conn.execute("SET enable_mergejoin = off")
    return conn


def copy_partition(dsn, schema, src_part, dst):
    with connect(dsn) as conn:
        n = conn.execute(
            f"INSERT INTO {schema}.{dst} ({COLS}) SELECT {COLS} FROM {schema}.{src_part}"
            " ON CONFLICT DO NOTHING"
        ).rowcount
    return f"copy {src_part}: {n} rows"


def upsert_partition(dsn, schema, src_part, dst):
    """Insert the partition's rows the destination lacks and overwrite changed ones."""
    differs = " OR ".join(f"x.{c} IS DISTINCT FROM s.{c}" for c in DATA)
    pk = ", ".join(PK)
    updates = ", ".join(f"{c} = EXCLUDED.{c}" for c in DATA)
    with connect(dsn) as conn:
        n = conn.execute(
            f"""INSERT INTO {schema}.{dst} ({COLS})
                SELECT {", ".join("s." + c.strip() for c in COLS.split(","))}
                FROM {schema}.{src_part} s
                LEFT JOIN LATERAL (
                    SELECT true AS hit, {", ".join("n." + c for c in DATA)}
                    FROM {schema}.{dst} n WHERE {pk_match("n", "s")} LIMIT 1
                ) x ON true
                WHERE x.hit IS NULL OR {differs}
                ON CONFLICT ({pk}) DO UPDATE SET {updates}"""
        ).rowcount
    return f"upsert {src_part}: {n} rows inserted or updated"


def delete_partition(dsn, schema, dst_part, src):
    """Delete the partition's rows the source no longer has."""
    pk = ", ".join(PK)
    with connect(dsn) as conn:
        gone = conn.execute(
            f"""SELECT {", ".join("d." + c for c in PK)} FROM {schema}.{dst_part} d
                LEFT JOIN LATERAL (
                    SELECT true AS hit FROM {schema}.{src} s WHERE {pk_match("s", "d")} LIMIT 1
                ) x ON true
                WHERE x.hit IS NULL"""
        ).fetchall()
        if gone:
            where = " AND ".join(f"{c} = %s" for c in PK)
            with conn.transaction(), conn.cursor() as cur:
                cur.executemany(f"DELETE FROM {schema}.{dst_part} WHERE {where}", gone)
    return f"delete {dst_part}: {len(gone)} rows ({pk})"


def run(jobs, parallel):
    t0 = time.time()
    with cf.ThreadPoolExecutor(parallel) as ex:
        for line in ex.map(lambda j: j[0](*j[1:]), jobs):
            print(f"{line} ({time.time() - t0:.0f}s)", flush=True)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    mode = ap.add_mutually_exclusive_group(required=True)
    mode.add_argument("--copy", action="store_true")
    mode.add_argument("--sync", action="store_true")
    mode.add_argument("--verify", action="store_true")
    ap.add_argument("--reverse", action="store_true", help="old <- new, for a rollback")
    ap.add_argument("--dsn", default=os.environ.get("DSN"))
    ap.add_argument("--parallel", type=int, default=8)
    ap.add_argument("--schema", default="datapacks")
    ap.add_argument("--old", default="meko_mem0_collection")
    ap.add_argument("--new", default="meko_mem0_memories")
    ap.add_argument("--partitions", type=int, default=128, help="partitions per table")
    a = ap.parse_args()
    if not a.dsn:
        sys.exit("set --dsn or DSN")

    src, dst = (a.new, a.old) if a.reverse else (a.old, a.new)
    pairs = [(src, dst), (f"{src}_entities", f"{dst}_entities")]
    parts = range(a.partitions)
    t0 = time.time()

    if a.verify:
        with connect(a.dsn) as conn:
            mismatch = False
            for s, d in pairs:
                ns = conn.execute(f"SELECT count(*) FROM {a.schema}.{s}").fetchone()[0]
                nd = conn.execute(f"SELECT count(*) FROM {a.schema}.{d}").fetchone()[0]
                mismatch |= ns != nd
                print(f"{s}: {ns}  {d}: {nd}  {'MISMATCH' if ns != nd else 'ok'}")
        sys.exit(1 if mismatch else 0)

    for s, d in pairs:
        if a.copy:
            run([(copy_partition, a.dsn, a.schema, f"{s}_{i:03d}", d) for i in parts], a.parallel)
        else:
            run([(upsert_partition, a.dsn, a.schema, f"{s}_{i:03d}", d) for i in parts], a.parallel)
            run([(delete_partition, a.dsn, a.schema, f"{d}_{i:03d}", s) for i in parts], a.parallel)
    print(f"done in {time.time() - t0:.0f}s", flush=True)


if __name__ == "__main__":
    main()
