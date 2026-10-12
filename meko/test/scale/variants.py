"""Compare rewrites of the memory and KB search SQL on the same datapacks and queries.

Answers: what does filtering by the tenant keys (datapack, user, agent) buy, and does an
exact scan of the tenant's own rows beat the HNSW index for small and large datapacks?

A MATERIALIZED CTE stops the planner from pushing ORDER BY <=> LIMIT into the vector
index, so the tenant's rows are fetched by primary-key prefix and sorted exactly.
"""

import argparse
import json
import os
import statistics
import time

import psycopg
from pgvector.psycopg import register_vector

import probe
import workload as w

MEM = "datapacks.meko_mem0_collection"
EXACT = """
WITH t AS MATERIALIZED (
  SELECT id, embeddings FROM {table} WHERE {where}
)
SELECT id, embeddings <=> %s::vector AS distance FROM t ORDER BY distance LIMIT %s
"""
ANN = (
    "SELECT id, embeddings <=> %s::vector AS distance FROM {table} WHERE {where}"
    " ORDER BY distance LIMIT %s"
)

VARIANTS = {
    # name: (kind, template, where, params-builder)
    "mem_current_dp_user": (
        "mem",
        ANN,
        "meko_datapack_id = %s AND meko_user_id = %s",
        lambda dp, u, q, k: (q, dp, u, k),
    ),
    "mem_ann_dp_user_agent": (
        "mem",
        ANN,
        "meko_datapack_id = %s AND meko_user_id = %s AND meko_agent_id = %s",
        lambda dp, u, q, k: (q, dp, u, w.AGENT_ID, k),
    ),
    "mem_exact_dp_user_agent": (
        "mem",
        EXACT,
        "meko_datapack_id = %s AND meko_user_id = %s AND meko_agent_id = %s",
        lambda dp, u, q, k: (dp, u, w.AGENT_ID, q, k),
    ),
    "mem_exact_dp_user": (
        "mem",
        EXACT,
        "meko_datapack_id = %s AND meko_user_id = %s",
        lambda dp, u, q, k: (dp, u, q, k),
    ),
    "kb_current_ann": ("kb", ANN, "tenant_id = %s", lambda dp, u, q, k: (q, dp, k)),
    "kb_exact": ("kb", EXACT, "tenant_id = %s", lambda dp, u, q, k: (dp, q, k)),
}
# The proposed schema itself (data migration 000006): 128 partitions keyed by datapack alone.
MEM_V2 = "datapacks.meko_mem0_memories"
VARIANTS["v2_mem_ann_dp_user"] = (
    "mem_v2",
    ANN,
    "meko_datapack_id = %s AND meko_user_id = %s",
    lambda dp, u, q, k: (q, dp, u, k),
)
VARIANTS["v2_mem_exact_dp_user"] = (
    "mem_v2",
    EXACT,
    "meko_datapack_id = %s AND meko_user_id = %s",
    lambda dp, u, q, k: (dp, u, q, k),
)
VARIANTS["v2_mem_ann_dp_user_ef400"] = VARIANTS["v2_mem_ann_dp_user"] + (400,)

# ef_search sweeps of the single-partition HNSW paths. The default is 40.
for _ef in (100, 400, 1000):
    VARIANTS[f"mem_ann_dp_user_agent_ef{_ef}"] = VARIANTS["mem_ann_dp_user_agent"] + (_ef,)
    VARIANTS[f"kb_current_ann_ef{_ef}"] = VARIANTS["kb_current_ann"] + (_ef,)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--dsn", default=os.environ.get("DSN", "postgresql://yugabyte@yb-tservers:5433/yugabyte")
    )
    ap.add_argument("--seeded", type=int, required=True)
    ap.add_argument("--small", type=int, default=10)
    ap.add_argument("--large", type=int, default=5)
    ap.add_argument("--queries", type=int, default=5)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--only", nargs="*", default=list(VARIANTS))
    ap.add_argument("--timeout", type=int, default=1800, help="statement timeout, seconds")
    a = ap.parse_args()

    conn = psycopg.connect(a.dsn, autocommit=True)
    register_vector(conn)
    cur = conn.cursor()
    cur.execute(f"SET statement_timeout = '{a.timeout}s'")
    dps = probe.probe_set(a.seeded, a.small, a.large)

    # hnsw.ef_search only exists once the vector library has loaded in this session.
    cur.execute("SELECT '[1]'::vector <=> '[1]'::vector")
    for name in a.only:
        kind, tmpl, where, mk, *ef = VARIANTS[name]
        cur.execute(f"SET hnsw.ef_search = {ef[0]}" if ef else "RESET hnsw.ef_search")
        table = {"mem": MEM, "mem_v2": MEM_V2}.get(kind, "datapacks.knowledge_bases")
        kind = "kb" if kind == "kb" else "mem"
        sql = tmpl.format(table=table, where=where)
        by_cls = {}
        plans = {}
        for i in dps:
            if w.sizes(i)[0 if kind == "mem" else 1] == 0:
                continue
            dp, cls, u = w.datapack_id(i), w.size_class(i), w.user_of(i)
            qs = w.queries(i, a.queries)
            if cls not in plans:
                plans[cls] = probe.plan_nodes(cur, sql, mk(dp, u, qs[0], a.k))
            for q in qs:
                t = time.perf_counter()
                cur.execute(sql, mk(dp, u, q, a.k))
                got = [r[0] for r in cur.fetchall()]
                ms = (time.perf_counter() - t) * 1000
                truth = probe.exact_topk(i, kind, q, a.k)
                rec = len(set(got) & set(truth)) / len(truth)
                by_cls.setdefault(cls, []).append((ms, rec))
        for cls, rs in sorted(by_cls.items()):
            ms = [r[0] for r in rs]
            print(
                json.dumps(
                    {
                        "variant": name,
                        "class": cls,
                        "n": len(rs),
                        "seeded": a.seeded,
                        "p50_ms": round(probe.pct(ms, 50), 1),
                        "p99_ms": round(probe.pct(ms, 99), 1),
                        "recall_mean": round(statistics.mean(r[1] for r in rs), 3),
                        "recall_min": round(min(r[1] for r in rs), 3),
                        "plan": plans[cls],
                    }
                ),
                flush=True,
            )


if __name__ == "__main__":
    main()
