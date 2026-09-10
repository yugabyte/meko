"""Evaluates the transferwise LRU's health rules against Prometheus every minute and
serves the verdicts as Prometheus metrics on :9100.

lru_check_status{check}   1 ok, 0.5 warn, 0 fail, -1 error (rule could not run)
lru_check_value{check}    the measured value the verdict was made on
lru_overall_status        min over every check, ERR included: a rule that cannot
                          run is a problem with the LRU or with this checker, and
                          silently dropping it would report green on no evidence
Every series carries lru="<LRU_NAME>", because one Prometheus serves every LRU.
amp_endpoint_info{endpoint,pool,cluster,branch,name}  1, from the controller API,
                          refreshed every 5 minutes so dashboards can show names

The rules are app-side first: the point of this LRU is the throughput and latency
the customer's own application sees, so a green verdict needs the driver's own
counters, not just a reachable endpoint.
"""

import json
import os
import threading
import time
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

PROM = os.environ.get("PROM_URL", "http://prometheus:9090")
LRU = os.environ.get("LRU_NAME", "unknown")
# The endpoint id is resolved from the controller by pool name rather than
# configured: it changes whenever the endpoint is recreated, and a value baked
# into a manifest would be both stale and dirty in git.
AMP_POOL = os.environ.get("AMP_POOL", "")
WORKLOAD_DB = os.environ.get("WORKLOAD_DB", "transferwise_lru")
# How many workers the driver was told to run. The floors below are absolute
# because this driver runs unpaced - there is no target to measure against - so
# they start deliberately low and get tightened from the first days of FINDINGS.
WORKERS = float(os.environ.get("WORKERS", "4"))
INTERVAL = int(os.environ.get("INTERVAL_SEC", "60"))
AMP_API = os.environ.get(
    "AMP_API_URL", "http://amp-controller.amp-dev.svc.cluster.local:8080/api/v1"
)
AMP_AUTH = os.environ.get("AMP_AUTH", "admin:admin")

OK, WARN, FAIL, ERR = 1.0, 0.5, 0.0, -1.0


def q(expr):
    url = PROM + "/api/v1/query?" + urllib.parse.urlencode({"query": expr})
    with urllib.request.urlopen(url, timeout=20) as r:
        d = json.load(r)
    res = d["data"]["result"]
    if not res:
        return None
    return float(res[0]["value"][1])


def amp_get(path):
    import base64

    req = urllib.request.Request(AMP_API + path)
    req.add_header("Authorization", "Basic " + base64.b64encode(AMP_AUTH.encode()).decode())
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r)


def endpoint_info():
    """Endpoints of this LRU's pool, as amp_endpoint_info series.

    Scoped to AMP_POOL rather than walking the whole controller: the full walk is
    one request per pool, cluster and branch on a shared dev deployment, so a
    checker restart used to leave the endpoint rules unreadable for minutes.
    """
    out = []
    pools = amp_get("/storage-pools?limit=500").get("storage_pools", [])
    if AMP_POOL:
        pools = [p for p in pools if p["name"] == AMP_POOL]
    for pool in pools:
        for cl in amp_get(f"/storage-pools/{pool['id']}/clusters?limit=500").get("clusters", []):
            branches = amp_get(
                f"/storage-pools/{pool['id']}/clusters/{cl['id']}/branches?limit=500"
            ).get("branches") or [cl["default_branch"]]
            for br in branches:
                eps = amp_get(
                    f"/storage-pools/{pool['id']}/clusters/{cl['id']}/branches/{br['branch_id']}/endpoints"
                ).get("endpoints", [])
                for ep in eps:
                    out.append((ep["id"], pool["name"], cl["name"], br["name"]))
    return out


def band(v, warn, fail, higher_is_bad=True):
    if v is None:
        return ERR
    if higher_is_bad:
        return FAIL if v >= fail else WARN if v >= warn else OK
    return FAIL if v <= fail else WARN if v <= warn else OK


def check_all():
    endpoint = state["endpoint_id"]
    ep = f'endpoint="{endpoint}"' if endpoint else ""
    lru = f'lru="{LRU}"'
    rules = {}

    v = q(f'max(up{{job="ledger",component="metrics",{lru}}})')
    rules["ledger_up"] = (v, OK if v == 1 else FAIL if v is not None else ERR)

    # This driver reports only at the end of a cycle, so every rule below that
    # reads a summary is meaningless until the first one lands. Judging them
    # early would hold the verdict at fail or unreadable for the whole first
    # cycle after every restart.
    summary_at = q(f"max(ledger_last_summary_timestamp_seconds{{{lru}}})") or 0
    if summary_at > 0:
        v = q(f"time() - max(ledger_last_summary_timestamp_seconds{{{lru}}})")
        rules["summary_age_s"] = (v, band(v, 1500, 2700))

        v = q(f'max(ledger_reported_rate{{{lru},kind="statements"}})')
        rules["last_cycle_statement_qps"] = (v, band(v, 200, 20, higher_is_bad=False))

        v = q(f'max(ledger_reported_rate{{{lru},kind="workflow_ops"}})')
        rules["last_cycle_workflow_ops_per_s"] = (v, band(v, 10, 1, higher_is_bad=False))

        for kind, warn, fail in (("write-workflow", 0.1, 0.5), ("read-op", 0.05, 0.25)):
            v = q(f'max(ledger_avg_latency_seconds{{{lru},kind="{kind}"}})')
            if v is not None:
                rules[kind.replace("-", "_") + "_latency_s"] = (v, band(v, warn, fail))

    v = q(f"sum(rate(ledger_errors_total{{{lru}}}[1h]))")
    v = 0.0 if v is None else v
    rules["errors_per_s"] = (v, band(v, 0.001, 0.1))

    # Conflicts are expected on a ledger under SELECT ... FOR UPDATE; a jump
    # means contention, not a broken workload.
    v = q(
        f"sum(rate(ledger_conflicts_total{{{lru}}}[1h])) / "
        f'clamp_min(sum(rate(ledger_operations_total{{{lru},kind="all"}}[1h])), 1e-9)'
    )
    v = 0.0 if v is None else v
    rules["conflict_ratio"] = (v, band(v, 0.02, 0.1))

    if not endpoint:
        # Nothing below can be scoped to one endpoint yet. Say so rather than
        # send Prometheus a selector with a leading comma.
        rules["amp_endpoint_resolved"] = (None, ERR)
        return rules
    rules["amp_endpoint_resolved"] = (1.0, OK)

    v = q(f"max(pg_up{{{ep}}})")
    rules["amp_endpoint_up"] = (v, OK if v == 1 else FAIL if v is not None else ERR)

    v = q(f'sum(rate(pg_stat_database_xact_commit{{{ep},datname="{WORKLOAD_DB}"}}[5m]))')
    rules["endpoint_commits_per_s"] = (v, band(v, 1, 0.1, higher_is_bad=False))

    v = q(f'sum(rate(pg_stat_database_deadlocks{{{ep},datname="{WORKLOAD_DB}"}}[5m]))')
    v = 0.0 if v is None else v
    rules["endpoint_deadlocks_per_s"] = (v, band(v, 0.001, 0.01))

    v = q(
        f'sum(rate(pg_stat_database_blks_hit{{{ep},datname="{WORKLOAD_DB}"}}[5m])) / '
        f'clamp_min(sum(rate(pg_stat_database_blks_hit{{{ep},datname="{WORKLOAD_DB}"}}[5m])) + '
        f'sum(rate(pg_stat_database_blks_read{{{ep},datname="{WORKLOAD_DB}"}}[5m])), 1)'
    )
    rules["buffer_hit_ratio"] = (v, band(v, 0.9, 0.5, higher_is_bad=False))

    # Disk. A workload that dies because something filled up looks identical to
    # one that died for an interesting reason, unless these are on the verdict.
    # The node filesystem behind the driver's emptyDir, not the volume sizeLimit;
    # a node that fills evicts the pod and the workload stops for no visible reason.
    v = q(f"max(ledger_work_disk_used_ratio{{{lru}}})")
    rules["node_disk_used_ratio"] = (v, band(v, 0.85, 0.95))

    # pg_wal against max_wal_size, which is a soft target rather than a hard cap.
    # Sitting at it is normal for a write-heavy endpoint here: measured across the
    # four LRUs it reads 0.05, 0.58, 0.84 and 1.00, so warning at 1.0 would leave
    # the busiest ones permanently yellow and say nothing. A real multiple of the
    # target is the signal.
    v = q(
        f"max(pg_wal_size_bytes{{{ep}}}) / clamp_min(max(pg_settings_max_wal_size_bytes{{{ep}}}), 1)"
    )
    rules["compute_wal_dir_ratio"] = (v, band(v, 1.5, 3.0))

    v = q(f"max(safekeeper_disk_usage_bytes{{{ep}}})")
    rules["safekeeper_disk_bytes"] = (v, band(v, 20e9, 50e9))

    # GC falling behind is how storage grows without the database growing. Not
    # every endpoint publishes this series, and one that never appears is not
    # evidence of a problem, so an absent metric drops the rule rather than
    # holding the whole verdict at unreadable forever.
    v = q(f"max(pageserver_yb_gc_drain_lag_bytes{{{ep}}})")
    if v is not None:
        rules["pageserver_gc_drain_lag_bytes"] = (v, band(v, 1e9, 10e9))
    return rules


state = {"rules": {}, "ts": 0.0, "endpoints": [], "endpoint_id": ""}


def info_loop():
    while True:
        try:
            eps = endpoint_info()
            state["endpoints"] = eps
            if AMP_POOL:
                mine = [ep for ep, pool, _cl, _br in eps if pool == AMP_POOL]
                state["endpoint_id"] = mine[0] if mine else ""
        except Exception as e:
            print("endpoint info failed:", e, flush=True)
        time.sleep(300)


def loop():
    while True:
        try:
            state["rules"] = check_all()
        except Exception as e:  # keep serving the last verdicts
            state["rules"] = {"checker": (0.0, ERR)}
            print("check failed:", e, flush=True)
        state["ts"] = time.time()
        time.sleep(INTERVAL)


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        lines = []
        statuses = []
        for name, (val, st) in sorted(state["rules"].items()):
            lines.append(f'lru_check_status{{lru="{LRU}",check="{name}"}} {st}')
            if val is not None:
                lines.append(f'lru_check_value{{lru="{LRU}",check="{name}"}} {val}')
            statuses.append(st)
        lines.append(f'lru_overall_status{{lru="{LRU}"}} {min(statuses) if statuses else -1}')
        for ep, pool, cl, br in state["endpoints"]:
            lines.append(
                f'amp_endpoint_info{{endpoint="{ep}",pool="{pool}",cluster="{cl}",branch="{br}",name="{pool}/{cl}"}} 1'
            )
        lines.append(f'lru_check_last_run_timestamp_seconds{{lru="{LRU}"}} {state["ts"]}')
        body = ("\n".join(lines) + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


threading.Thread(target=loop, daemon=True).start()
threading.Thread(target=info_loop, daemon=True).start()
HTTPServer(("0.0.0.0", 9100), H).serve_forever()
