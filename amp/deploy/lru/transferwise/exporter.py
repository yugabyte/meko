"""Turns the ledger driver's summary into Prometheus metrics on :9095.

Unlike the card driver, this one prints nothing while it runs: no interval rows,
no progress lines, only an end-of-run summary. That is why run.sh gives it a
short cycle, and why the app-side series here only move once a cycle. Continuous
liveness comes from the endpoint's own Postgres metrics, which is what the
checker leans on; see checker/check.py.

The summary lines this reads:

  elapsed: 15m0s
  successful operations: 31924 (read-ops=22759 write-workflows=9165)
  non-tx read ops: 7961
  statement totals: 649174 (read=314979 write=334195)
  conflicts observed (40001/40P01): 0
  errors: 0
  workflow op/s: 709.42
  statement qps: 14426.05
  READ-OP latency: avg=5.25ms p50~5.00ms p95~10.00ms p99~10.00ms max~68.09ms

Totals are cumulative across cycles: the driver counts from zero every cycle, so
each summary is added to a running base rather than replacing it, which would
make rate() see a reset.
"""

import os
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = os.environ.get("LEDGER_LOG", "/work/ledger.log")
PORT = int(os.environ.get("EXPORTER_PORT", "9095"))
# statvfs on an emptyDir sees the node filesystem behind it, not the volume's
# sizeLimit, so this is "disk on the node we landed on" - the eviction risk.
WORK_DIR = os.environ.get("WORK_DIR", "/work")

OPS = re.compile(r"^successful operations: (\d+) \(read-ops=(\d+) write-workflows=(\d+)\)")
NONTX = re.compile(r"^non-tx read ops: (\d+)")
STMTS = re.compile(r"^statement totals: (\d+) \(read=(\d+) write=(\d+)\)")
CONFLICTS = re.compile(r"^conflicts observed \([^)]*\): (\d+)")
RETRIES = re.compile(r"^retries applied \([^)]*\): (\d+)")
TRANSIENT = re.compile(r"^transient retries applied \([^)]*\): (\d+)")
POOL_RESETS = re.compile(r"^pool resets triggered: (\d+)")
ERRORS = re.compile(r"^errors: (\d+)")
OPS_RATE = re.compile(r"^workflow op/s: ([\d.]+)")
QPS = re.compile(r"^statement qps: ([\d.]+)")
LATENCY = re.compile(r"^([A-Z-]+) latency: avg=([\d.]+)ms.*?p99~([\d.]+)ms")
ELAPSED = re.compile(r"^elapsed: (\S+)")

# The counters a summary contributes, keyed by the metric label they land under.
COUNTER_LINES = {
    "conflicts": CONFLICTS,
    "retries": RETRIES,
    "transient_retries": TRANSIENT,
    "pool_resets": POOL_RESETS,
    "errors": ERRORS,
}

state = {
    "base": {},
    "cycle": {},
    "rates": {},
    "latency_ms": {},
    "p99_ms": {},
    "cycles": 0,
    "last_summary": 0.0,
    "cycle_started": 0.0,
}


def add(key, value):
    state["cycle"][key] = value


def roll():
    for k, v in state["cycle"].items():
        state["base"][k] = state["base"].get(k, 0) + v
    state["cycle"] = {}


def handle(line):
    line = line.strip()
    if line.startswith("=== cycle "):
        # A cycle line means the previous summary, if any, is now final.
        roll()
        state["cycles"] += 1
        state["cycle_started"] = time.time()
        return
    m = OPS.match(line)
    if m:
        add("ops_total", int(m.group(1)))
        add("read_ops", int(m.group(2)))
        add("write_workflows", int(m.group(3)))
        state["last_summary"] = time.time()
        return
    m = NONTX.match(line)
    if m:
        add("non_tx_reads", int(m.group(1)))
        return
    m = STMTS.match(line)
    if m:
        add("statements_total", int(m.group(1)))
        add("statements_read", int(m.group(2)))
        add("statements_write", int(m.group(3)))
        return
    for label, pat in COUNTER_LINES.items():
        m = pat.match(line)
        if m:
            add(label, int(m.group(1)))
            return
    m = OPS_RATE.match(line)
    if m:
        state["rates"]["workflow_ops"] = float(m.group(1))
        return
    m = QPS.match(line)
    if m:
        state["rates"]["statements"] = float(m.group(1))
        return
    m = LATENCY.match(line)
    if m:
        kind = m.group(1).lower()
        state["latency_ms"][kind] = float(m.group(2))
        state["p99_ms"][kind] = float(m.group(3))
        return
    m = ELAPSED.match(line)
    if m:
        state["rates"]["last_cycle_elapsed_s"] = go_duration(m.group(1))


def go_duration(s):
    total = 0.0
    for value, unit in re.findall(r"([\d.]+)(h|m|s|ms|us|ns)", s):
        total += (
            float(value) * {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "ns": 1e-9}[unit]
        )
    return total


def tail():
    pos = 0
    while True:
        try:
            with open(LOG) as f:
                f.seek(0, os.SEEK_END)
                if f.tell() < pos:  # run.sh truncated it for a new cycle
                    pos = 0
                f.seek(pos)
                for line in f:
                    handle(line)
                pos = f.tell()
        except FileNotFoundError:
            pass
        except Exception as e:
            print("tail failed:", e, flush=True)
        time.sleep(2)


def work_disk():
    try:
        st = os.statvfs(WORK_DIR)
    except OSError as e:
        print("statvfs failed:", e, flush=True)
        return []
    total = st.f_blocks * st.f_frsize
    free = st.f_bavail * st.f_frsize
    return [
        "# TYPE ledger_work_disk_bytes gauge",
        f'ledger_work_disk_bytes{{state="total"}} {total}',
        f'ledger_work_disk_bytes{{state="free"}} {free}',
        f'ledger_work_disk_bytes{{state="used"}} {total - free}',
        "# TYPE ledger_work_disk_used_ratio gauge",
        f"ledger_work_disk_used_ratio {(total - free) / total if total else 0}",
    ]


def total(key):
    return state["base"].get(key, 0) + state["cycle"].get(key, 0)


def render():
    out = [
        "# TYPE ledger_operations_total counter",
        f'ledger_operations_total{{kind="all"}} {total("ops_total")}',
        f'ledger_operations_total{{kind="read"}} {total("read_ops")}',
        f'ledger_operations_total{{kind="write_workflow"}} {total("write_workflows")}',
        f'ledger_operations_total{{kind="non_tx_read"}} {total("non_tx_reads")}',
        "# TYPE ledger_statements_total counter",
        f'ledger_statements_total{{kind="all"}} {total("statements_total")}',
        f'ledger_statements_total{{kind="read"}} {total("statements_read")}',
        f'ledger_statements_total{{kind="write"}} {total("statements_write")}',
        "# TYPE ledger_conflicts_total counter",
        f"ledger_conflicts_total {total('conflicts')}",
        "# TYPE ledger_retries_total counter",
        f'ledger_retries_total{{kind="conflict"}} {total("retries")}',
        f'ledger_retries_total{{kind="transient"}} {total("transient_retries")}',
        "# TYPE ledger_pool_resets_total counter",
        f"ledger_pool_resets_total {total('pool_resets')}",
        "# TYPE ledger_errors_total counter",
        f"ledger_errors_total {total('errors')}",
        "# TYPE ledger_cycles_total counter",
        f"ledger_cycles_total {state['cycles']}",
        "# TYPE ledger_last_summary_timestamp_seconds gauge",
        f"ledger_last_summary_timestamp_seconds {state['last_summary']}",
        "# TYPE ledger_cycle_started_timestamp_seconds gauge",
        f"ledger_cycle_started_timestamp_seconds {state['cycle_started']}",
    ]
    out.append("# TYPE ledger_reported_rate gauge")
    for label, v in sorted(state["rates"].items()):
        out.append(f'ledger_reported_rate{{kind="{label}"}} {v}')
    out.append("# TYPE ledger_avg_latency_seconds gauge")
    for kind, ms in sorted(state["latency_ms"].items()):
        out.append(f'ledger_avg_latency_seconds{{kind="{kind}"}} {ms / 1000.0}')
    out.append("# TYPE ledger_p99_latency_seconds gauge")
    for kind, ms in sorted(state["p99_ms"].items()):
        out.append(f'ledger_p99_latency_seconds{{kind="{kind}"}} {ms / 1000.0}')
    out += work_disk()
    return ("\n".join(out) + "\n").encode()


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = render()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


threading.Thread(target=tail, daemon=True).start()
HTTPServer(("0.0.0.0", PORT), H).serve_forever()
