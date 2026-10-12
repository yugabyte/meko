"""Seed datapacks [--start, --end) straight into the tables api_server and mem0 write.

The API cannot create 1M datapacks today (the yb_universes cap of 1000, 60 creates/min
per PAT), and test 1 measures that path on its own. This writes the same rows the API
would: meko_system.users + agents per account, datapacks + agent_access per datapack,
both memory tables (meko_mem0_collection and data migration 000006's meko_mem0_memories,
in the shape meko-mem0's PGVector.insert writes) and datapacks.knowledge_bases
for KB chunks (the shape rag-agent writes). Langfuse mappings and PATs, which only the
MCP tests need, come from stack/seed_stack.sh.

Idempotent per datapack: a datapack whose meko_system row exists is skipped whole, so a
worker that died mid-range can be rerun over the same range. Rows of a datapack are
written before its meko_system row, so "row exists" means "fully seeded".

Sharding: worker k of W takes every datapack i in range with i % W == k.
"""

import argparse
import json
import os
import sys
import time

import psycopg
from pgvector.psycopg import register_vector

import workload as w


def ensure_universe(conn: psycopg.Connection):
    with conn.cursor() as cur:
        # The API assigns every datapack a universe; the seeded one is capped at 1000.
        cur.execute("UPDATE meko_system.yb_universes SET max_datapacks = 100000000")
        cur.execute(
            "SELECT universe_id, ysql_admin_connection_string FROM meko_system.yb_universes LIMIT 1"
        )
        return cur.fetchone()


def ensure_accounts(conn: psycopg.Connection, accounts: set) -> None:
    """The user and default agent of every account a batch touches."""
    ids = [w.account_user_id(a) for a in sorted(accounts)]
    emails = [f"acct-{a}@scale.test" for a in sorted(accounts)]
    with conn.cursor() as cur:
        cur.execute(
            "INSERT INTO meko_system.users"
            " (user_id, user_email, user_first_name, user_last_name, user_type)"
            " SELECT u, e, 'Scale', 'Account', 'enterprise'"
            " FROM unnest(%s::uuid[], %s::text[]) AS t(u, e)"
            " ON CONFLICT DO NOTHING",
            (ids, emails),
        )
        cur.execute(
            "INSERT INTO meko_system.agents (user_id, agent_id)"
            " SELECT u, %s FROM unnest(%s::uuid[]) AS t(u) ON CONFLICT DO NOTHING",
            (w.AGENT_ID, ids),
        )
    conn.commit()


def existing(conn: psycopg.Connection, ids: list) -> set:
    with conn.cursor() as cur:
        cur.execute(
            "SELECT datapack_id FROM meko_system.datapacks WHERE datapack_id = ANY(%s)", (ids,)
        )
        return {r[0] for r in cur.fetchall()}


def copy_rows(conn, table: str, cols: list[str], types: list[str], rows) -> int:
    n = 0
    with conn.cursor() as cur:
        with cur.copy(f"COPY {table} ({', '.join(cols)}) FROM STDIN WITH (FORMAT BINARY)") as cp:
            cp.set_types(types)
            for r in rows:
                cp.write_row(r)
                n += 1
    return n


def mem_rows(i: int) -> list:
    dp, user = w.datapack_id(i), w.user_of(i)
    rows = []
    for j, v in enumerate(w.vectors(i, "mem")):
        text = f"synthetic memory {j} of datapack {i}"
        payload = json.dumps({"data": text, "text_lemmatized": text, "user_id": str(user)})
        rows.append((dp, user, w.AGENT_ID, w.row_id(i, "mem", j), v, payload))
    return rows


def kb_rows(i: int):
    dp = w.datapack_id(i)
    doc = w.row_id(i, "doc", 0)
    for j, v in enumerate(w.vectors(i, "kb")):
        yield (dp, w.row_id(i, "kb", j), f"synthetic chunk {j} of datapack {i}", v, doc)


NEW_TABLE_ONLY = os.environ.get("NEW_TABLE_ONLY") == "1"

MEM_COLS = ["meko_datapack_id", "meko_user_id", "meko_agent_id", "id", "embeddings", "payload"]
MEM_TYPES = ["uuid", "uuid", "text", "uuid", "vector", "jsonb"]


def seed_one(conn, i: int, universe) -> int:
    dp, user = w.datapack_id(i), w.user_of(i)
    mems = mem_rows(i)
    # Both memory schemas get the same rows, so either can be measured without a
    # backfill. NEW_TABLE_ONLY skips the old one, which the fixed build never reads,
    # to halve the memory rows past the range the comparisons use.
    n = copy_rows(conn, "datapacks.meko_mem0_memories", MEM_COLS, MEM_TYPES, mems)
    if not NEW_TABLE_ONLY:
        copy_rows(conn, "datapacks.meko_mem0_collection", MEM_COLS, MEM_TYPES, mems)
    n += copy_rows(
        conn,
        "datapacks.knowledge_bases",
        ["tenant_id", "id", "chunk_text", "embeddings", "document_id"],
        ["uuid", "uuid", "text", "vector", "uuid"],
        kb_rows(i),
    )
    with conn.cursor() as cur:
        cur.execute(
            "INSERT INTO meko_system.datapacks (datapack_id, user_id, created_by_agent_id,"
            " datapack_name, universe_id, datapack_details, datapack_ysql_connection_string,"
            " datapack_ysql_username, datapack_ysql_password)"
            " VALUES (%s, %s, %s, %s, %s, %s, %s, 'onemil', 'unused')",
            (
                dp,
                user,
                w.AGENT_ID,
                f"onemil-{i:07d}",
                universe[0],
                json.dumps({"description": f"scale-test datapack {i} ({w.size_class(i)})"}),
                universe[1],
            ),
        )
        cur.execute(
            "INSERT INTO meko_system.agent_access (user_id, agent_id, datapack_id)"
            " VALUES (%s, %s, %s) ON CONFLICT DO NOTHING",
            (user, w.AGENT_ID, dp),
        )
    conn.commit()
    return n


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--dsn", default=os.environ.get("DSN", "postgresql://yugabyte@yb-tservers:5433/yugabyte")
    )
    ap.add_argument("--start", type=int, required=True)
    ap.add_argument("--end", type=int, required=True)
    ap.add_argument("--worker", type=int, default=int(os.environ.get("JOB_COMPLETION_INDEX", 0)))
    ap.add_argument("--workers", type=int, default=1)
    ap.add_argument("--batch", type=int, default=200, help="datapacks per existence check")
    a = ap.parse_args()

    conn = psycopg.connect(a.dsn)
    register_vector(conn)
    universe = ensure_universe(conn)
    conn.commit()

    mine = [i for i in range(a.start, a.end) if i % a.workers == a.worker]
    t0, done, skipped, rows = time.time(), 0, 0, 0
    for b in range(0, len(mine), a.batch):
        batch = mine[b : b + a.batch]
        have = existing(conn, [w.datapack_id(i) for i in batch])
        ensure_accounts(conn, {w.account_of(i) for i in batch if w.datapack_id(i) not in have})
        for i in batch:
            if w.datapack_id(i) in have:
                skipped += 1
                continue
            rows += seed_one(conn, i, universe)
            done += 1
        dt = time.time() - t0
        print(
            json.dumps(
                {
                    "worker": a.worker,
                    "progress": b + len(batch),
                    "of": len(mine),
                    "seeded": done,
                    "skipped": skipped,
                    "vector_rows": rows,
                    "secs": round(dt, 1),
                    "rows_per_s": round(rows / dt, 1) if dt else None,
                }
            ),
            flush=True,
        )
    print(
        json.dumps(
            {
                "worker": a.worker,
                "finished": True,
                "seeded": done,
                "skipped": skipped,
                "vector_rows": rows,
                "secs": round(time.time() - t0, 1),
            }
        ),
        flush=True,
    )


if __name__ == "__main__":
    sys.exit(main())
