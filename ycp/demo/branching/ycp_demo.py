#!/usr/bin/env python3
"""Branching demo on YCP: fork a seeded database once per hypothesis, test each fix
on its own branch in parallel, then drop the branches. Everything goes through the
YBM public API (YCP desired state); nothing touches the amp controller directly.

  ycp_demo.py preflight   check credentials, VPN, and the two cluster settings the demo needs
  ycp_demo.py setup       create the cluster, its main branch and writer, and seed the REJ Outdoors store
  ycp_demo.py run         fork, test, report, drop -- repeatable, plain output
  ycp_demo.py show        the presentation: Claude proposes the fixes, live branch tree
                          [--pause] wait for Enter between acts  [--no-ship] keep main untouched
                          [--no-agent] rehearsed hypotheses instead of asking Claude
  ycp_demo.py scale       the resize presentation: Claude scales main up under load, down when idle
                          [--pause] [--no-agent]
  ycp_demo.py extensions  try pgvector on a copy of main; main never gets it  [--pause]
  ycp_demo.py stage       split screen in Chrome: YCP console left, terminal + store app right
  ycp_demo.py reset       put the slow schema back on main (show does this itself)
  ycp_demo.py teardown    delete the cluster

Configuration is environment variables (defaults in CONFIG). Secrets live in
STATE_DIR, never in this file: the YBM API key (YCP_API_KEY, or STATE_DIR/api_key)
and the generated cloud_admin password (STATE_DIR/pg_password).
"""

import concurrent.futures as cf
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

CONFIG = {
    "YCP_PORTAL": "https://cloudportal.yugabyte.com",
    "YCP_ACCOUNT_ID": "caed7539-bcbd-4d03-8fd0-16cbdaec000f",
    "YCP_ENV": "itest-dev-portal-2",
    "YCP_POOL": "test-pool",
    "DEMO_CLUSTER": "demo-shop",
    "DEMO_CPU": "0.5",
    "KUBE_CONTEXT": "gke_yugabyte_us-west1_itest-dev",
    "KUBE_NAMESPACE": "amp-ycp-portal-1",
    "STATE_DIR": os.path.expanduser("~/.ycp-demo"),
    "YCP_MODE": "byoc",
    "YCP_REGION": "",
    "DEMO_CLIENT_CIDR": "",  # managed: who may reach the endpoints; defaults to this machine's public IP
    # "1": kubectl works and pod IPs are routable from here (VPN), so debug copies
    # need no load balancer.
    "DIRECT_PODS": "1",
}
# DEMO_PROFILE=managed: an AMP Managed account, where Yugabyte runs the environment and
# the account sees only its clusters. No kubectl, so endpoints get a public load
# balancer open to this machine only.
# DEMO_PROFILE=managed-east: the same account in the region gurushant-dev-1 serves. It
# runs on devcloud's infra cluster, reachable through Teleport, with pod IPs on the VPN.
PROFILES = {
    "managed": {
        "YCP_PORTAL": "https://devcloud.yugabyte.com",
        "YCP_ACCOUNT_ID": "202fbb87-94bd-4ef8-913e-e97f136624f7",
        "YCP_MODE": "managed",
        "YCP_REGION": "us-west1",
        "STATE_DIR": os.path.expanduser("~/.ycp-demo/managed"),
        "DIRECT_PODS": "",
    },
    "managed-east": {
        "YCP_PORTAL": "https://devcloud.yugabyte.com",
        "YCP_ACCOUNT_ID": "202fbb87-94bd-4ef8-913e-e97f136624f7",
        "YCP_MODE": "managed",
        "YCP_REGION": "us-east-1",
        "KUBE_CONTEXT": "teleport.devcloud.yugabyte.com-infra",
        "KUBE_NAMESPACE": "amp-gurushant-dev-1",
        "STATE_DIR": os.path.expanduser("~/.ycp-demo/managed"),
        "DIRECT_PODS": "1",
    },
}
cfg = {**CONFIG, **PROFILES.get(os.environ.get("DEMO_PROFILE", ""), {})}
cfg = {k: os.environ.get(k, v) for k, v in cfg.items()}
MANAGED = cfg["YCP_MODE"] == "managed"
DIRECT = cfg["DIRECT_PODS"] == "1"

ACCOUNT_BASE = f"{cfg['YCP_PORTAL']}/api/public/v1/accounts/{cfg['YCP_ACCOUNT_ID']}/ycp"
BASE = ACCOUNT_BASE + ("/clusters" if MANAGED else f"/environments/{cfg['YCP_ENV']}")
# A managed account's storage pool is the account itself.
CLUSTER = f"{cfg['YCP_ACCOUNT_ID'] if MANAGED else cfg['YCP_POOL']}/{cfg['DEMO_CLUSTER']}"
# BYOC: the environment is shared and POST /desired replaces the whole set (the agent
# deletes anything undeclared), so every write is read-modify-write and refuses to
# drop or change anything outside this cluster. That is best effort: POST /desired has
# no If-Match, so a foreign write landing between the last re-read and the POST is
# lost. Managed writes are deltas and need none of that.
OWNED = CLUSTER
CPU = {"cpu": cfg["DEMO_CPU"], "memory": f"{max(1, round(float(cfg['DEMO_CPU']) * 8))}Gi"}
T0 = time.time()

# REJ Outdoors, a (fictional) outdoor-gear co-op's web store. QUERY is its "My Orders" page.
STORE = "REJ Outdoors"
TAGLINE = "an outdoor-gear co-op"
CUSTOMER_ID = 4242  # the customer whose page is slow
CUSTOMER = "Aman Nijhawan"
QUERY = (
    f"SELECT id, placed_at, status, total FROM orders WHERE customer_id = {CUSTOMER_ID} "
    "ORDER BY placed_at DESC LIMIT 20"
)
HYPOTHESES = {
    "dbg-index": (
        "missing index on orders.customer_id",
        "CREATE INDEX ON orders (customer_id, placed_at DESC)",
    ),
    "dbg-stats": ("stale planner statistics", "ANALYZE orders; ANALYZE customers"),
    "dbg-workmem": (
        "work_mem too small, sort spills to disk",
        "ALTER DATABASE postgres SET work_mem = '16MB'",
    ),
}
ORDERS = int(os.environ.get("DEMO_ORDERS", "5000000"))
SEED = """
SET work_mem = '256MB';
DROP TABLE IF EXISTS order_items, orders, products, customers;

CREATE TABLE customers (id bigint PRIMARY KEY, name text, email text, city text, country text, joined_at timestamptz);
CREATE TABLE products (id int PRIMARY KEY, sku text, name text, category text, price numeric(8,2));
-- Planted: no index on orders.customer_id; autovacuum off, so it is never analyzed.
CREATE TABLE orders (id bigint PRIMARY KEY, customer_id bigint, status text, total numeric(10,2),
                     placed_at timestamptz, ship_city text) WITH (autovacuum_enabled = off);
CREATE TABLE order_items (order_id bigint, line int, product_id int, qty int, unit_price numeric(8,2),
                          PRIMARY KEY (order_id, line));

WITH firsts AS (SELECT ARRAY['Maya','Liam','Ava','Noah','Zoe','Ethan','Priya','Lucas','Sofia','Mateo',
                             'Hana','Omar','Chloe','Arjun','Elena','Kai','Nora','Diego','Aisha','Felix',
                             'Ines','Jonas','Mei','Rafael','Leah','Tariq','Freya','Sam','Yuki','Ben'] a),
     lasts  AS (SELECT ARRAY['Chen','Patel','Garcia','Smith','Kim','Nguyen','Silva','Muller','Rossi','Okafor',
                             'Haddad','Johansson','Tanaka','Kowalski','Murphy','Dubois','Novak','Costa',
                             'Larsen','Singh','Lopez','Brown','Ito','Fischer','Ahmed'] a),
     places AS (SELECT ARRAY['Portland|US','Denver|US','Seattle|US','Austin|US','Boston|US','Chicago|US',
                             'Vancouver|CA','Toronto|CA','London|GB','Edinburgh|GB','Berlin|DE','Munich|DE',
                             'Paris|FR','Lyon|FR','Madrid|ES','Lisbon|PT','Oslo|NO','Stockholm|SE',
                             'Zurich|CH','Bangalore|IN','Mumbai|IN','Tokyo|JP','Sydney|AU','Auckland|NZ'] a)
INSERT INTO customers
SELECT g, f.a[1 + g % 30] || ' ' || l.a[1 + (g / 30) % 25],
       lower(f.a[1 + g % 30] || '.' || l.a[1 + (g / 30) % 25]) || g || '@example.com',
       split_part(p.a[1 + (g * 7) % 24], '|', 1), split_part(p.a[1 + (g * 7) % 24], '|', 2),
       now() - ((g * 37) % 1400) * interval '1 day'
FROM generate_series(1, 50000) g, firsts f, lasts l, places p;
UPDATE customers SET name = 'Aman Nijhawan', email = 'aman.nijhawan@example.com', city = 'Seattle', country = 'US',
       joined_at = now() - interval '1400 days' WHERE id = 4242;

WITH cats AS (SELECT ARRAY['Outerwear','Footwear','Packs','Camping','Climbing','Accessories'] c),
     adj  AS (SELECT ARRAY['Summit','Ridge','Alpine','Trail','Canyon','Glacier','Cascade','Basecamp',
                           'Granite','Timberline'] a),
     noun AS (SELECT ARRAY['Shell Jacket|Down Parka|Rain Jacket|Fleece Hoodie',
                           'Hiking Boot|Trail Runner|Approach Shoe|Camp Sandal',
                           'Daypack 22L|Pack 45L|Pack 65L|Hip Pack',
                           '2P Tent|Sleeping Bag|Sleeping Pad|Camp Stove',
                           'Harness|Chalk Bag|Rope 60m|Belay Device',
                           'Headlamp|Water Bottle|Beanie|Trekking Poles'] n)
INSERT INTO products
SELECT g, 'RJ-' || lpad(g::text, 5, '0'),
       a.a[1 + g % 10] || ' ' || split_part(n.n[1 + g % 6], '|', 1 + (g / 6) % 4) || ' ' || (100 + g / 24),
       c.c[1 + g % 6], round((15 + (g * 53) % 430)::numeric + 0.99, 2)
FROM generate_series(1, 2000) g, cats c, adj a, noun n;

INSERT INTO order_items
SELECT g, line, p.id, 1 + (g / 7 + line) % 3, p.price
FROM generate_series(1, {orders}) g CROSS JOIN LATERAL generate_series(1, 1 + g % 3) line
JOIN products p ON p.id = 1 + (g * 31 + g / 1009 + line * 977) % 2000;

INSERT INTO orders
SELECT g, 1 + (g::bigint * 7919) % 50000,
       CASE WHEN g % 50 = 0 THEN 'returned' WHEN g % 37 = 0 THEN 'cancelled'
            WHEN (g * 7) % 730 < 3 THEN 'processing' WHEN (g * 7) % 730 < 10 THEN 'shipped'
            ELSE 'delivered' END,
       t.total, now() - ((g * 7) % 730) * interval '1 day' - (g % 86400) * interval '1 second',
       c.city
FROM generate_series(1, {orders}) g
JOIN (SELECT order_id, sum(qty * unit_price) AS total FROM order_items GROUP BY order_id) t ON t.order_id = g
JOIN customers c ON c.id = 1 + (g::bigint * 7919) % 50000;

-- Planted decoy: tiny work_mem for every session.
ALTER DATABASE postgres SET work_mem = '64kB';
"""
SEED = SEED.replace("{orders}", str(ORDERS))
SEEDED_CHECK = "SELECT to_regclass('order_items') IS NOT NULL"


def say(msg):
    print(f"[+{time.time() - T0:6.1f}s] {msg}", flush=True)


def die(msg):
    sys.exit(f"error: {msg}")


# ---------------------------------------------------------------- secrets


def secret(name, env=None, create=False):
    if env and os.environ.get(env):
        return os.environ[env].strip()
    path = os.path.join(cfg["STATE_DIR"], name)
    if os.path.exists(path):
        return open(path).read().strip()
    if not create:
        die(f"no {name}: set {env} or write it to {path}" if env else f"no {path}; run setup first")
    os.makedirs(cfg["STATE_DIR"], mode=0o700, exist_ok=True)
    value = secrets.token_urlsafe(18)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(value)
    return value


def api_key():
    return secret("api_key", env="YCP_API_KEY")


def pg_password(create=False):
    return secret("pg_password", env="PG_PASSWORD", create=create)


# ---------------------------------------------------------------- YCP API


def call(method, path="", body=None):
    req = urllib.request.Request(
        BASE + path,
        method=method,
        data=None if body is None else json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {api_key()}", "Content-Type": "application/json"},
    )
    # GETs retry transient network errors; a write is never blindly resent.
    for attempt in range(5 if method == "GET" else 1):
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                b = r.read()
                return (json.loads(b) or {}).get("data") if b.strip() else None
        except urllib.error.HTTPError as e:
            die(f"{method} {path or '/'} -> {e.code}: {e.read().decode()[:500]}")
        except (urllib.error.URLError, TimeoutError):
            if method != "GET" or attempt == 4:
                raise
            time.sleep(2 * (attempt + 1))


def _managed_group():
    """The account's resources in this demo's region (the managed API groups them by region)."""
    groups = (call("GET") or {}).get("environments") or []
    return next((g for g in groups if g.get("region") == cfg["YCP_REGION"]), {})


def desired():
    if MANAGED:
        return _managed_group().get("desired") or []
    return call("GET", "/desired")["resources"]


def inventory():
    rows = (_managed_group().get("observed") if MANAGED else call("GET")["resources"]) or []
    return {r["id"]: r for r in rows}


def _managed_write(method, resources):
    call(method, "", {"regions": [{"region": cfg["YCP_REGION"], "resources": resources}]})


def mine(name):
    return name == OWNED or name.startswith(OWNED + "/")


def _foreign(resources):
    return {json.dumps(r, sort_keys=True) for r in resources if not mine(r["name"])}


def write(new_set, before):
    keep = _foreign(before)
    if not keep <= _foreign(new_set):
        die("refusing a write that would drop resources this demo does not own")
    # Re-read right before the POST to narrow the window for a concurrent foreign write.
    if _foreign(desired()) != keep:
        die("another user changed the environment's desired state during this write; re-run")
    call("POST", "/desired", {"resources": new_set})
    if not keep <= _foreign(desired()):
        die(
            "write landed but a foreign resource is missing afterwards -- check the environment now"
        )


def add(resources):
    before = desired()
    have = {(r["kind"], r["name"]) for r in before}
    fresh = [r for r in resources if (r["kind"], r["name"]) not in have]
    if fresh and MANAGED:
        _managed_write("POST", fresh)
    elif fresh:
        write(before + fresh, before)


def update(name, **spec_fields):
    """Change fields of one resource's spec."""
    assert mine(name), name
    before = desired()
    r = next(r for r in before if r["name"] == name)
    changed = dict(r, spec={**r["spec"], **spec_fields})
    if MANAGED:
        _managed_write("PUT", [{"kind": r["kind"], "name": name, "spec": changed["spec"]}])
    else:
        write([changed if x["name"] == name else x for x in before], before)


def remove(*prefixes):
    """Drop every resource under any prefix, in one write."""
    under = lambda n: any(n == p or n.startswith(p + "/") for p in prefixes)
    before = desired()
    if MANAGED:
        # A delta naming only the top of each removed subtree: a branch takes its endpoint with it.
        gone = [r for r in before if under(r["name"])]
        names = {r["name"] for r in gone}
        tops = [r for r in gone if r["name"].rsplit("/", 1)[0] not in names]
        if tops:
            _managed_write(
                "PATCH", [{"kind": r["kind"], "name": r["name"], "delete": True} for r in tops]
            )
        return
    write([r for r in before if not under(r["name"])], before)


def wait(ids, gone=False, timeout=900, poll=1):
    t0 = time.time()
    while time.time() - t0 < timeout:
        inv = inventory()
        if all((i not in inv) if gone else (inv.get(i, {}).get("state") == "ready") for i in ids):
            return inv
        # An in-flight update carries a progress note in `error` too; only a failed state ends the wait.
        bad = [
            f"{i}: {inv[i].get('error')}"
            for i in ids
            if inv.get(i, {}).get("state") in ("error", "rejected", "blocked", "drift")
        ]
        if bad and not gone:
            die("; ".join(bad))
        time.sleep(poll)
    die(f"timed out waiting for {ids}")


# ---------------------------------------------------------------- kubernetes / postgres


def kubectl(*args, timeout=30):
    r = subprocess.run(
        ["kubectl", "--context", cfg["KUBE_CONTEXT"], "-n", cfg["KUBE_NAMESPACE"], *args],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    return r.returncode, r.stdout.strip(), r.stderr.strip()


def pod_addr(endpoint_id):
    """A cluster_ip endpoint's address is in-cluster DNS. Over the VPN the pod IP is
    routable, so dial it directly instead of waiting ~65s for a GCP load balancer."""
    for _ in range(30):
        _, ip, _ = kubectl(
            "get",
            "pods",
            "-l",
            f"app.kubernetes.io/instance=yb-ep-{endpoint_id}",
            "-o",
            "jsonpath={.items[0].status.podIP}",
        )
        if ip:
            return f"{ip}:55433"
        time.sleep(1)
    die(f"no pod IP for endpoint {endpoint_id}")


def client_cidr():
    if not cfg["DEMO_CLIENT_CIDR"]:
        with urllib.request.urlopen("https://api.ipify.org", timeout=10) as r:
            cfg["DEMO_CLIENT_CIDR"] = r.read().decode().strip() + "/32"
    return cfg["DEMO_CLIENT_CIDR"]


def endpoint_addr(row):
    """Where to dial an endpoint: its reported address, or the pod IP for a cluster_ip one."""
    a = row["attrs"]
    if a.get("connectivity_mode") == "cluster_ip" and DIRECT:
        return pod_addr(a["id"])
    return a["address"]


def psql(addr, sql, timeout=900, statement_timeout_s=None):
    """Run sql (a string, or a list of statements sent as separate -c in ONE session,
    so a session-level SET reaches the statements after it)."""
    stmts = [sql] if isinstance(sql, str) else sql
    env = {**os.environ, "PGPASSWORD": pg_password(), "PGCONNECT_TIMEOUT": "15"}
    if statement_timeout_s:
        env["PGOPTIONS"] = f"-c statement_timeout={int(statement_timeout_s * 1000)}"
    # YCP reports an endpoint ready a few seconds before Postgres accepts connections.
    for _ in range(12):
        host, port = addr.rsplit(":", 1)
        r = subprocess.run(
            [
                shutil.which("psql") or "psql",
                f"host={host} port={port} user=cloud_admin dbname=postgres sslmode=disable",
                "-v",
                "ON_ERROR_STOP=1",
                "-AtX",
                *[a for s in stmts for a in ("-c", s)],
            ],
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        if r.returncode == 0:
            return r.stdout.strip()
        if "Connection refused" not in r.stderr:
            raise RuntimeError(r.stderr.strip())
        time.sleep(5)
    raise RuntimeError(f"{addr} kept refusing connections")


def exec_ms(addr, runs=5):
    """Best of several runs, so a cold cache on a fresh branch isn't mistaken for a slower fix."""
    return min(
        json.loads(psql(addr, f"EXPLAIN (ANALYZE, FORMAT JSON) {QUERY}"))[0]["Execution Time"]
        for _ in range(runs)
    )


def endpoint(mode):
    # cluster_ip: no load balancer, reached by pod IP. main keeps an internal LB so a
    # stable address can be shown and shared. Managed: no pod IPs and no VPN into
    # Yugabyte's cluster, so a public LB open to this machine only.
    if not DIRECT:
        return {
            "role": "read_write",
            "password": pg_password(),
            "resources": CPU,
            "connectivity": {"mode": "nlb_external", "allowed_cidrs": [client_cidr()]},
        }
    return {
        "role": "read_write",
        "password": pg_password(),
        "resources": CPU,
        "connectivity": {"mode": mode},
    }


def main_allowed_cidrs():
    """What main's public LB admits (managed profile), or None when it has no allow-list."""
    r = next((r for r in desired() if r["name"] == f"{CLUSTER}/main/rw"), None)
    return ((r or {}).get("spec", {}).get("connectivity") or {}).get("allowed_cidrs")


def follow_client_ip():
    """main's allow-list was this machine's IP at setup; re-point it when the network changed."""
    if DIRECT:
        return
    cidrs = main_allowed_cidrs()
    if cidrs is not None and client_cidr() not in cidrs:
        say(f"main admits {', '.join(cidrs)}; re-opening it to {client_cidr()}")
        update(
            f"{CLUSTER}/main/rw",
            connectivity={"mode": "nlb_external", "allowed_cidrs": [client_cidr()]},
        )
        wait([f"endpoint/{CLUSTER}/main/rw"])


def main_endpoint():
    inv = inventory()
    ep = inv.get(f"endpoint/{CLUSTER}/main/rw")
    if not ep or ep.get("state") != "ready":
        die(f"{CLUSTER} is not set up; run setup")
    follow_client_ip()
    return ep["attrs"]["address"], inv[f"branch/{CLUSTER}/main"]["attrs"]["id"]


# ---------------------------------------------------------------- commands


def _seconds(duration):
    """A Go duration ("2s", "500ms", "1m30s") in seconds; unparseable reads as infinite."""
    units = {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}
    parts = re.findall(r"(\d+(?:\.\d+)?)(h|ms|m|s|us|µs|ns)", duration)
    if not parts or "".join(n + u for n, u in parts) != duration:
        return float("inf")
    return sum(float(n) * units[u] for n, u in parts)


def preflight():
    ok = True

    def check(cond, good, bad, fix=None):
        nonlocal ok
        print(("  ok    " if cond else "  FAIL  ") + (good if cond else bad))
        if not cond:
            ok = False
            if fix:
                print("        fix: " + fix)

    try:
        if MANAGED:
            g = _managed_group()
            check(
                bool(g),
                f"YBM API key works; managed region {cfg['YCP_REGION']} is served by environment "
                f"{g.get('environment_id')}",
                f"the account has nothing in region {cfg['YCP_REGION']} yet (setup will place the cluster there)",
            )
        else:
            env = call("GET")
            check(True, f"YBM API key works; environment {cfg['YCP_ENV']} is {env['state']}", "")
    except SystemExit as e:
        check(
            False,
            "",
            f"YBM API: {e}",
            f"export YCP_API_KEY=... or write it to {cfg['STATE_DIR']}/api_key",
        )
    for tool in ("kubectl", "psql") if DIRECT else ("psql",):
        check(bool(shutil.which(tool)), f"{tool} found", f"{tool} not on PATH")
    if not DIRECT:
        print(f"  --    managed: Yugabyte runs the cluster; endpoints open to {client_cidr()} only")

    rc, svc, err = (
        (1, "", "")
        if not DIRECT
        else kubectl(
            "get",
            "svc",
            "-l",
            "serviceName=yb-tserver-conn,scope=Namespaced",
            "-o",
            'jsonpath={range .items[*]}{.metadata.name} {.spec.sessionAffinity}{"\\n"}{end}',
        )
    )
    if DIRECT:
        check(
            rc == 0,
            "kubectl can read the cluster",
            f"kubectl failed: {err[:200]}",
            "! tsh login (managed-east) or ! gcloud auth login (BYOC)",
        )
    if rc == 0 and not svc:
        print(
            "  --    no foundation tserver Service (a single yugabyted tserver): https://github.com/yugabyte/meko/issues/1493 can't occur"
        )
    if rc == 0:
        for line in svc.splitlines():
            name, aff = line.split()
            check(
                aff == "ClientIP",
                f"{name} has sessionAffinity ClientIP",
                f"{name} has sessionAffinity {aff}: commits hang on SyncRep (https://github.com/yugabyte/meko/issues/1493)",
                f"kubectl --context {cfg['KUBE_CONTEXT']} -n {cfg['KUBE_NAMESPACE']} patch svc {name} "
                '-p \'{"spec":{"sessionAffinity":"ClientIP"}}\'',
            )
        _, interval, _ = kubectl(
            "get",
            "deploy",
            "amp-reconcile-agent",
            "-o",
            'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="RECONCILE_INTERVAL")].value}',
        )
        # A managed environment's agent isn't ours to retune: report its tick, don't fail on it.
        check(
            MANAGED or _seconds(interval or "10s") < 10,
            f"reconcile-agent ticks every {interval or '10s (default)'}",
            f"reconcile-agent ticks every {interval or '10s (default)'}: forks take 2-3 ticks to show",
            f"kubectl --context {cfg['KUBE_CONTEXT']} -n {cfg['KUBE_NAMESPACE']} set env "
            "deploy/amp-reconcile-agent RECONCILE_INTERVAL=2s",
        )

    inv = None
    try:
        inv = inventory()
    except SystemExit:
        pass
    ep = (inv or {}).get(f"endpoint/{CLUSTER}/main/rw")
    if ep and ep.get("state") == "ready" and not DIRECT:
        cidrs = main_allowed_cidrs() or []
        check(
            client_cidr() in cidrs,
            f"main admits this machine ({client_cidr()})",
            f"main admits {', '.join(cidrs) or 'nothing'}, not this machine ({client_cidr()})",
            "any show re-opens it on start, or run setup",
        )
    if ep and ep.get("state") == "ready":
        addr = ep["attrs"]["address"]
        try:
            t = time.time()
            psql(addr, "CREATE TEMP TABLE probe(x int); INSERT INTO probe VALUES (1)", timeout=30)
            check(True, f"{CLUSTER} main at {addr} commits a write in {time.time() - t:.1f}s", "")
        except Exception as e:  # noqa: BLE001 -- report any failure as a check line
            check(
                False,
                "",
                f"write to {addr} failed: {str(e)[:200]}",
                "connect the VPN, or see https://github.com/yugabyte/meko/issues/1493 above"
                if DIRECT
                else "check main's allow-list above",
            )
    else:
        print(f"  --    {CLUSTER} not set up yet (run setup)")
    sys.exit(0 if ok else 1)


def setup():
    pg_password(create=True)
    say(f"declaring {CLUSTER} with a {CPU['cpu']}-core writer")
    add(
        [
            {"kind": "cluster", "name": CLUSTER, "spec": {"pg_version": 18}},
            {"kind": "branch", "name": f"{CLUSTER}/main", "spec": {}},
            {"kind": "endpoint", "name": f"{CLUSTER}/main/rw", "spec": endpoint("nlb_internal")},
        ]
    )
    wait([f"endpoint/{CLUSTER}/main/rw"])
    addr, _ = main_endpoint()
    say(f"main ready at {addr}")
    t = time.time()
    psql(addr, "CREATE TEMP TABLE probe(x int); INSERT INTO probe VALUES (1)", timeout=60)
    say(f"write probe committed in {time.time() - t:.1f}s")
    if psql(addr, SEEDED_CHECK) == "t" and psql(addr, "SELECT count(*) FROM orders") == str(ORDERS):
        say(f"{STORE} already seeded")
    else:
        t = time.time()
        psql(addr, SEED)
        say(
            f"seeded {STORE}: 50k customers, 2k products, {ORDERS:,} orders, ~{2 * ORDERS:,} order lines "
            f"in {time.time() - t:.0f}s"
        )
    say(f"baseline on main: {exec_ms(addr):.1f} ms  ({QUERY})")


def run():
    import show  # for reset_main: start from the slow main, as the show does

    addr, main_id = main_endpoint()
    show.reset_main(addr)
    base = exec_ms(addr)
    say(f"main: {base:.1f} ms for the slow query")
    for b, (why, _) in HYPOTHESES.items():
        say(f"hypothesis {b}: {why}")

    say(f"forking {len(HYPOTHESES)} branches from main in one write")
    t_fork = time.time()
    add(
        [
            r
            for b in HYPOTHESES
            for r in (
                {"kind": "branch", "name": f"{CLUSTER}/{b}", "spec": {"parent_branch_id": main_id}},
                {"kind": "endpoint", "name": f"{CLUSTER}/{b}/rw", "spec": endpoint("cluster_ip")},
            )
        ]
    )
    try:
        wait([f"branch/{CLUSTER}/{b}" for b in HYPOTHESES])
        say(
            f"{len(HYPOTHESES)} branches ready, each with the full dataset, {time.time() - t_fork:.0f}s after the write"
        )
        inv = wait([f"endpoint/{CLUSTER}/{b}/rw" for b in HYPOTHESES])
        say(f"{len(HYPOTHESES)} endpoints ready {time.time() - t_fork:.0f}s after the write")

        def trial(b):
            a = endpoint_addr(inv[f"endpoint/{CLUSTER}/{b}/rw"])
            psql(a, HYPOTHESES[b][1])
            return b, exec_ms(a)

        with cf.ThreadPoolExecutor(len(HYPOTHESES)) as ex:
            results = dict(ex.map(trial, HYPOTHESES))
        print(f"\n  {'branch':13s} {'hypothesis':42s} {'ms':>8s} {'speedup':>8s}")
        print(f"  {'main':13s} {'(baseline)':42s} {base:8.1f}")
        for b, ms in sorted(results.items(), key=lambda kv: kv[1]):
            print(f"  {b:13s} {HYPOTHESES[b][0]:42s} {ms:8.1f} {base / ms:7.1f}x")
        print()
    finally:
        t = time.time()
        remove(*(f"{CLUSTER}/{b}" for b in HYPOTHESES))
        wait([f"branch/{CLUSTER}/{b}" for b in HYPOTHESES], gone=True)
        say(f"debug branches dropped in {time.time() - t:.0f}s; main untouched")


def teardown():
    remove(CLUSTER)
    wait([f"cluster/{CLUSTER}"], gone=True)
    say(f"{CLUSTER} deleted")


if __name__ == "__main__":
    # Ctrl-C must reach the shows' clean-up even when this was started with SIGINT ignored.
    import signal

    signal.signal(signal.SIGINT, signal.default_int_handler)
    cmds = {"preflight": preflight, "setup": setup, "run": run, "teardown": teardown}
    if len(sys.argv) >= 2 and sys.argv[1] in ("show", "reset", "scale", "stage", "extensions"):
        sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
        if sys.argv[1] == "extensions":
            import ext  # try pgvector on a copy; see ext.py

            ext.main(sys.argv[2:])
        elif sys.argv[1] == "stage":
            import stage  # split screen: console left, terminal + store right; see stage.py

            stage.main(sys.argv[2:])
        elif sys.argv[1] == "scale":
            import scale  # the resize presentation; see scale.py

            scale.main(sys.argv[2:])
        else:
            import show  # the branching presentation; see show.py

            show.main(sys.argv[1], sys.argv[2:])
    elif len(sys.argv) != 2 or sys.argv[1] not in cmds:
        sys.exit(__doc__)
    else:
        cmds[sys.argv[1]]()
