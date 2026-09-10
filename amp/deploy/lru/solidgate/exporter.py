"""Turns the card driver's CSV output into Prometheus metrics on :9095.

The driver is a verbatim copy of the customer-facing workload and has no metrics
endpoint, so run.sh tees its output to a file and this reads the tail. It already
computes per-interval rates, so the throughput and latency series here are gauges
taken straight from its own numbers rather than a rate() over a counter we built.
That also means no five-minute warm-up before a dashboard reads true.

The rows it emits, header included:

  timestamp_utc,event,concurrency,stmts_per_txn,total_qps,txns_per_s,read_qps,
  insert_qps,update_qps,read_avg_ms,insert_avg_ms,update_avg_ms,txn_avg_ms,
  read_errs,insert_errs,update_errs,txn_errs,open,in_use,idle

Error columns are per-interval deltas, so they are summed into counters that
survive a cycle boundary. The `final` row at shutdown is a partial interval and
is counted for errors but never used for a rate.
"""

import os
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = os.environ.get("CARD_LOG", "/work/card.log")
PORT = int(os.environ.get("EXPORTER_PORT", "9095"))
# The driver's scratch volume. statvfs on an emptyDir sees the node filesystem
# behind it, not the volume's sizeLimit, so this is really "disk on the node we
# landed on" - which is the eviction risk worth watching.
WORK_DIR = os.environ.get("WORK_DIR", "/work")

COLS = [
    "timestamp_utc",
    "event",
    "concurrency",
    "stmts_per_txn",
    "total_qps",
    "txns_per_s",
    "read_qps",
    "insert_qps",
    "update_qps",
    "read_avg_ms",
    "insert_avg_ms",
    "update_avg_ms",
    "txn_avg_ms",
    "read_errs",
    "insert_errs",
    "update_errs",
    "txn_errs",
    "open",
    "in_use",
    "idle",
]
RATES = {
    "total_qps": ("card_qps", "total"),
    "read_qps": ("card_qps", "read"),
    "insert_qps": ("card_qps", "insert"),
    "update_qps": ("card_qps", "update"),
}
LATENCIES = {
    "read_avg_ms": "read",
    "insert_avg_ms": "insert",
    "update_avg_ms": "update",
    "txn_avg_ms": "txn",
}
ERRORS = {
    "read_errs": "read",
    "insert_errs": "insert",
    "update_errs": "update",
    "txn_errs": "txn",
}

state = {
    "rates": {},
    "latency_ms": {},
    "pool": {},
    "errors": {k: 0 for k in ERRORS.values()},
    "txns_per_s": 0.0,
    "concurrency": 0.0,
    "cycles": 0,
    "last_report": 0.0,
}


def handle(line):
    line = line.strip()
    if line.startswith("=== cycle "):
        state["cycles"] += 1
        return
    parts = line.split(",")
    if len(parts) != len(COLS) or parts[1] not in ("interval", "final"):
        return
    row = dict(zip(COLS, parts, strict=True))

    def num(key):
        try:
            return float(row[key])
        except (TypeError, ValueError):
            return 0.0

    # A final row is a partial interval: its rates are meaningless (they read
    # near zero) but its error deltas are real.
    if row["event"] == "interval":
        for col, (_metric, label) in RATES.items():
            state["rates"][label] = num(col)
        for col, label in LATENCIES.items():
            state["latency_ms"][label] = num(col)
        for col in ("open", "in_use", "idle"):
            state["pool"][col] = num(col)
        state["txns_per_s"] = num("txns_per_s")
        state["concurrency"] = num("concurrency")
        state["last_report"] = time.time()
    for col, label in ERRORS.items():
        state["errors"][label] += int(num(col))


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
        "# TYPE card_work_disk_bytes gauge",
        f'card_work_disk_bytes{{state="total"}} {total}',
        f'card_work_disk_bytes{{state="free"}} {free}',
        f'card_work_disk_bytes{{state="used"}} {total - free}',
        "# TYPE card_work_disk_used_ratio gauge",
        f"card_work_disk_used_ratio {(total - free) / total if total else 0}",
    ]


def render():
    out = ["# TYPE card_qps gauge"]
    for label, v in sorted(state["rates"].items()):
        out.append(f'card_qps{{kind="{label}"}} {v}')
    out += [
        "# TYPE card_txns_per_s gauge",
        f"card_txns_per_s {state['txns_per_s']}",
        "# TYPE card_concurrency gauge",
        f"card_concurrency {state['concurrency']}",
        "# TYPE card_cycles_total counter",
        f"card_cycles_total {state['cycles']}",
        "# TYPE card_last_report_timestamp_seconds gauge",
        f"card_last_report_timestamp_seconds {state['last_report']}",
    ]
    out.append("# TYPE card_avg_latency_seconds gauge")
    for label, ms in sorted(state["latency_ms"].items()):
        out.append(f'card_avg_latency_seconds{{kind="{label}"}} {ms / 1000.0}')
    out.append("# TYPE card_errors_total counter")
    for label, n in sorted(state["errors"].items()):
        out.append(f'card_errors_total{{kind="{label}"}} {n}')
    out.append("# TYPE card_pool_connections gauge")
    for label, v in sorted(state["pool"].items()):
        out.append(f'card_pool_connections{{state="{label}"}} {v}')
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
