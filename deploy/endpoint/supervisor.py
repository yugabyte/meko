#!/usr/bin/env python3
"""yb-light single-container endpoint supervisor.

Brings up, in one container, the services that make up a yb-light endpoint
and supervises them (basic monitor + restart-with-backoff). Runs as PID 1
under tini.

Services (in start order):
  1. endpoint_storage   - LFC scratch blobs           (YugabyteDB-backed)
  2. safekeeper         - WAL                          (YugabyteDB-backed)
  3. yb-pageserver      - page service                 (YugabyteDB-backed)
  4. compute_ctl        - supervises Postgres v17 + the `neon` extension

The storage_controller is NOT here: it is a shared, cluster-level control
plane that lives in the AMP layer. This container assumes the `yb_storage`
schema already exists (AMP/SC provisions it via diesel migrations). For
standalone testing set DEV_PROVISION_SCHEMA=true to have the supervisor
create the database + apply the migrations itself.

Config comes from environment variables (delivered via a ConfigMap + Secret
in the Helm chart). See the chart's values.yaml / README for the contract.
"""

import hashlib
import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

# ---------------------------------------------------------------------------
# Config (environment contract — matches the Helm ConfigMap keys)
# ---------------------------------------------------------------------------

RUN_DIR = Path(os.environ.get("RUN_DIR", "/data"))
BIN_DIR = Path(os.environ.get("BIN_DIR", "/usr/local/bin"))
PG_DISTRIB_DIR = os.environ.get("PG_DISTRIB_DIR", "/usr/local")


def env(name, default=None):
    v = os.environ.get(name)
    return v if v not in (None, "") else default


def env_int(name, default):
    v = env(name)
    return int(v) if v is not None else default


def log(msg):
    print(f"[supervisor] {msg}", flush=True)


# YugabyteDB connection (the external durable backend).
YB_HOST_PORTS = env("YB_HOST_PORTS", "127.0.0.1:5433")
YB_USER = env("YB_USER", "yugabyte")
YB_DBNAME = env("YB_DBNAME", "yb_storage")
YB_SSLMODE = env("YB_SSLMODE")
YB_SSLROOTCERT = env("YB_SSLROOTCERT")
# Name of the env var holding the YB password (matches YbConnConfig.password_env).
YB_PASSWORD_ENV = env("YB_PASSWORD_ENV")
if YB_PASSWORD_ENV is None and env("YB_PASSWORD") is not None:
    YB_PASSWORD_ENV = "YB_PASSWORD"

# Endpoint / compute.
PG_VERSION = env("ENDPOINT_PG_VERSION", "17")
COMPUTE_PORT = env_int("COMPUTE_PORT", 55433)
COMPUTE_CTL_HTTP_PORT = env_int("COMPUTE_CTL_HTTP_PORT", 3080)
COMPUTE_PASSWORD = env("COMPUTE_PASSWORD", "cloud_admin")
SHARED_PRELOAD_LIBRARIES = env("SHARED_PRELOAD_LIBRARIES", "neon")
TENANT_ID = env("TENANT_ID")
TIMELINE_ID = env("TIMELINE_ID")
GENERATION = env_int("GENERATION", 1)

# Internal service ports.
PAGESERVER_PG_PORT = env_int("PAGESERVER_PG_PORT", 64000)
PAGESERVER_HTTP_PORT = env_int("PAGESERVER_HTTP_PORT", 9898)
PAGESERVER_GRPC_PORT = env_int("PAGESERVER_GRPC_PORT", 51051)
SAFEKEEPER_PG_PORT = env_int("SAFEKEEPER_PG_PORT", 5454)
SAFEKEEPER_HTTP_PORT = env_int("SAFEKEEPER_HTTP_PORT", 7676)
ENDPOINT_STORAGE_HTTP_PORT = env_int("ENDPOINT_STORAGE_HTTP_PORT", 9993)

LISTEN_HOST = "127.0.0.1"
NODE_ID = 1  # one pageserver + one safekeeper per endpoint pod

# The pageserver requires `control_plane_api` to be set ("pageservers cannot
# run in isolation"), but this fork's pageserver no longer calls back to it at
# runtime (controller_upcall_client was removed), so a placeholder is fine in
# the SC-free pod. Point it at the AMP storage_controller if you have one.
CONTROL_PLANE_API = env("CONTROL_PLANE_API", "http://localhost:6666")

# Supervisor behaviour.
RESTART_BACKOFF = env_int("SUPERVISOR_RESTART_BACKOFF_SECONDS", 2)
MAX_RESTARTS = env_int("SUPERVISOR_MAX_RESTARTS", 5)
RESTART_WINDOW = env_int("SUPERVISOR_RESTART_WINDOW_SECONDS", 60)

DEV_PROVISION_SCHEMA = env("DEV_PROVISION_SCHEMA", "false").lower() in ("1", "true", "yes")
MIGRATIONS_DIR = Path(env("YB_LIGHT_MIGRATIONS_DIR", "/usr/local/share/yb-light/migrations"))

PG_BIN = Path(PG_DISTRIB_DIR) / f"v{PG_VERSION}" / "bin"
PG_LIB = Path(PG_DISTRIB_DIR) / f"v{PG_VERSION}" / "lib"

# The Postgres-family binaries (postgres, psql, initdb) and the compute's
# walredo child link against pg_install/v{N}/lib (libpq.so.5, ...). Put it on
# the loader path for this process and everything it spawns.
_existing_ld = os.environ.get("LD_LIBRARY_PATH", "")
os.environ["LD_LIBRARY_PATH"] = str(PG_LIB) + (f":{_existing_ld}" if _existing_ld else "")


# ---------------------------------------------------------------------------
# YB connection rendering
# ---------------------------------------------------------------------------

def yb_conn_dict():
    d = {"host_ports": YB_HOST_PORTS, "user": YB_USER, "dbname": YB_DBNAME}
    if YB_PASSWORD_ENV:
        d["password_env"] = YB_PASSWORD_ENV
    if YB_SSLMODE:
        d["sslmode"] = YB_SSLMODE
    if YB_SSLROOTCERT:
        d["sslrootcert"] = YB_SSLROOTCERT
    return d


def _toml_quote(s):
    return '"' + str(s).replace("\\", "\\\\").replace('"', '\\"') + '"'


def yb_conn_inline_toml():
    """Inline TOML table form, e.g. { host_ports = "...", user = "..." }."""
    parts = [f"{k} = {_toml_quote(v)}" for k, v in yb_conn_dict().items()]
    return "{ " + ", ".join(parts) + " }"


def yb_conn_typed_json():
    """JSON object with the YbStore discriminator, for endpoint_storage."""
    d = {"type": "YbStore"}
    d.update(yb_conn_dict())
    return d


# ---------------------------------------------------------------------------
# Small HTTP + wait helpers (stdlib only)
# ---------------------------------------------------------------------------

def http_request(method, url, body=None, timeout=10):
    data = None
    headers = {}
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
        return resp.status, (json.loads(raw) if raw else None)


def tcp_open(host, port):
    try:
        with socket.create_connection((host, port), timeout=1):
            return True
    except OSError:
        return False


def wait_for(desc, predicate, timeout, interval=0.5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return True
        except Exception:
            pass
        time.sleep(interval)
    raise TimeoutError(f"timed out waiting for {desc} ({timeout}s)")


def psql(dbname, sql=None, file=None, ignore_errors=False):
    """Run psql against the external YugabyteDB using the bundled client."""
    cmd = [
        str(PG_BIN / "psql"),
        "-h", YB_HOST_PORTS.split(",")[0].split(":")[0],
        "-p", YB_HOST_PORTS.split(",")[0].split(":")[1] if ":" in YB_HOST_PORTS.split(",")[0] else "5433",
        "-U", YB_USER,
        "-d", dbname,
        "-v", "ON_ERROR_STOP=1",
        "--no-psqlrc",
    ]
    if sql is not None:
        cmd += ["-c", sql]
    if file is not None:
        cmd += ["-f", str(file)]
    penv = dict(os.environ)
    if YB_PASSWORD_ENV and os.environ.get(YB_PASSWORD_ENV):
        penv["PGPASSWORD"] = os.environ[YB_PASSWORD_ENV]
    res = subprocess.run(cmd, env=penv, capture_output=True, text=True)
    if res.returncode != 0 and not ignore_errors:
        raise RuntimeError(f"psql failed: {res.stderr.strip()}")
    return res


# ---------------------------------------------------------------------------
# Config file rendering
# ---------------------------------------------------------------------------
# NOTE: pageserver.toml's `timeline_init` Bootstrap serialization and the
# exact compute spec shape are verified against `yb_light_local`-generated
# configs during bring-up testing.

def write_pageserver_config(datadir, bootstrap):
    datadir.mkdir(parents=True, exist_ok=True)
    (datadir / "identity.toml").write_text(f"id={NODE_ID}\n")

    lines = [
        f"pg_distrib_dir='{PG_DISTRIB_DIR}'",
        f"listen_pg_addr='{LISTEN_HOST}:{PAGESERVER_PG_PORT}'",
        f"listen_http_addr='{LISTEN_HOST}:{PAGESERVER_HTTP_PORT}'",
        f"listen_grpc_addr='{LISTEN_HOST}:{PAGESERVER_GRPC_PORT}'",
        f"safekeeper_pg_addr='{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}'",
        "availability_zone='local'",
        f"control_plane_api='{CONTROL_PLANE_API}'",
        f"yb_storage_conn={yb_conn_inline_toml()}",
        f"tenant_id='{TENANT_ID}'",
        f"timeline_id='{TIMELINE_ID}'",
        f"generation={GENERATION}",
    ]
    if bootstrap:
        # Tell the pageserver to run initdb + import for a fresh timeline.
        # TimelineInitConfig is internally tagged (`tag = "mode"`,
        # rename_all = snake_case); PgMajorVersion serializes as its integer.
        lines += ["", "[timeline_init]", 'mode = "bootstrap"', f"pg_version = {int(PG_VERSION)}"]
    (datadir / "pageserver.toml").write_text("\n".join(lines) + "\n")


def write_safekeeper_config(datadir):
    datadir.mkdir(parents=True, exist_ok=True)
    lines = [
        f"id = {NODE_ID}",
        f"tenant_id = {_toml_quote(TENANT_ID)}",
        f"timeline_id = {_toml_quote(TIMELINE_ID)}",
        f"listen_pg = {_toml_quote(f'{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}')}",
        f"listen_http = {_toml_quote(f'{LISTEN_HOST}:{SAFEKEEPER_HTTP_PORT}')}",
        f"availability_zone = {_toml_quote(f'sk-{NODE_ID}')}",
        f"yb_storage_conn = {_toml_quote(yb_conn_inline_toml())}",
    ]
    (datadir / "safekeeper.toml").write_text("\n".join(lines) + "\n")


def write_endpoint_storage_config(datadir, pemfile):
    datadir.mkdir(parents=True, exist_ok=True)
    cfg = {
        "listen": f"{LISTEN_HOST}:{ENDPOINT_STORAGE_HTTP_PORT}",
        "pemfile": str(pemfile),
        "yb_storage_conn": yb_conn_typed_json(),
    }
    path = datadir / "endpoint_storage.json"
    path.write_text(json.dumps(cfg))
    return path


def generate_jwt_keypair(keydir):
    """EdDSA keypair: public PEM used by endpoint_storage to verify tokens."""
    keydir.mkdir(parents=True, exist_ok=True)
    priv = keydir / "auth_private_key.pem"
    pub = keydir / "auth_public_key.pem"
    if not pub.exists():
        subprocess.run(["openssl", "genpkey", "-algorithm", "ed25519", "-out", str(priv)], check=True)
        subprocess.run(["openssl", "pkey", "-in", str(priv), "-pubout", "-out", str(pub)], check=True)
    return pub


def md5_role_password(password, user):
    return "md5" + hashlib.md5((password + user).encode()).hexdigest()


def render_compute_spec(specfile):
    settings = [
        {"name": "fsync", "value": "off", "vartype": "bool"},
        {"name": "wal_level", "value": "logical", "vartype": "enum"},
        {"name": "wal_log_hints", "value": "on", "vartype": "bool"},
        {"name": "port", "value": str(COMPUTE_PORT), "vartype": "integer"},
        {"name": "listen_addresses", "value": "0.0.0.0", "vartype": "string"},
        {"name": "max_connections", "value": "100", "vartype": "integer"},
        {"name": "shared_buffers", "value": "128MB", "vartype": "string"},
        {"name": "max_wal_senders", "value": "10", "vartype": "integer"},
        {"name": "max_replication_slots", "value": "10", "vartype": "integer"},
        {"name": "password_encryption", "value": "md5", "vartype": "enum"},
        {"name": "restart_after_crash", "value": "off", "vartype": "bool"},
        {"name": "synchronous_standby_names", "value": "walproposer", "vartype": "string"},
        {"name": "shared_preload_libraries", "value": SHARED_PRELOAD_LIBRARIES, "vartype": "string"},
        {"name": "neon.safekeepers", "value": f"{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}", "vartype": "string"},
        {"name": "neon.timeline_id", "value": TIMELINE_ID, "vartype": "string"},
        {"name": "neon.tenant_id", "value": TENANT_ID, "vartype": "string"},
        {"name": "neon.pageserver_connstring",
         "value": f"host={LISTEN_HOST} port={PAGESERVER_PG_PORT}", "vartype": "string"},
    ]
    for k, v in json.loads(env("COMPUTE_EXTRA_SETTINGS_JSON", "{}")).items():
        settings.append({"name": k, "value": str(v), "vartype": "string"})

    spec = {
        "format_version": 1.0,
        "timestamp": "2024-01-01T00:00:00.000Z",
        "operation_uuid": "00000000-0000-0000-0000-000000000000",
        "suspend_timeout_seconds": -1,
        "cluster": {
            "cluster_id": "yb-light-endpoint",
            "name": "yb-light-endpoint",
            "state": "running",
            "roles": [{
                "name": "cloud_admin",
                "encrypted_password": md5_role_password(COMPUTE_PASSWORD, "cloud_admin"),
                "options": None,
            }],
            "databases": [],
            "settings": settings,
        },
        "delta_operations": [],
        "skip_pg_catalog_updates": True,
        "mode": "Primary",
        "tenant_id": TENANT_ID,
        "timeline_id": TIMELINE_ID,
        "pageserver_connstring": f"host={LISTEN_HOST} port={PAGESERVER_PG_PORT}",
        "safekeeper_connstrings": [f"{LISTEN_HOST}:{SAFEKEEPER_PG_PORT}"],
        # Membership generation of the safekeeper timeline (created with
        # mconf generation=1). Emitted as the `g#1:` prefix on neon.safekeepers.
        "safekeepers_generation": 1,
    }
    config = {
        "spec": spec,
        "compute_ctl_config": {"jwks": {"keys": []}},
    }
    specfile.write_text(json.dumps(config))
    return specfile


# ---------------------------------------------------------------------------
# Service supervision
# ---------------------------------------------------------------------------

class Service:
    def __init__(self, name, argv, cwd, health=None, extra_env=None):
        self.name = name
        self.argv = [str(a) for a in argv]
        self.cwd = str(cwd)
        self.health = health  # callable -> bool, or None
        self.extra_env = extra_env or {}
        self.proc = None
        self.restarts = []  # monotonic timestamps

    def start(self):
        penv = dict(os.environ)
        penv.update(self.extra_env)
        log(f"starting {self.name}: {' '.join(self.argv)}")
        self.proc = subprocess.Popen(self.argv, cwd=self.cwd, env=penv)

    def is_alive(self):
        return self.proc is not None and self.proc.poll() is None

    def wait_healthy(self, timeout):
        if self.health is None:
            return
        wait_for(f"{self.name} healthy", self.health, timeout)
        log(f"{self.name} is healthy")

    def record_restart(self):
        now = time.monotonic()
        self.restarts = [t for t in self.restarts if now - t < RESTART_WINDOW]
        self.restarts.append(now)
        return len(self.restarts)


class Supervisor:
    def __init__(self):
        self.services = []
        self.shutting_down = threading.Event()

    def add(self, svc):
        self.services.append(svc)

    def shutdown(self, signum=None, frame=None):
        if self.shutting_down.is_set():
            return
        log(f"shutdown (signal {signum}); stopping services")
        self.shutting_down.set()
        for svc in reversed(self.services):
            if svc.is_alive():
                log(f"  SIGTERM {svc.name}")
                try:
                    svc.proc.terminate()
                except Exception:
                    pass
        deadline = time.monotonic() + 30
        for svc in reversed(self.services):
            if svc.proc is None:
                continue
            remaining = max(0, deadline - time.monotonic())
            try:
                svc.proc.wait(timeout=remaining)
            except Exception:
                log(f"  SIGKILL {svc.name}")
                try:
                    svc.proc.kill()
                except Exception:
                    pass

    def monitor(self):
        """Block, restarting any crashed service. Exit non-zero if a service
        exceeds its restart budget so the pod is restarted by k8s/tini."""
        while not self.shutting_down.is_set():
            for svc in self.services:
                if self.shutting_down.is_set():
                    break
                if svc.is_alive():
                    continue
                rc = svc.proc.returncode if svc.proc else None
                log(f"{svc.name} exited (rc={rc})")
                count = svc.record_restart()
                if count > MAX_RESTARTS:
                    log(f"{svc.name} exceeded {MAX_RESTARTS} restarts in "
                        f"{RESTART_WINDOW}s; failing the pod")
                    self.shutdown()
                    return 1
                log(f"restarting {svc.name} in {RESTART_BACKOFF}s "
                    f"({count}/{MAX_RESTARTS} within {RESTART_WINDOW}s)")
                time.sleep(RESTART_BACKOFF)
                if not self.shutting_down.is_set():
                    svc.start()
            time.sleep(1)
        return 0


# ---------------------------------------------------------------------------
# Bring-up
# ---------------------------------------------------------------------------

def provision_schema():
    log("DEV_PROVISION_SCHEMA: ensuring database + schema in YugabyteDB")
    psql("yugabyte", sql=f'CREATE DATABASE "{YB_DBNAME}"', ignore_errors=True)
    # Already provisioned? (pageserver.pages is created by the migration.)
    check = psql(
        YB_DBNAME,
        sql="SELECT 1 FROM information_schema.tables "
            "WHERE table_schema='pageserver' AND table_name='pages'",
        ignore_errors=True,
    )
    if check.returncode == 0 and "1 row" in (check.stdout or ""):
        log("schema already present; skipping migrations")
        return
    for mig in sorted(MIGRATIONS_DIR.glob("*/up.sql")):
        log(f"applying migration {mig.parent.name}")
        psql(YB_DBNAME, file=mig)


def main():
    global TENANT_ID, TIMELINE_ID

    sup = Supervisor()
    signal.signal(signal.SIGTERM, sup.shutdown)
    signal.signal(signal.SIGINT, sup.shutdown)

    bootstrap = TENANT_ID is None
    if bootstrap:
        TENANT_ID = os.urandom(16).hex()
        TIMELINE_ID = os.urandom(16).hex()
        log(f"no TENANT_ID supplied; bootstrapping fresh tenant {TENANT_ID} "
            f"timeline {TIMELINE_ID}")
    else:
        if TIMELINE_ID is None:
            raise SystemExit("TENANT_ID set but TIMELINE_ID missing")
        log(f"serving tenant {TENANT_ID} timeline {TIMELINE_ID}")

    if DEV_PROVISION_SCHEMA:
        provision_schema()

    es_dir = RUN_DIR / "endpoint_storage"
    sk_dir = RUN_DIR / "safekeeper"
    ps_dir = RUN_DIR / "pageserver"
    compute_pgdata = RUN_DIR / "pgdata"
    keydir = RUN_DIR / "keys"

    # 1. endpoint_storage
    pem = generate_jwt_keypair(keydir)
    es_cfg = write_endpoint_storage_config(es_dir, pem)
    es = Service(
        "endpoint_storage",
        [BIN_DIR / "endpoint_storage", es_cfg],
        cwd=es_dir,
        extra_env={"RUST_LOG": "info"},
        health=lambda: tcp_open(LISTEN_HOST, ENDPOINT_STORAGE_HTTP_PORT),
    )

    # 2. safekeeper
    write_safekeeper_config(sk_dir)
    sk = Service(
        "safekeeper",
        [BIN_DIR / "safekeeper", "-D", sk_dir],
        cwd=sk_dir,
        health=lambda: http_request("GET", f"http://{LISTEN_HOST}:{SAFEKEEPER_HTTP_PORT}/v1/status")[0] == 200,
    )

    # 3. pageserver (runs initdb for the Bootstrap recipe on first start)
    write_pageserver_config(ps_dir, bootstrap)
    ps = Service(
        "yb-pageserver",
        [BIN_DIR / "yb-pageserver", "-D", ps_dir],
        cwd=ps_dir,
        health=lambda: http_request("GET", f"http://{LISTEN_HOST}:{PAGESERVER_HTTP_PORT}/v1/status")[0] == 200,
    )

    for svc in (es, sk, ps):
        sup.add(svc)
        svc.start()
        svc.wait_healthy(timeout=120)

    # Wait for the pageserver's timeline to come up Active and read its start LSN.
    ps_tl = f"http://{LISTEN_HOST}:{PAGESERVER_HTTP_PORT}/v1/tenant/{TENANT_ID}/timeline/{TIMELINE_ID}"

    def timeline_active():
        st, body = http_request("GET", ps_tl)
        return st == 200 and body and body.get("state") == "Active"

    wait_for("pageserver timeline Active", timeline_active, timeout=180)
    _, tl = http_request("GET", ps_tl)
    start_lsn = tl.get("last_record_lsn", "0/0")
    log(f"pageserver timeline Active at LSN {start_lsn}")

    # Create the timeline on the safekeeper (single-member membership).
    sk_create = {
        "tenant_id": TENANT_ID,
        "timeline_id": TIMELINE_ID,
        "mconf": {
            "generation": 1,
            "members": [{"id": NODE_ID, "host": LISTEN_HOST, "pg_port": SAFEKEEPER_PG_PORT}],
            "new_members": None,
        },
        "pg_version": int(PG_VERSION) * 10000,
        "start_lsn": start_lsn,
    }
    try:
        http_request("POST", f"http://{LISTEN_HOST}:{SAFEKEEPER_HTTP_PORT}/v1/tenant/timeline", body=sk_create)
        log("created timeline on safekeeper")
    except urllib.error.HTTPError as e:
        # 409/already-exists is fine on restart.
        if e.code not in (409,):
            log(f"safekeeper create_timeline returned {e.code}: {e.read()[:200]}")

    # 4. compute_ctl + postgres
    spec = render_compute_spec(RUN_DIR / "compute_spec.json")
    compute = Service(
        "compute_ctl",
        [
            BIN_DIR / "compute_ctl",
            "--pgdata", compute_pgdata,
            "-C", f"postgresql://cloud_admin@{LISTEN_HOST}:{COMPUTE_PORT}/postgres",
            "-b", PG_BIN / "postgres",
            "--compute-id", "yb-light-endpoint",
            "--config", spec,
            "--external-http-port", str(COMPUTE_CTL_HTTP_PORT),
            "--internal-http-port", str(COMPUTE_CTL_HTTP_PORT + 1),
            "--dev",
        ],
        cwd=RUN_DIR,
        health=lambda: tcp_open(LISTEN_HOST, COMPUTE_PORT),
    )
    sup.add(compute)
    compute.start()

    log("all services started; entering monitor loop")
    rc = sup.monitor()
    sys.exit(rc)


if __name__ == "__main__":
    main()
