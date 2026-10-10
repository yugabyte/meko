#!/usr/bin/env python3
"""Moves mem0 memories from meko_mem0_collection to meko_mem0_memories (data
migration 000006) with Meko up the whole time. Covers the memory table and its
entity store ({name}_entities).

Exactly one mode per run:
  --mirror-on   install triggers that copy every insert, update and delete on
                either table to the other, in the same transaction. From then on
                a write to either table lands in both.
  --copy        copy every old row the new table lacks.
  --sync        make the destination match the source exactly: insert missing
                rows, overwrite changed ones, delete rows the source no longer has.
  --diff        count rows that differ between the tables. Exits 1 if any do.
  --mirror-off  drop the triggers.

--reverse swaps source and destination for --sync and --diff.

Run them in order: --mirror-on, --copy, --sync, --diff. If --diff reports
differences, repeat --sync and --diff until it reports none. The mirror catches
every write from the moment it is on; --copy and --sync bring over what was
there before, and fix the few rows a write changed while --copy was reading
them. The script's own writes are not mirrored.

Every comparison is one primary-key lookup per row, driven from one partition
at a time. A plain join makes the planner hash the other table whole, which
spilled past temp_file_limit and failed on a 3-node test universe.

Requires psycopg 3 (pip install "psycopg[binary]"). The DSN has no default, so
the script never connects to a database nobody named:

  DSN=postgresql://... python sync.py --mirror-on
"""

import argparse
import concurrent.futures as cf
import os
import sys
import time

import psycopg
from psycopg import errors

COLS = (
    "meko_datapack_id",
    "meko_user_id",
    "meko_agent_id",
    "id",
    "meko_conversation_id",
    "embeddings",
    "payload",
    "meko_created_at",
    "meko_promotion_user_action",
)
PK = ("meko_datapack_id", "meko_user_id", "meko_agent_id", "meko_created_at", "id")
DATA = ("meko_conversation_id", "embeddings", "payload", "meko_promotion_user_action")
PK_PARAMS = " AND ".join(f"{c} = ${i + 1}" for i, c in enumerate(PK))
TRIGGER = "meko_mem0_mirror"
RETRIES = 5

# The trigger function. TG_ARGV names the table to copy to. A write the mirror
# makes fires the other table's trigger one level down, which returns at once,
# so writes never bounce back and forth. Sessions that set meko.mem0_mirror=off
# (this script) are not mirrored either, and a dropped target is skipped, so a
# trigger left behind never fails the writes on the table it sits on.
#
# The old table's trigger runs AFTER the write and the new table's BEFORE it, so
# a write through either table locks the old row first. With both AFTER, two
# writes to one memory through different tables took the two row locks in
# opposite order and deadlocked (2-3% of writes with 16 writers on 1-2 rows).
MIRROR_FN = f"""
CREATE OR REPLACE FUNCTION {{schema}}.{TRIGGER}() RETURNS trigger
LANGUAGE plpgsql AS $fn$
DECLARE
  target text := format('%I.%I', TG_ARGV[0], TG_ARGV[1]);
  mirror boolean := pg_trigger_depth() = 1
                    AND current_setting('meko.mem0_mirror', true) IS DISTINCT FROM 'off'
                    AND to_regclass(target) IS NOT NULL;
  pk_moved boolean := TG_OP = 'UPDATE'
                      AND ({", ".join("OLD." + c for c in PK)})
                          IS DISTINCT FROM ({", ".join("NEW." + c for c in PK)});
BEGIN
  IF mirror AND (TG_OP = 'DELETE' OR pk_moved) THEN
    EXECUTE format('DELETE FROM %s WHERE {PK_PARAMS}', target)
      USING {", ".join("OLD." + c for c in PK)};
  END IF;
  IF mirror AND TG_OP <> 'DELETE' THEN
    EXECUTE format('INSERT INTO %s ({", ".join(COLS)})
                    VALUES ({", ".join(f"${i + 1}" for i in range(len(COLS)))})
                    ON CONFLICT ({", ".join(PK)}) DO UPDATE SET
                    {", ".join(f"{c} = EXCLUDED.{c}" for c in DATA)}', target)
      USING {", ".join("NEW." + c for c in COLS)};
  END IF;
  -- A BEFORE trigger that returns NULL cancels the write it fired for.
  IF TG_WHEN = 'AFTER' THEN
    RETURN NULL;
  ELSIF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END $fn$
"""


def pk_match(a: str, b: str) -> str:
    return " AND ".join(f"{a}.{c} = {b}.{c}" for c in PK)


def connect(dsn: str) -> psycopg.Connection:
    conn = psycopg.connect(dsn, autocommit=True)
    conn.execute("SET statement_timeout = 0")
    # Never let a comparison hash a table, and never mirror this script's writes.
    conn.execute("SET enable_hashjoin = off")
    conn.execute("SET enable_mergejoin = off")
    conn.execute("SET meko.mem0_mirror = 'off'")
    return conn


def retried(fn):
    """Retry a partition's statement when a concurrent write conflicts or deadlocks with it."""

    def run(*args):
        for attempt in range(RETRIES):
            try:
                return fn(*args)
            except (errors.SerializationFailure, errors.DeadlockDetected):
                if attempt == RETRIES - 1:
                    raise
                time.sleep(0.5 * 2**attempt)

    return run


@retried
def copy_partition(dsn, schema, src_part, dst):
    cols = ", ".join(COLS)
    with connect(dsn) as conn:
        n = conn.execute(
            f"INSERT INTO {schema}.{dst} ({cols}) SELECT {cols} FROM {schema}.{src_part}"
            " ON CONFLICT DO NOTHING"
        ).rowcount
    return f"copy {src_part}: {n} rows"


def _changed_rows(schema, src_part, dst):
    """Rows of the source partition the destination lacks or holds differently."""
    differs = " OR ".join(f"x.{c} IS DISTINCT FROM s.{c}" for c in DATA)
    return f"""FROM {schema}.{src_part} s
               LEFT JOIN LATERAL (
                   SELECT true AS hit, {", ".join("n." + c for c in DATA)}
                   FROM {schema}.{dst} n WHERE {pk_match("n", "s")} LIMIT 1
               ) x ON true
               WHERE x.hit IS NULL OR {differs}"""


def _gone_rows(schema, dst_part, src):
    """Rows of the destination partition the source no longer has. With hash and
    merge joins off, this plans as a batched nested-loop anti join on the source's
    primary key."""
    return f"""FROM {schema}.{dst_part} d
               WHERE NOT EXISTS (SELECT 1 FROM {schema}.{src} s WHERE {pk_match("s", "d")})"""


@retried
def upsert_partition(dsn, schema, src_part, dst):
    updates = ", ".join(f"{c} = EXCLUDED.{c}" for c in DATA)
    with connect(dsn) as conn:
        n = conn.execute(
            f"""INSERT INTO {schema}.{dst} ({", ".join(COLS)})
                SELECT {", ".join("s." + c for c in COLS)} {_changed_rows(schema, src_part, dst)}
                ON CONFLICT ({", ".join(PK)}) DO UPDATE SET {updates}"""
        ).rowcount
    return f"upsert {src_part}: {n} rows inserted or updated"


@retried
def delete_partition(dsn, schema, dst_part, src):
    # One statement, so the rows it finds gone and the rows it deletes come from
    # the same snapshot.
    with connect(dsn) as conn:
        n = conn.execute(f"DELETE {_gone_rows(schema, dst_part, src)}").rowcount
    return f"delete {dst_part}: {n} rows"


def count_changed(dsn, schema, src_part, dst):
    with connect(dsn) as conn:
        return conn.execute(f"SELECT count(*) {_changed_rows(schema, src_part, dst)}").fetchone()[0]


def count_gone(dsn, schema, dst_part, src):
    with connect(dsn) as conn:
        return conn.execute(f"SELECT count(*) {_gone_rows(schema, dst_part, src)}").fetchone()[0]


def run(jobs, parallel, quiet=False):
    t0 = time.time()
    out = []
    with cf.ThreadPoolExecutor(parallel) as ex:
        for result in ex.map(lambda j: j[0](*j[1:]), jobs):
            out.append(result)
            if not quiet:
                print(f"{result} ({time.time() - t0:.0f}s)", flush=True)
    return out


def mirror(dsn, schema, pairs, on):
    with connect(dsn) as conn, conn.transaction():
        if on:
            conn.execute(MIRROR_FN.format(schema=schema))
        for old, new in pairs:
            for src, dst, when in ((old, new, "AFTER"), (new, old, "BEFORE")):
                conn.execute(f"DROP TRIGGER IF EXISTS {TRIGGER} ON {schema}.{src}")
                if on:
                    conn.execute(
                        f"CREATE TRIGGER {TRIGGER} {when} INSERT OR UPDATE OR DELETE"
                        f" ON {schema}.{src} FOR EACH ROW"
                        f" EXECUTE FUNCTION {schema}.{TRIGGER}('{schema}', '{dst}')"
                    )
                print(f"{'mirroring' if on else 'stopped mirroring'} {src} -> {dst}")
        if not on:
            conn.execute(f"DROP FUNCTION IF EXISTS {schema}.{TRIGGER}()")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    mode = ap.add_mutually_exclusive_group(required=True)
    for m in ("--mirror-on", "--copy", "--sync", "--diff", "--mirror-off"):
        mode.add_argument(m, action="store_true")
    ap.add_argument("--reverse", action="store_true", help="new -> old, for --sync and --diff")
    ap.add_argument("--dsn", default=os.environ.get("DSN"))
    ap.add_argument("--parallel", type=int, default=8)
    ap.add_argument("--schema", default="datapacks")
    ap.add_argument("--old", default="meko_mem0_collection")
    ap.add_argument("--new", default="meko_mem0_memories")
    ap.add_argument("--partitions", type=int, default=128, help="partitions per table")
    a = ap.parse_args()
    if not a.dsn:
        sys.exit("set --dsn or DSN")

    tables = [(a.old, a.new), (f"{a.old}_entities", f"{a.new}_entities")]
    if a.mirror_on or a.mirror_off:
        mirror(a.dsn, a.schema, tables, a.mirror_on)
        return

    pairs = [(n, o) for o, n in tables] if a.reverse else tables
    parts = range(a.partitions)
    t0 = time.time()

    if a.diff:
        total = 0
        for s, d in pairs:
            changed = sum(
                run(
                    [(count_changed, a.dsn, a.schema, f"{s}_{i:03d}", d) for i in parts],
                    a.parallel,
                    quiet=True,
                )
            )
            gone = sum(
                run(
                    [(count_gone, a.dsn, a.schema, f"{d}_{i:03d}", s) for i in parts],
                    a.parallel,
                    quiet=True,
                )
            )
            total += changed + gone
            print(f"{s} -> {d}: {changed} missing or different, {gone} extra")
        print(f"done in {time.time() - t0:.0f}s", flush=True)
        sys.exit(1 if total else 0)

    for s, d in pairs:
        if a.copy:
            run([(copy_partition, a.dsn, a.schema, f"{s}_{i:03d}", d) for i in parts], a.parallel)
        else:
            run([(upsert_partition, a.dsn, a.schema, f"{s}_{i:03d}", d) for i in parts], a.parallel)
            run([(delete_partition, a.dsn, a.schema, f"{d}_{i:03d}", s) for i in parts], a.parallel)
    print(f"done in {time.time() - t0:.0f}s", flush=True)


if __name__ == "__main__":
    main()
