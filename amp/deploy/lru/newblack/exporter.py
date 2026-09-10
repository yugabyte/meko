"""Turns the bomber load driver's stdout into Prometheus metrics on :9095.

The driver is a verbatim copy of the customer-facing load test and has no metrics
endpoint of its own, so run.sh tees its output to a file and this reads the tail.
Parsing the log instead of patching the driver keeps the vendored copy identical
to the one QA runs elsewhere, which is the point of using it at all.

Two line shapes carry everything:

  progress elapsed=00:04:35 ro=12043 rw=8123 errors=0 tx_retries=4 total_tps=901.31
  RO mean latency ms: 3.412            (once per cycle, in the summary block)

Counters are cumulative across cycles: the driver restarts its own counts every
cycle, so a cycle boundary is added to a running base rather than resetting the
series, which would make rate() spike.
"""

import os
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = os.environ.get("BOMBER_LOG", "/work/bomber.log")
PORT = int(os.environ.get("EXPORTER_PORT", "9095"))
# The driver's scratch volume: the NuGet cache, the build output and this log all
# live here. Nothing else reports it, and a full one kills the driver with an
# error that looks nothing like "out of disk". Note statvfs on an emptyDir sees
# the node filesystem behind it, not the volume's sizeLimit, so this is really
# "disk on the node we landed on" - which is the eviction risk worth watching.
WORK_DIR = os.environ.get("WORK_DIR", "/work")

# The driver formats decimals with the container's locale, which is a comma on a
# host with a European locale and a dot under DOTNET_SYSTEM_GLOBALIZATION_INVARIANT.
# run.sh forces the latter; accept both anyway rather than silently export zeros.
PROGRESS = re.compile(
    r"progress elapsed=(\S+) ro=(\d+) rw=(\d+) errors=(\d+) tx_retries=(\d+) total_tps=([\d.,]+)"
)
MEAN_LAT = re.compile(r"(RO|RW) mean latency ms: ([\d.,]+)")


def num(s):
    return (
        float(s.replace(",", "."))
        if s.count(",") == 1 and "." not in s
        else float(s.replace(",", ""))
    )


# The summary block closes a cycle with the authoritative totals: the last
# progress line is up to PROGRESS_SECONDS short of them.
SUMMARY_OPS = re.compile(r"^(RO|RW) success ops: (\d+)")
SUMMARY_ERRORS = re.compile(r"^Total errors: (\d+)")
SUMMARY_RETRIES = re.compile(r"^TXBUNDLE retries: (\d+)")
QID_HEADER = re.compile(r"^QID (execution|error) counts:")
QID_ROW = re.compile(r"^\s{2}(Q\d+): (\d+)$")
SUMMARY = re.compile(r"^=== Npgsql Load Test Summary ===")

state = {
    # base + cycle keeps the counters monotonic across driver restarts
    "base": {"ro": 0, "rw": 0, "errors": 0, "retries": 0},
    "cycle": {"ro": 0, "rw": 0, "errors": 0, "retries": 0},
    "qid_ops": {},
    "qid_errors": {},
    "latency_ms": {},
    "tps": 0.0,
    "elapsed_s": 0.0,
    "cycles": 0,
    "last_progress": 0.0,
}


def hhmmss(s):
    parts = [num(p) for p in s.split(":")]
    while len(parts) < 3:
        parts.insert(0, 0.0)
    return parts[0] * 3600 + parts[1] * 60 + parts[2]


def roll_cycle():
    state.pop("_saw_summary", None)
    for k in state["base"]:
        state["base"][k] += state["cycle"][k]
        state["cycle"][k] = 0
    state["cycles"] += 1


def handle(line):
    m = PROGRESS.search(line)
    if m:
        prev = state["cycle"]["ro"] + state["cycle"]["rw"]
        cur = int(m.group(2)) + int(m.group(3))
        # First progress line after a summary is the next cycle. Without a
        # summary a count drop is the only evidence, which is the crash case.
        if state.pop("_saw_summary", False) or cur < prev:
            roll_cycle()
        state["cycle"] = {
            "ro": int(m.group(2)),
            "rw": int(m.group(3)),
            "errors": int(m.group(4)),
            "retries": int(m.group(5)),
        }
        state["tps"] = num(m.group(6))
        state["elapsed_s"] = hhmmss(m.group(1))
        state["last_progress"] = time.time()
        return
    m = MEAN_LAT.search(line)
    if m:
        state["latency_ms"][m.group(1).lower()] = num(m.group(2))
        return
    if QID_HEADER.match(line):
        state["_qid_target"] = "qid_errors" if "error" in line else "qid_ops"
        return
    m = QID_ROW.match(line.rstrip("\n"))
    if m and state.get("_qid_target"):
        state[state["_qid_target"]][m.group(1)] = int(m.group(2))
        return
    m = SUMMARY_OPS.match(line)
    if m:
        state["cycle"][m.group(1).lower()] = int(m.group(2))
        state["_saw_summary"] = True
        return
    m = SUMMARY_ERRORS.match(line)
    if m:
        state["cycle"]["errors"] = int(m.group(1))
        return
    m = SUMMARY_RETRIES.match(line)
    if m:
        state["cycle"]["retries"] = int(m.group(1))
        return
    if SUMMARY.match(line):
        state["_qid_target"] = None


def tail():
    pos = 0
    while True:
        try:
            with open(LOG) as f:
                f.seek(0, os.SEEK_END)
                if f.tell() < pos:  # run.sh truncated the log for a new cycle
                    roll_cycle()
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
        "# TYPE bomber_work_disk_bytes gauge",
        f'bomber_work_disk_bytes{{state="total"}} {total}',
        f'bomber_work_disk_bytes{{state="free"}} {free}',
        f'bomber_work_disk_bytes{{state="used"}} {total - free}',
        "# TYPE bomber_work_disk_used_ratio gauge",
        f"bomber_work_disk_used_ratio {(total - free) / total if total else 0}",
    ]


def render():
    b, c = state["base"], state["cycle"]
    out = [
        "# TYPE bomber_ops_total counter",
        f'bomber_ops_total{{lane="ro"}} {b["ro"] + c["ro"]}',
        f'bomber_ops_total{{lane="rw"}} {b["rw"] + c["rw"]}',
        "# TYPE bomber_errors_total counter",
        f"bomber_errors_total {b['errors'] + c['errors']}",
        "# TYPE bomber_tx_retries_total counter",
        f"bomber_tx_retries_total {b['retries'] + c['retries']}",
        "# TYPE bomber_tps gauge",
        f"bomber_tps {state['tps']}",
        "# TYPE bomber_cycle_elapsed_seconds gauge",
        f"bomber_cycle_elapsed_seconds {state['elapsed_s']}",
        "# TYPE bomber_cycles_total counter",
        f"bomber_cycles_total {state['cycles']}",
        "# TYPE bomber_last_progress_timestamp_seconds gauge",
        f"bomber_last_progress_timestamp_seconds {state['last_progress']}",
    ]
    out.append("# TYPE bomber_mean_latency_seconds gauge")
    for lane, ms in sorted(state["latency_ms"].items()):
        out.append(f'bomber_mean_latency_seconds{{lane="{lane}"}} {ms / 1000.0}')
    out.append("# TYPE bomber_qid_ops_total counter")
    for qid, n in sorted(state["qid_ops"].items()):
        out.append(f'bomber_qid_ops_total{{qid="{qid}"}} {n}')
    out.append("# TYPE bomber_qid_errors_total counter")
    for qid, n in sorted(state["qid_errors"].items()):
        out.append(f'bomber_qid_errors_total{{qid="{qid}"}} {n}')
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
