"""Tests 2 and 3: latency and recall of the queries Meko runs, at the current seed size.

Runs the production SQL verbatim against a fixed set of probe datapacks:

- KB search: meko-mcp-server src/hybrid_search.py _SEMANTIC_SQL
- memory search: meko-mem0 mem0/vector_stores/pgvector.py PGVector.search, with the
  filters meko-mcp-server's memory_search passes (datapack + user, agent_id=None)
- metadata: the datapack list query DatapackRepo.List used to run, and
  meko-mcp-server _has_datapack_access. List is gone. Production pages or
  streams instead, so --list times the old query and is not a production path.

Recall@k is measured against an exact brute-force search over the datapack's vectors,
recomputed from workload.py rather than read back, so a wrong answer from the database
cannot also corrupt the ground truth.

Writes one JSON object per measurement to --out, then prints a summary.
"""

import argparse
import json
import os
import re
import statistics
import time

import numpy as np
import psycopg
from pgvector.psycopg import register_vector

import workload as w

KB_SQL = """
SELECT id, chunk_text, document_id, metadata_filters,
       embeddings <=> %s::vector AS distance
FROM datapacks.knowledge_bases
WHERE tenant_id = %s::uuid
ORDER BY distance
LIMIT %s
"""

MEM_SQL = """
SELECT id, embeddings <=> %s::vector AS distance, payload
FROM datapacks.meko_mem0_collection
WHERE meko_datapack_id = %s AND meko_user_id = %s
ORDER BY distance
LIMIT %s
"""

# The query DatapackRepo.List ran before it was removed. Production no longer
# executes it. --list keeps it so older probe runs stay comparable.
LIST_SQL = """
SELECT d.datapack_id, d.user_id, d.datapack_name,
       COALESCE(d.created_by_agent_id, ''),
       d.universe_id, d.datapack_ysql_connection_string,
       d.datapack_ysql_username,
       d.created_at, d.updated_at,
       COALESCE(d.datapack_details->>'description', ''),
       COALESCE(CONCAT(u.user_first_name, ' ', u.user_last_name), ''),
       COALESCE(u.user_email, ''),
       'owner'
FROM meko_system.datapacks d
JOIN meko_system.users u ON u.user_id = d.user_id
WHERE d.user_id = $1
UNION ALL
SELECT d.datapack_id, d.user_id, d.datapack_name,
       COALESCE(d.created_by_agent_id, ''),
       d.universe_id, d.datapack_ysql_connection_string,
       d.datapack_ysql_username,
       d.created_at, d.updated_at,
       COALESCE(d.datapack_details->>'description', ''),
       COALESCE(CONCAT(u.user_first_name, ' ', u.user_last_name), ''),
       COALESCE(u.user_email, ''),
       i.grant_role
FROM meko_system.datapacks d
JOIN meko_system.users u ON u.user_id = d.user_id
JOIN meko_system.agent_access aa
     ON aa.datapack_id = d.datapack_id AND aa.user_id = $1
JOIN meko_system.datapack_invites i
     ON i.datapack_id = d.datapack_id
        AND i.recipient_user_id = $1
        AND i.status = 'accepted'
WHERE d.user_id != $1
ORDER BY created_at DESC
""".replace("$1", "%(u)s")

ACCESS_SQL = """
SELECT EXISTS (
  SELECT 1 FROM meko_system.datapacks WHERE user_id = %s::uuid AND datapack_id = %s::uuid
  UNION ALL
  SELECT 1 FROM meko_system.agent_access WHERE user_id = %s::uuid AND datapack_id = %s::uuid
)
"""


def probe_set(seeded: int, n_small: int, n_large: int) -> list[int]:
    """Fixed across seed sizes: the same datapacks answer at 1k and at 1M."""
    small, large = [], []
    for i in range(seeded):
        c = w.size_class(i)
        if c == "small" and len(small) < n_small and w.sizes(i)[1] > 0:
            small.append(i)
        elif c == "large" and len(large) < n_large:
            large.append(i)
        if len(small) >= n_small and len(large) >= n_large:
            break
    return small + large


def exact_topk(i: int, kind: str, q: np.ndarray, k: int) -> list:
    v = w.vectors(i, kind)
    if len(v) == 0:
        return []
    order = np.argsort(1.0 - v @ q)[:k]
    return [w.row_id(i, kind, int(j)) for j in order]


def plan_nodes(cur, sql: str, params) -> str:
    """Summarise the plan: node types, and how many partitions were scanned vs. hit."""
    cur.execute("EXPLAIN (ANALYZE, COSTS OFF, SUMMARY OFF) " + sql, params)
    lines = [r[0].strip() for r in cur.fetchall()]
    scans = [line for line in lines if "Scan" in line]
    hit = [line for line in scans if not re.search(r"rows=0 loops", line)]
    kinds = sorted({re.sub(r" on .*| using .*|->\s*", "", line).strip() for line in scans})
    idx = sorted({m.group(1) for line in scans if (m := re.search(r"using (\S+)", line))})
    return (
        f"scans={len(scans)} with_rows={len(hit)} kinds={kinds} sort={'Sort' in ' '.join(lines)}"
        f" index={idx[0] if len(idx) == 1 else f'{len(idx)} indexes'}"
    )


def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))] if xs else 0.0


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument(
        "--dsn", default=os.environ.get("DSN", "postgresql://yugabyte@yb-tservers:5433/yugabyte")
    )
    ap.add_argument("--seeded", type=int, required=True, help="datapacks [0, seeded) are seeded")
    ap.add_argument("--small", type=int, default=90)
    ap.add_argument("--large", type=int, default=10)
    ap.add_argument("--queries", type=int, default=5)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--ef-search", type=int, nargs="*", default=[0], help="0 = server default")
    ap.add_argument("--list", action="store_true", help="also time the old unpaginated list query")
    ap.add_argument(
        "--list-big",
        action="store_true",
        help="list the 50k big account, not the largest regular one",
    )
    ap.add_argument("--label", default="")
    ap.add_argument("--out", default="probe.jsonl")
    ap.add_argument("--timeout", type=int, default=1800, help="statement timeout, seconds")
    a = ap.parse_args()

    conn = psycopg.connect(a.dsn, autocommit=True)
    register_vector(conn)
    cur = conn.cursor()
    cur.execute(f"SET statement_timeout = '{a.timeout}s'")
    dps = probe_set(a.seeded, a.small, a.large)
    out = open(a.out, "a")
    recs = []

    def emit(r):
        r.update(label=a.label, seeded=a.seeded, ts=time.time())
        out.write(json.dumps(r, default=str) + "\n")
        recs.append(r)

    # hnsw.ef_search only exists once the vector library has loaded in this session.
    cur.execute("SELECT '[1]'::vector <=> '[1]'::vector")
    for ef in a.ef_search:
        ef_ok = True
        if ef:
            try:
                cur.execute(f"SET hnsw.ef_search = {int(ef)}")
            except psycopg.Error as e:
                ef_ok = False
                emit({"test": "ef_search_set", "ef": ef, "error": str(e).strip()})
        for i in dps:
            dp, cls, user = w.datapack_id(i), w.size_class(i), w.user_of(i)
            qs = w.queries(i, a.queries)
            for kind, sql, mk in (
                ("kb", KB_SQL, lambda q: (q, dp, a.k)),
                ("mem", MEM_SQL, lambda q: (q, dp, user, a.k)),
            ):
                n_rows = w.sizes(i)[0 if kind == "mem" else 1]
                if n_rows == 0:
                    continue
                plan = plan_nodes(cur, sql, mk(qs[0]))
                for qn, q in enumerate(qs):
                    t = time.perf_counter()
                    cur.execute(sql, mk(q))
                    got = [r[0] for r in cur.fetchall()]
                    ms = (time.perf_counter() - t) * 1000
                    truth = exact_topk(i, kind, q, a.k)
                    recall = len(set(got) & set(truth)) / len(truth) if truth else None
                    emit(
                        {
                            "test": "search",
                            "kind": kind,
                            "ef": ef if ef_ok else "default",
                            "dp": i,
                            "class": cls,
                            "rows": n_rows,
                            "q": qn,
                            "ms": ms,
                            "returned": len(got),
                            "recall": recall,
                            "plan": plan if qn == 0 else None,
                        }
                    )
        if ef:
            cur.execute("RESET hnsw.ef_search")

    for i in dps[:50]:
        dp, user = w.datapack_id(i), w.user_of(i)
        t = time.perf_counter()
        cur.execute(ACCESS_SQL, (user, dp, user, dp))
        cur.fetchone()
        emit({"test": "access_check", "dp": i, "ms": (time.perf_counter() - t) * 1000})

    if a.list:
        # The largest account's list: the one a paging-free endpoint struggles with.
        t = time.perf_counter()
        acct = w.BIG_ACCOUNT if a.list_big else w.largest_account()
        cur.execute(LIST_SQL, {"u": w.account_user_id(acct)})
        n = len(cur.fetchall())
        emit({"test": "list_datapacks", "rows": n, "ms": (time.perf_counter() - t) * 1000})

    # Summary: one line per (test, kind, class, ef).
    groups = {}
    for r in recs:
        if r["test"] == "search":
            groups.setdefault(("search", r["kind"], r["class"], r["ef"]), []).append(r)
        elif r["test"] in ("access_check", "list_datapacks"):
            groups.setdefault((r["test"], "", "", ""), []).append(r)
    print(f"seeded={a.seeded} label={a.label}")
    for key, rs in sorted(groups.items(), key=str):
        ms = [r["ms"] for r in rs]
        rc = [r["recall"] for r in rs if r.get("recall") is not None]
        line = {
            "group": "/".join(str(x) for x in key if x != ""),
            "n": len(rs),
            "p50_ms": round(pct(ms, 50), 2),
            "p99_ms": round(pct(ms, 99), 2),
        }
        if rc:
            line.update(
                recall_mean=round(statistics.mean(rc), 3),
                recall_min=round(min(rc), 3),
                empty=sum(1 for r in rs if r["returned"] == 0),
            )
        if key[0] == "list_datapacks":
            line["rows"] = rs[0]["rows"]
        print(json.dumps(line))
    plans = {(r["kind"], r["class"]): r["plan"] for r in recs if r.get("plan")}
    for k, p in sorted(plans.items()):
        print(f"plan {k[0]}/{k[1]}: {p}")


if __name__ == "__main__":
    main()
