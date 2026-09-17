"""Evaluates the LRU health rules against Prometheus every minute and serves the
verdicts as Prometheus metrics on :9100, so the dashboard shows status and the
history of every check is kept with the rest of the data.

lru_check_status{check}   1 ok, 0.5 warn, 0 fail, -1 error (rule could not run)
lru_check_value{check}    the measured value the verdict was made on
lru_overall_status        min over every check, ERR included: a rule that cannot
                          run is a problem with the LRU or with this checker, and
                          silently dropping it would report green on no evidence
Every series carries lru="<LRU_NAME>", because one Prometheus serves every LRU.
amp_endpoint_info{endpoint,pool,cluster,branch,name}  1, from the controller API,
                          refreshed every 5 minutes so dashboards can show names
"""

import json
import os
import threading
import time
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

PROM = os.environ.get("PROM_URL", "http://prometheus:9090")
TEMPORAL_METRICS = os.environ.get("TEMPORAL_METRICS", "http://temporal-frontend:8000/metrics")
LRU = os.environ.get("LRU_NAME", "unknown")
# The endpoint id is resolved from the controller by pool name rather than
# configured: it changes whenever the endpoint is recreated, and a value baked
# into a manifest would be both stale and dirty in git.
AMP_POOL = os.environ.get("AMP_POOL", "")
INTERVAL = int(os.environ.get("INTERVAL_SEC", "60"))
AMP_API = os.environ.get(
    "AMP_API_URL", "http://amp-controller.amp-dev.svc.cluster.local:8080/api/v1"
)
AMP_AUTH = os.environ.get("AMP_AUTH", "admin:admin")

# The one throughput and the one latency series that stand for this workload,
# named once so the drift rules below cannot drift from them.
THROUGHPUT = 'sum(rate(workflow_success{lru="small_temporal"}[10m]))'
LATENCY = 'histogram_quantile(0.99, sum by (le) (rate(persistence_latency_bucket{lru="small_temporal"}[10m])))'

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
    req = urllib.request.Request(AMP_API + path)
    import base64

    req.add_header("Authorization", "Basic " + base64.b64encode(AMP_AUTH.encode()).decode())
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.load(r)


def endpoint_info():
    out = []
    for pool in amp_get("/storage-pools?limit=500").get("storage_pools", []):
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


def http_ok(url):
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return 1.0 if r.status == 200 else 0.0
    except Exception:
        return 0.0


def advisory(status):
    """Caps a verdict at WARN.

    Drift and capacity rules answer "someone should look at this", not "the
    workload is broken". They cannot tell a regression from a change somebody
    made on purpose: solidgate's throughput reads 0.20 of yesterday because its
    concurrency was deliberately cut, which is not a failure by any reading. A
    FAIL should stay reserved for the workload not doing its job, so the portal
    badge keeps meaning that.
    """
    return WARN if status == FAIL else status


def liveness(selector, grace):
    """OK if the target answered any scrape in the window.

    A single missed scrape is not an outage: omes closes its metrics ports for
    ~32s between 2-hour scenario blocks, read as 13 FAILs over 48h.
    """
    v = q(f"max(max_over_time({selector}[{grace}]))")
    return v, OK if v == 1 else FAIL if v is not None else ERR


def check_all():
    endpoint = state["endpoint_id"]
    lru = f'lru="{LRU}"'
    ep = f'endpoint="{endpoint}"' if endpoint else ""
    rules = {}

    v = http_ok(TEMPORAL_METRICS)
    rules["temporal_reachable"] = (v, OK if v else FAIL)

    rules["omes_up"] = liveness(f'up{{job="omes",component="metrics",lru="{LRU}"}}', "5m")

    v = q(f'sum(rate(workflow_success{{workflowType="kitchenSink",lru="{LRU}"}}[5m]))')
    rules["workflows_completing_per_s"] = (v, band(v, 0.2, 0.01, higher_is_bad=False))

    v = q(
        "sum(rate(workflow_failed[5m])) / clamp_min(sum(rate(workflow_success[5m])) + sum(rate(workflow_failed[5m])), 1e-9)"
    )
    v = 0.0 if v is None else v
    rules["workflow_failure_ratio"] = (v, band(v, 0.01, 0.05))

    v = q(
        'sum(rate(service_error_with_type{error_type!~"context_Canceled|serviceerror_Canceled|serviceerror_WorkflowExecutionAlreadyStarted|serviceerror_NotFound"}[5m])) / clamp_min(sum(rate(service_requests[5m])), 1e-9)'
    )
    v = 0.0 if v is None else v
    rules["service_error_ratio"] = (v, band(v, 0.005, 0.02))

    v = q(
        'sum(rate(persistence_error_with_type{error_type!="persistence_CurrentWorkflowConditionFailedError"}[5m])) / clamp_min(sum(rate(persistence_requests[5m])), 1e-9)'
    )
    v = 0.0 if v is None else v
    rules["persistence_error_ratio"] = (v, band(v, 0.001, 0.01))

    v = q(
        f'histogram_quantile(0.99, sum by (le) (rate(persistence_latency_bucket{{lru="{LRU}"}}[5m])))'
    )
    rules["persistence_p99_s"] = (v, band(v, 0.05, 0.25))

    # --- drift against this LRU's own past -----------------------------------
    # The rules above are absolute floors, which a slow decay clears all the way
    # down. A young LRU has no rule rather than a failing one. See FINDINGS.
    prev = q(f"last_over_time(({THROUGHPUT})[10m:1m] offset 24h)")
    now_v = q(THROUGHPUT)
    if prev and prev > 0 and now_v is not None:
        rules["throughput_vs_24h_ago"] = (
            now_v / prev,
            advisory(band(now_v / prev, 0.8, 0.6, higher_is_bad=False)),
        )

    prev = q(f"last_over_time(({LATENCY})[10m:1m] offset 24h)")
    now_v = q(LATENCY)
    if prev and prev > 0 and now_v is not None:
        rules["latency_vs_24h_ago"] = (now_v / prev, advisory(band(now_v / prev, 1.5, 3.0)))

    # Against a week's own baseline, not a fixed offset: 24h cannot see a slower
    # decay, and an offset can land inside a past incident. Latency only - the
    # same rule on throughput flags every deliberate rate cut.
    #
    # Its own try/except because this is the one expensive query here: a 7d
    # subquery over small_temporal's 1,720-series histogram already takes 5s and
    # grows with retention. A failure has to cost this rule, not the verdict.
    try:
        base = q(f"quantile_over_time(0.1, ({LATENCY})[7d:1h])")
        now_v = q(LATENCY)
        if base and base > 0 and now_v is not None:
            rules["latency_vs_7d_baseline"] = (
                now_v / base,
                advisory(band(now_v / base, 1.5, 3.0)),
            )
    except Exception as e:
        print("latency_vs_7d_baseline skipped:", e, flush=True)

    if not endpoint:
        # Nothing below can be scoped to one endpoint yet. Say so rather than
        # send Prometheus a selector with a leading comma.
        rules["amp_endpoint_resolved"] = (None, ERR)
        return rules
    rules["amp_endpoint_resolved"] = (1.0, OK)

    rules["amp_endpoint_up"] = liveness(f"pg_up{{{ep}}}", "3m")

    v = q(f'sum(rate(pg_stat_database_xact_commit{{{ep},datname=~"temporal_lru.*"}}[5m]))')
    rules["endpoint_commits_per_s"] = (v, band(v, 1, 0.1, higher_is_bad=False))

    v = q(f'sum(pg_stat_database_deadlocks{{{ep},datname=~"temporal_lru.*"}})')
    rules["endpoint_deadlocks_total"] = (v, OK if v is not None else ERR)

    v = q(f'sum(rate(omes_execute_histogram_count{{lru="{LRU}"}}[5m]))')
    # one throughput_stress iteration is a whole kitchen-sink batch, ~0.07/s at 4 concurrent
    rules["omes_iterations_per_s"] = (v, band(v, 0.02, 0.0, higher_is_bad=False))

    # --- how long until the data outgrows the endpoint -----------------------
    # Not "is it big" but "when does it stop fitting". small_temporal reached 99%
    # of its file cache with every rule green and no warning anywhere; the answer
    # wanted to be a number of days, not a number of bytes.
    size = q(f"max(pageserver_current_logical_size{{{ep}}})")
    cache = q(f"max(pg_settings_yb_amp_max_file_cache_size_bytes{{{ep}}})")
    if size and cache and cache > 0:
        rules["logical_size_vs_file_cache"] = (size / cache, advisory(band(size / cache, 0.8, 1.0)))
        growth = q(f"deriv(max(pageserver_current_logical_size{{{ep}}})[6h:5m])")
        if growth and growth > 0:
            days = max(0.0, cache - size) / (growth * 86400)
            rules["days_until_file_cache_full"] = (
                days,
                advisory(band(days, 7, 2, higher_is_bad=False)),
            )

    # A checkpointer that never rests is the mechanism behind a write-side slide:
    # requested checkpoints mean max_wal_size is the trigger, not the timeout.
    v = q(
        f'sum(rate(lru_pg_checkpoints_total{{{lru},kind="requested"}}[1h])) / '
        f"clamp_min(sum(rate(lru_pg_checkpoints_total{{{lru}}}[1h])), 1e-9)"
    )
    if v is not None:
        rules["checkpoints_forced_ratio"] = (v, advisory(band(v, 0.5, 0.9)))

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
