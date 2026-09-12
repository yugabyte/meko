"""Exports the Postgres internals amp's own exporter does not, on :9096.

Written after a day spent diagnosing a 44% throughput slide on the transferwise
LRU. Every series that mattered had to be fetched by hand with psql, because the
endpoint's exporter publishes pg_stat_database and settings but not the
checkpointer, not per-table size, and not analyze staleness. Those three are what
told the story, so they are metrics now.

    lru_pg_checkpoints_total{kind}              timed vs requested; requested
                                                climbing means max_wal_size is
                                                being hit rather than the timeout
    lru_pg_checkpoint_seconds_total{phase}      write and sync time. Approaching
                                                wall-clock means the checkpointer
                                                never rests.
    lru_pg_checkpoint_buffers_written_total
    lru_pg_table_bytes{table,kind}              heap, index and total per table
    lru_pg_table_rows{table}
    lru_pg_table_mod_since_analyze{table}       planner staleness
    lru_pg_table_autovacuum_total{table}
    lru_pg_table_autoanalyze_total{table}
    lru_pg_database_bytes
    lru_pg_setting_bytes{name}                  max_wal_size, shared_buffers and
                                                friends, so a dashboard can plot
                                                usage against the limit
    lru_pgstats_up                              0 when the queries failed

One copy per LRU, like to_amp_ddl.py: a shared one would have to live outside the
kustomization root, and kustomize refuses that without a load restrictor flag.

psql does the querying because the endpoint speaks Postgres and this image has
the client. Nothing here writes.
"""

import os
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = int(os.environ.get("PGSTATS_PORT", "9096"))
INTERVAL = int(os.environ.get("PGSTATS_INTERVAL_SEC", "60"))
HOST = os.environ.get("PGHOST", "")
PGPORT = os.environ.get("PGPORT", "55433")
USER = os.environ.get("PGUSER", "cloud_admin")
DB = os.environ.get("PGDATABASE", "")
# The driver's own schema, when it does not use public. Empty means every schema
# outside the catalogs.
SCHEMA = os.environ.get("PGSCHEMA", "")

# Sizes are the expensive part of this, so the table list is capped: the point is
# to see which tables dominate and how fast they grow, not to inventory hundreds.
TOP_TABLES = int(os.environ.get("PGSTATS_TOP_TABLES", "20"))

SETTINGS = [
    "max_wal_size",
    "min_wal_size",
    "shared_buffers",
    "effective_cache_size",
    "maintenance_work_mem",
    "work_mem",
    "wal_segment_size",
]

Q_CHECKPOINTER = """
select num_timed, num_requested, write_time, sync_time, buffers_written,
       extract(epoch from (now() - stats_reset))
from pg_stat_checkpointer
"""

Q_TABLES = """
select schemaname || '.' || relname,
       pg_relation_size(relid), pg_indexes_size(relid), pg_total_relation_size(relid),
       n_live_tup, n_mod_since_analyze, autovacuum_count, autoanalyze_count,
       coalesce(extract(epoch from (now() - greatest(last_autoanalyze, last_analyze))), -1)
from pg_stat_user_tables
{where}
order by pg_total_relation_size(relid) desc
limit {limit}
"""

Q_DBSIZE = "select pg_database_size(current_database())"

Q_SETTINGS = """
select name, setting, unit from pg_settings where name in ({names})
"""

state = {"lines": [], "up": 0.0, "ts": 0.0}


def psql(sql):
    """Runs one statement and returns rows as lists of strings."""
    cmd = [
        "psql",
        "-X",
        "-A",
        "-t",
        "-F",
        "\x1f",
        "--no-psqlrc",
        "-h",
        HOST,
        "-p",
        PGPORT,
        "-U",
        USER,
        "-d",
        DB,
        "-v",
        "ON_ERROR_STOP=1",
        "-c",
        sql,
    ]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
    if out.returncode != 0:
        raise RuntimeError(out.stderr.strip()[:300])
    return [ln.split("\x1f") for ln in out.stdout.splitlines() if ln.strip()]


def esc(v):
    return v.replace("\\", "\\\\").replace('"', '\\"')


def as_bytes(setting, unit):
    """pg_settings reports a number plus a unit; normalise the memory ones."""
    mult = {"8kB": 8192, "kB": 1024, "MB": 1024**2, "GB": 1024**3, "B": 1}.get(unit)
    if mult is None:
        return None
    try:
        return float(setting) * mult
    except ValueError:
        return None


def collect():
    out = []
    n_timed, n_req, w_ms, s_ms, buffers, since = psql(Q_CHECKPOINTER)[0]
    out += [
        "# TYPE lru_pg_checkpoints_total counter",
        f'lru_pg_checkpoints_total{{kind="timed"}} {n_timed}',
        f'lru_pg_checkpoints_total{{kind="requested"}} {n_req}',
        "# TYPE lru_pg_checkpoint_seconds_total counter",
        f'lru_pg_checkpoint_seconds_total{{phase="write"}} {float(w_ms) / 1000.0}',
        f'lru_pg_checkpoint_seconds_total{{phase="sync"}} {float(s_ms) / 1000.0}',
        "# TYPE lru_pg_checkpoint_buffers_written_total counter",
        f"lru_pg_checkpoint_buffers_written_total {buffers}",
        "# TYPE lru_pg_stats_age_seconds gauge",
        f"lru_pg_stats_age_seconds {since}",
    ]

    where = (
        f"where schemaname = '{SCHEMA}'"
        if SCHEMA
        else "where schemaname not in ('pg_catalog','information_schema')"
    )
    rows = psql(Q_TABLES.format(where=where, limit=TOP_TABLES))
    out += ["# TYPE lru_pg_table_bytes gauge", "# TYPE lru_pg_table_rows gauge"]
    for name, heap, idx, total, live, modded, av, aa, analyze_age in rows:
        t = esc(name)
        out += [
            f'lru_pg_table_bytes{{table="{t}",kind="heap"}} {heap}',
            f'lru_pg_table_bytes{{table="{t}",kind="index"}} {idx}',
            f'lru_pg_table_bytes{{table="{t}",kind="total"}} {total}',
            f'lru_pg_table_rows{{table="{t}"}} {live}',
            f'lru_pg_table_mod_since_analyze{{table="{t}"}} {modded}',
            f'lru_pg_table_autovacuum_total{{table="{t}"}} {av}',
            f'lru_pg_table_autoanalyze_total{{table="{t}"}} {aa}',
            f'lru_pg_table_analyze_age_seconds{{table="{t}"}} {analyze_age}',
        ]

    out += ["# TYPE lru_pg_database_bytes gauge", f"lru_pg_database_bytes {psql(Q_DBSIZE)[0][0]}"]

    names = ",".join(f"'{s}'" for s in SETTINGS)
    out.append("# TYPE lru_pg_setting_bytes gauge")
    for name, setting, unit in psql(Q_SETTINGS.format(names=names)):
        v = as_bytes(setting, unit)
        if v is not None:
            out.append(f'lru_pg_setting_bytes{{name="{esc(name)}"}} {v}')
    return out


def loop():
    while True:
        try:
            state["lines"] = collect()
            state["up"] = 1.0
        except Exception as e:
            # Keep serving the last good sample; a blank scrape would look like
            # the workload stopped rather than like this collector tripping.
            state["up"] = 0.0
            print("collect failed:", e, flush=True)
        state["ts"] = time.time()
        time.sleep(INTERVAL)


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = (
            "\n".join(
                state["lines"]
                + [
                    "# TYPE lru_pgstats_up gauge",
                    f"lru_pgstats_up {state['up']}",
                    "# TYPE lru_pgstats_last_run_timestamp_seconds gauge",
                    f"lru_pgstats_last_run_timestamp_seconds {state['ts']}",
                ]
            )
            + "\n"
        )
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *a):
        pass


threading.Thread(target=loop, daemon=True).start()
HTTPServer(("0.0.0.0", PORT), H).serve_forever()
